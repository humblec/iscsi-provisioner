package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
	"strings"
	"bytes"
	"github.com/golang/glog"
	"github.com/humblec/iscsi-provisioner/framework"
	"k8s.io/client-go/1.4/kubernetes"
	core_v1 "k8s.io/client-go/1.4/kubernetes/typed/core/v1"
	"k8s.io/client-go/1.4/pkg/api"
	"k8s.io/client-go/1.4/pkg/api/resource"
	"k8s.io/client-go/1.4/pkg/api/v1"
	"k8s.io/client-go/1.4/pkg/apis/storage/v1beta1"
	"k8s.io/client-go/1.4/pkg/runtime"
	"k8s.io/client-go/1.4/pkg/watch"
	"k8s.io/client-go/1.4/tools/cache"
	"k8s.io/client-go/1.4/tools/record"

	"k8s.io/kubernetes/pkg/util/goroutinemap"
)

// annClass annotation represents the storage class associated with a resource:
// - in PersistentVolumeClaim it represents required class to match.
//   Only PersistentVolumes with the same class (i.e. annotation with the same
//   value) can be bound to the claim. In case no such volume exists, the
//   controller will provision a new one using StorageClass instance with
//   the same name as the annotation value.
// - in PersistentVolume it represents storage class to which the persistent
//   volume belongs.
const annClass = "volume.beta.kubernetes.io/storage-class"

// This annotation is added to a PV that has been dynamically provisioned by
// Kubernetes. Its value is name of volume plugin that created the volume.
// It serves both user (to show where a PV comes from) and Kubernetes (to
// recognize dynamically provisioned PVs in its decisions).
const annDynamicallyProvisioned = "pv.kubernetes.io/provisioned-by"

// TODO upstream proposes PV controller will set this on all claims automatically
// https://github.com/kubernetes/kubernetes/pull/30285
const annStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"

// Number of retries when we create a PV object for a provisioned volume.
const createProvisionedPVRetryCount = 5

// Interval between retries when we create a PV object for a provisioned volume.
const createProvisionedPVInterval = 10 * time.Second

type iscsiController struct {
	client kubernetes.Interface

	// The name of the provisioner for which this controller dynamically
	// provisions volumes. The value of annDynamicallyProvisioned and
	// annStorageProvisioner to set & watch for, respectively
	provisionerName string
	provisionerConfig ProvisionerConfig

	claimSource      cache.ListerWatcher
	claimController  *framework.Controller
	volumeSource     cache.ListerWatcher
	volumeController *framework.Controller
	classSource      cache.ListerWatcher
	classReflector   *cache.Reflector

	volumes cache.Store
	claims  cache.Store
	classes cache.Store

	eventRecorder record.EventRecorder

	// Map of scheduled/running operations.
	runningOperations goroutinemap.GoRoutineMap

	createProvisionedPVRetryCount int
	createProvisionedPVInterval   time.Duration
}

