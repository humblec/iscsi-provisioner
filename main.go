package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang/glog"

	"k8s.io/client-go/1.4/kubernetes"
	"k8s.io/client-go/1.4/pkg/util/wait"
	"k8s.io/client-go/1.4/rest"
	"k8s.io/client-go/1.4/tools/clientcmd"
)

var (
	provisionerName = flag.String("provisioner-name", "iscsi-provisioner", "The name of this provisioner, i.e. the value `StorageClasses` will set for their `provisioner`.")
	execMode 		= flag.String("execmode", "script", "[script/restapi..etc]")
	scriptPath 		= flag.String("scriptpath", "path", "[--path=./prov.sh]")
	outOfCluster 	= flag.Bool("out-of-cluster", false, "If the provisioner is being run out of cluster. Set the master or kubeconfig flag accordingly if true. Default false.")
	master       	= flag.String("master", "", "Master URL to build a client config from. Either this or kubeconfig needs to be set if the provisioner is being run out of cluster.")
	kubeconfig 		= flag.String("kubeconfig", "", "Absolute path to the kubeconfig file. Either this or master needs to be set if the provisioner is being run out of cluster.")
)




type ProvisionerConfig struct {
	Opmode string  // Operation Mode
	Scriptpath string // Path of script
	Resturl string // Url of rest server
	Restuser string // rest user
	Restkey string // password of above use
}

func main() {
	flag.Set("logtostderr", "true")
	flag.Parse()
	var provisionerConfig ProvisionerConfig
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		os.Exit(1)
	}()
	if *execMode != "" {
		switch *execMode {
			case "script":
					glog.V(1).Infof("Provision a volume by executing the script")
					provisionerConfig.Opmode = "script"
					if *execMode == "script" && *scriptPath == "" {
						glog.Errorf("scriptpath is nil, exiting.")
					} else {
						provisionerConfig.Scriptpath = *scriptPath
					}
			case "restapi":
					glog.V(1).Infof("Contact REST server and provision volume")
					provisionerConfig.Opmode = "restapi"
					provisionerConfig.Resturl = "http://localhost:8081"
					provisionerConfig.Restuser = "admin"
					provisionerConfig.Restkey = "password"
			default:
					glog.Errorf("Unknown option for execmode")
		}

	}
	glog.Errorf("Provisioner Config :%#v", provisionerConfig)
	
		var config *rest.Config
	var err error
	if *outOfCluster {
		config, err = clientcmd.BuildConfigFromFlags(*master, *kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		glog.Errorf("Failed to create config: %v", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Errorf("Failed to create client: %v", err)
			os.Exit(1)
}
	glusterc := newiscsiController(clientset, 15*time.Second, *provisionerName, provisionerConfig)
	glusterc.Run(wait.NeverStop)
}