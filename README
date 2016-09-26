Once you clone the repository, do 
```
go build .
```
To run the binary

```
#./iscsi-provisioner -out-of-cluster=true -kubeconfig=./config -provisioner-name=iscsi-provisioner -execmode=script -scriptpath=./prov.sh
```

#### Storage class configuration

```
[root@dhcp35-111 cluster]# cat ../class.yaml
kind: StorageClass
apiVersion: storage.k8s.io/v1beta1
metadata:
  name: hchiramm
provisioner: iscsi-provisioner
```

To request a claim, we need a claim file as shown below. The `volume.beta.kubernetes.io/storage-class: "hchiramm"` attach this claim to above defined storage class in name of "hchiramm".

```
[root@dhcp35-111 cluster]# cat ../claim.yaml 
kind: PersistentVolumeClaim
apiVersion: v1
metadata:
  name: iscsivolume
  annotations:
    volume.beta.kubernetes.io/storage-class: "hchiramm"
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: 1Mi
```

Lets create a storage class and a claim:

```
[root@dhcp35-111 cluster]# ./kubectl.sh create -f ../class.yaml 
storageclass "hchiramm" created
[root@dhcp35-111 cluster]# ./kubectl.sh create -f ../claim.yaml 
persistentvolumeclaim "iscsivolume" created
```

As soon as the claim is created in name of "iscsivolume", you can see a new PVC and PV got created!!! 


```
[root@dhcp35-111 cluster]# ./kubectl.sh get pvc
NAME          STATUS    VOLUME                                     CAPACITY   ACCESSMODES   AGE
iscsivolume   Bound     pvc-1cd896ec-8354-11e6-899f-54ee7551fd0c   1Mi        RWX           3s
[root@dhcp35-111 cluster]# ./kubectl.sh get pv
NAME                                       CAPACITY   ACCESSMODES   RECLAIMPOLICY   STATUS     CLAIM                  REASON    AGE
pvc-1cd896ec-8354-11e6-899f-54ee7551fd0c   1Mi        RWX           Delete          Bound      default/iscsivolume              20m
```

Awesome, the PVC is in BOUND status !! Lets list the property of this newly created PV called pvc-1cd896ec-8354-11e6-899f-54ee7551fd0c

```
[root@dhcp35-111 cluster]# ./kubectl.sh describe pv pvc-1cd896ec-8354-11e6-899f-54ee7551fd0c
Name:		pvc-1cd896ec-8354-11e6-899f-54ee7551fd0c
Labels:		<none>
Status:		Bound
Claim:		default/iscsivolume
Reclaim Policy:	Delete
Access Modes:	RWX
Capacity:	1Mi
Message:	
Source:
    Type:		ISCSI (an ISCSI Disk resource that is attached to a kubelet's host machine and then exposed to the pod)
    TargetPortal:	192.168.43.65
    IQN:		iqn.2016-12.example.server:storage.target00
    Lun:		0
    ISCSIInterface	default
    FSType:		ext3
    ReadOnly:		false
No events.
[root@dhcp35-111 cluster]# 
```

I am running my provisioner with options like `-execmode=script -scriptpath=./prov.sh` where you can use any script and provide its output to the provisioner binary. The only requirement is that the script/executable should return 2 values, First one is the HOSTNAME/IP of your iscsi target and Second is "The IQN".

You can script your dynamic iscsi volume creator and provide the output to the provisioner, and the provisioner will use these values. 
You can also run this provisioner in a container.

Reference # http://website-humblec.rhcloud.com/unpolished-external-iscsi-provisioner-dynamic-iscsi-persistent-volume-kubernetes/