func newiscsiController(
	client kubernetes.Interface,
	resyncPeriod time.Duration,
	provisionerName string,
	provisionerConfig ProvisionerConfig,
) *iscsiController {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&core_v1.EventSinkImpl{Interface: client.Core().Events(v1.NamespaceAll)})
	var eventRecorder record.EventRecorder
	out, err := exec.Command("hostname").Output()
	if err != nil {
		glog.Errorf("Error getting hostname for specifying it as source of events: %v", err)
		eventRecorder = broadcaster.NewRecorder(v1.EventSource{Component: "iscsi-provisioner"})
	} else {
		eventRecorder = broadcaster.NewRecorder(v1.EventSource{Component: fmt.Sprintf("iscsi-provisioner-%s", strings.TrimSpace(string(out)))})
	}

	controller := &iscsiController{
		client:                        client,
		provisionerName:               provisionerName,
		provisionerConfig: 				provisionerConfig,
		eventRecorder:                 eventRecorder,
		runningOperations:             goroutinemap.NewGoRoutineMap(false /* exponentialBackOffOnError */),
		createProvisionedPVRetryCount: createProvisionedPVRetryCount,
		createProvisionedPVInterval:   createProvisionedPVInterval,
	}

	controller.claimSource = &cache.ListWatch{
		ListFunc: func(options api.ListOptions) (runtime.Object, error) {
			return client.Core().PersistentVolumeClaims(v1.NamespaceAll).List(options)
		},
		WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
			return client.Core().PersistentVolumeClaims(v1.NamespaceAll).Watch(options)
		},
	}
	controller.claims, controller.claimController = framework.NewInformer(
		controller.claimSource,
		&v1.PersistentVolumeClaim{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    controller.addClaim,
			UpdateFunc: controller.updateClaim,
			DeleteFunc: nil,
		},
	)

	controller.volumeSource = &cache.ListWatch{
		ListFunc: func(options api.ListOptions) (runtime.Object, error) {
			return client.Core().PersistentVolumes().List(options)
		},
		WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
			return client.Core().PersistentVolumes().Watch(options)
		},
	}
	controller.volumes, controller.volumeController = framework.NewInformer(
		controller.volumeSource,
		&v1.PersistentVolume{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    nil,
			UpdateFunc: controller.updateVolume,
			DeleteFunc: nil,
		},
	)

	controller.classSource = &cache.ListWatch{
		ListFunc: func(options api.ListOptions) (runtime.Object, error) {
			return client.Storage().StorageClasses().List(options)
			//return client.Extensions().StorageClasses().List(options)
		},
		WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
			return client.Storage().StorageClasses().Watch(options)
			//return client.Extensions().StorageClasses().Watch(options)
		},
	}
	controller.classes = cache.NewStore(framework.DeletionHandlingMetaNamespaceKeyFunc)
	controller.classReflector = cache.NewReflector(
		controller.classSource,
		&v1beta1.StorageClass{},
		controller.classes,
		resyncPeriod,
	)

	return controller
}

func (ctrl *iscsiController) Run(stopCh <-chan struct{}) {
	glog.Info("Starting iscsi provisioner controller!")
	go ctrl.claimController.Run(stopCh)
	go ctrl.volumeController.Run(stopCh)
	go ctrl.classReflector.RunUntil(stopCh)
	<-stopCh
}

// On add claim, check if the added claim should have a volume provisioned for
// it and provision one if so.
func (ctrl *iscsiController) addClaim(obj interface{}) {
	claim, ok := obj.(*v1.PersistentVolumeClaim)
	if !ok {
		glog.Errorf("Expected PersistentVolumeClaim but addClaim received %+v", obj)
		return
	}

	if ctrl.shouldProvision(claim) {
		opName := fmt.Sprintf("provision-%s[%s]", claimToClaimKey(claim), string(claim.UID))
		ctrl.scheduleOperation(opName, func() error {
			ctrl.provisionClaimOperation(claim)
			return nil
		})
	}
}

// On update claim, pass the new claim to addClaim. Updates occur at least every
// resyncPeriod.
func (ctrl *iscsiController) updateClaim(oldObj, newObj interface{}) {
	ctrl.addClaim(newObj)
}

// On update volume, check if the updated volume should be deleted and delete if
// so. Updates occur at least every resyncPeriod.
func (ctrl *iscsiController) updateVolume(oldObj, newObj interface{}) {
	volume, ok := newObj.(*v1.PersistentVolume)
	if !ok {
		glog.Errorf("Expected PersistentVolume but handler received %#v", newObj)
		return
	}

	if ctrl.shouldDelete(volume) {
		opName := fmt.Sprintf("delete-%s[%s]", volume.Name, string(volume.UID))
		ctrl.scheduleOperation(opName, func() error {
			ctrl.deleteVolumeOperation(volume)
			return nil
		})
	}
}

func (ctrl *iscsiController) shouldProvision(claim *v1.PersistentVolumeClaim) bool {
	// TODO do this and remove all code below VolumeName check
	// https://github.com/kubernetes/kubernetes/pull/30285
	// if claim.Annotations[annStorageProvisioner] != provisionerName {
	// 	return false, nil
	// }

	if claim.Spec.VolumeName != "" {
		return false
	}

	claimClass := getClaimClass(claim)
	classObj, found, err := ctrl.classes.GetByKey(claimClass)
	if err != nil {
		glog.Errorf("Error getting StorageClass %q: %v", claimClass, err)
		return false
	}
	if !found {
		glog.Errorf("StorageClass %q not found", claimClass)
		return false
	}
	class, ok := classObj.(*v1beta1.StorageClass)
	if !ok {
		glog.Errorf("Cannot convert object to StorageClass: %+v", classObj)
		return false
	}

	if class.Provisioner != ctrl.provisionerName {
		return false
	}

	return true
}

func (ctrl *iscsiController) shouldDelete(volume *v1.PersistentVolume) bool {
	// TODO https://github.com/kubernetes/kubernetes/pull/32565 will not Fail PVs
	if volume.Status.Phase != v1.VolumeReleased && volume.Status.Phase != v1.VolumeFailed {
		return false
	}

	if hasAnnotation(volume.ObjectMeta, annDynamicallyProvisioned) {
		if ann := volume.Annotations[annDynamicallyProvisioned]; ann != ctrl.provisionerName {
			return false
		}
	}

	path := fmt.Sprintf("iscsi-volume-%s", volume.ObjectMeta.Name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}

	return true
}

func (ctrl *iscsiController) provisionClaimOperation(claim *v1.PersistentVolumeClaim) {
	// Most code here is identical to that found in controller.go of kube's PV controller...
	claimClass := getClaimClass(claim)
	glog.V(4).Infof("provisionClaimOperation [%s] started, class: %q", claimToClaimKey(claim), claimClass)

	//  A previous doProvisionClaim may just have finished while we were waiting for
	//  the locks. Check that PV (with deterministic name) hasn't been provisioned
	//  yet.
	pvName := ctrl.getProvisionedVolumeNameForClaim(claim)
	volume, err := ctrl.client.Core().PersistentVolumes().Get(pvName)
	if err == nil && volume != nil {
		// Volume has been already provisioned, nothing to do.
		glog.V(4).Infof("provisionClaimOperation [%s]: volume already exists, skipping", claimToClaimKey(claim))
		return
	}

	// Prepare a claimRef to the claim early (to fail before a volume is
	// provisioned)
	claimRef, err := v1.GetReference(claim)
	if err != nil {
		glog.Errorf("unexpected error getting claim reference: %v", err)
		return
	}

	classObj, found, err := ctrl.classes.GetByKey(claimClass)
	if err != nil {
		glog.Errorf("Error getting StorageClass %q: %v", claimClass, err)
		return
	}
	if !found {
		glog.Errorf("StorageClass %q not found", claimClass)
		return
	}
	storageClass, ok := classObj.(*v1beta1.StorageClass)
	if !ok {
		glog.Errorf("Cannot convert object to StorageClass: %+v", classObj)
		return
	}

	options := VolumeOptions{
		Capacity:                      claim.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
		AccessModes:                   claim.Spec.AccessModes,
		PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimDelete,
		PVName:     pvName,
		Parameters: storageClass.Parameters,
	}

	volume, err = ctrl.provision(options)
	if err != nil {
		strerr := fmt.Sprintf("Failed to provision volume with StorageClass %q: %v", storageClass.Name, err)
		glog.Errorf("Failed to provision volume for claim %q with StorageClass %q: %v", claimToClaimKey(claim), claim.Name, err)
		ctrl.eventRecorder.Event(claim, v1.EventTypeWarning, "ProvisioningFailed", strerr)
		return
	}

	glog.V(3).Infof("volume %q for claim %q created", volume.Name, claimToClaimKey(claim))

	// Set ClaimRef and the PV controller will bind and set annBoundByController for us
	volume.Spec.ClaimRef = claimRef

	setAnnotation(&volume.ObjectMeta, annDynamicallyProvisioned, ctrl.provisionerName)
	setAnnotation(&volume.ObjectMeta, annClass, claimClass)

	// Try to create the PV object several times
	for i := 0; i < ctrl.createProvisionedPVRetryCount; i++ {
		glog.V(4).Infof("provisionClaimOperation [%s]: trying to save volume %s", claimToClaimKey(claim), volume.Name)
		if _, err = ctrl.client.Core().PersistentVolumes().Create(volume); err == nil {
			// Save succeeded.
			glog.V(3).Infof("volume %q for claim %q saved", volume.Name, claimToClaimKey(claim))
			break
		}
		// Save failed, try again after a while.
		glog.V(3).Infof("failed to save volume %q for claim %q: %v", volume.Name, claimToClaimKey(claim), err)
		time.Sleep(ctrl.createProvisionedPVInterval)
	}

	if err != nil {
		// Save failed. Now we have a storage asset outside of Kubernetes,
		// but we don't have appropriate PV object for it.
		// Emit some event here and try to delete the storage asset several
		// times.
		strerr := fmt.Sprintf("Error creating provisioned PV object for claim %s: %v. Deleting the volume.", claimToClaimKey(claim), err)
		glog.V(3).Info(strerr)
		ctrl.eventRecorder.Event(claim, v1.EventTypeWarning, "ProvisioningFailed", strerr)

		for i := 0; i < ctrl.createProvisionedPVRetryCount; i++ {
			if err = ctrl.delete(volume); err == nil {
				// Delete succeeded
				glog.V(4).Infof("provisionClaimOperation [%s]: cleaning volume %s succeeded", claimToClaimKey(claim), volume.Name)
				break
			}
			// Delete failed, try again after a while.
			glog.V(3).Infof("failed to delete volume %q: %v", volume.Name, err)
			time.Sleep(ctrl.createProvisionedPVInterval)
		}

		if err != nil {
			// Delete failed several times. There is an orphaned volume and there
			// is nothing we can do about it.
			strerr := fmt.Sprintf("Error cleaning provisioned volume for claim %s: %v. Please delete manually.", claimToClaimKey(claim), err)
			glog.V(2).Info(strerr)
			ctrl.eventRecorder.Event(claim, v1.EventTypeWarning, "ProvisioningCleanupFailed", strerr)
		}
	} else {
		glog.V(2).Infof("volume %q provisioned for claim %q", volume.Name, claimToClaimKey(claim))
	}
}

// VolumeOptions contains option information about a volume
type VolumeOptions struct {
	// Capacity is the size of a volume.
	Capacity resource.Quantity
	// AccessModes of a volume
	AccessModes []v1.PersistentVolumeAccessMode
	// Reclamation policy for a persistent volume
	PersistentVolumeReclaimPolicy v1.PersistentVolumeReclaimPolicy
	// PV.Name of the appropriate PersistentVolume. Used to generate cloud
	// volume name.
	PVName string
	// Volume provisioning parameters from StorageClass
	Parameters map[string]string
}


// provision creates a volume i.e. the storage asset and returns a PV object for
// the volume
func (ctrl *iscsiController) provision(options VolumeOptions) (*v1.PersistentVolume, error) {
	server, path, err := ctrl.createVolume(options.PVName)
	if err != nil {
		return nil, err
	}
	glog.V(1).Infof("Server and path returned :%v %v", server, path)
	pv := &v1.PersistentVolume{
		ObjectMeta: v1.ObjectMeta{
			Name:   options.PVName,
			Labels: map[string]string{},
			Annotations: map[string]string{
				"kubernetes.io/createdby": "iscsi-dynamic-provisioner",
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: options.PersistentVolumeReclaimPolicy,
			AccessModes:                   options.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.Capacity,
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				ISCSI: &v1.ISCSIVolumeSource{
					TargetPortal: server,
					IQN: path,
     				Lun: 0,
     				FSType: "ext3",
     			    ReadOnly: false,
					
				},
			},
		},
	}

	return pv, nil
}

// createVolume creates a volume i.e. the storage asset.


func (ctrl *iscsiController) createVolume(PVName string) (string, string, error) {
	var server,path string
	if ctrl.provisionerConfig.Opmode == "script" {
			cmd := exec.Command("sh", ctrl.provisionerConfig.Scriptpath)
			var out bytes.Buffer
			cmd.Stdout = &out
			err := cmd.Run()
			if err != nil {
					glog.Errorf("%v", err)
			}
			result := strings.Fields(out.String())
			server = result[0]
			path = result[1]
	}
	return server, path, nil
}

func (ctrl *iscsiController) deleteVolumeOperation(volume *v1.PersistentVolume) {
	glog.V(4).Infof("deleteVolumeOperation [%s] started", volume.Name)

	// This method may have been waiting for a volume lock for some time.
	// Our check does not have to be as sophisticated as PV controller's, we can
	// trust that the PV controller has set the PV to Released/Failed and it's
	// ours to delete
	newVolume, err := ctrl.client.Core().PersistentVolumes().Get(volume.Name)
	if err != nil {
		glog.V(3).Infof("error reading peristent volume %q: %v", volume.Name, err)
		return
	}
	if !ctrl.shouldDelete(newVolume) {
		glog.V(3).Infof("volume %q no longer needs deletion, skipping", volume.Name)
		return
	}

	if err := ctrl.delete(volume); err != nil {
		// Delete failed, emit an event.
		glog.V(3).Infof("deletion of volume %q failed: %v", volume.Name, err)
		ctrl.eventRecorder.Event(volume, v1.EventTypeWarning, "VolumeFailedDelete", err.Error())
		return
	}

	glog.V(4).Infof("deleteVolumeOperation [%s]: success", volume.Name)
	// Delete the volume
	if err = ctrl.client.Core().PersistentVolumes().Delete(volume.Name, nil); err != nil {
		// Oops, could not delete the volume and therefore the controller will
		// try to delete the volume again on next update.
		glog.V(3).Infof("failed to delete volume %q from database: %v", volume.Name, err)
		return
	}

	return
}

// delete removes the directory backing the given PV that was created by
// createVolume.
func (ctrl *iscsiController) delete(volume *v1.PersistentVolume) error {
	// TODO quota, something better than just directories
	return nil
}

// scheduleOperation starts given asynchronous operation on given volume. It
// makes sure the operation is already not running.
func (ctrl *iscsiController) scheduleOperation(operationName string, operation func() error) {
	glog.V(4).Infof("scheduleOperation[%s]", operationName)

	err := ctrl.runningOperations.Run(operationName, operation)
	if err != nil {
		if goroutinemap.IsAlreadyExists(err) {
			glog.V(4).Infof("operation %q is already running, skipping", operationName)
		} else {
			glog.Errorf("error scheduling operaion %q: %v", operationName, err)
		}
	}
}

func hasAnnotation(obj v1.ObjectMeta, ann string) bool {
	_, found := obj.Annotations[ann]
	return found
}

func setAnnotation(obj *v1.ObjectMeta, ann string, value string) {
	if obj.Annotations == nil {
		obj.Annotations = make(map[string]string)
	}
	obj.Annotations[ann] = value
}

// getClaimClass returns name of class that is requested by given claim.
// Request for `nil` class is interpreted as request for class "",
// i.e. for a classless PV.
// controller_base.go
func getClaimClass(claim *v1.PersistentVolumeClaim) string {
	// TODO: change to PersistentVolumeClaim.Spec.Class value when this
	// attribute is introduced.
	if class, found := claim.Annotations[annClass]; found {
		return class
	}

	return ""
}

// getProvisionedVolumeNameForClaim returns PV.Name for the provisioned volume.
// The name must be unique.
func (ctrl *iscsiController) getProvisionedVolumeNameForClaim(claim *v1.PersistentVolumeClaim) string {
	return "pvc-" + string(claim.UID)
}

func claimToClaimKey(claim *v1.PersistentVolumeClaim) string {
	return fmt.Sprintf("%s/%s", claim.Namespace, claim.Name)
}

func claimrefToClaimKey(claimref *v1.ObjectReference) string {
	return fmt.Sprintf("%s/%s", claimref.Namespace, claimref.Name)
}
