/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/golang/glog"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/kubernetes/pkg/util/sysctl"
	k8sexec "k8s.io/utils/exec"

	"github.com/aledbf/kube-keepalived-vip/pkg/controller"
)

var (
	// sysctl changes required by keepalived
	sysctlAdjustments = map[string]int{
		// allows processes to bind() to non-local IP addresses
		"net/ipv4/ip_nonlocal_bind": 1,
		// enable connection tracking for LVS connections
		"net/ipv4/vs/conntrack": 1,
	}
	apiserverHost  string
	kubeConfigFile string
	useUnicast     bool
	configmapLabel string
	proxyMode      bool
	vrid           int
	showVersion    bool

	_version_   = ""
	_branch_    = ""
	_commitId_  = ""
	_buildTime_ = ""
)

func init() {
	flag.StringVar(&apiserverHost, "apiserver-host", "", "The address of the Kubernetes Apiserver "+
		"to connect to in the format of protocol://address:port, e.g., "+
		"http://localhost:8080. If not specified, the assumption is that the binary runs inside a "+
		"Kubernetes cluster and local discovery is attempted.")

	flag.StringVar(&kubeConfigFile, "kubeconfig", "", "Path to kubeconfig file with authorization and master location information.")

	flag.BoolVar(&useUnicast, "use-unicast", false, `use unicast instead of multicast for communication
		with other keepalived instances`)

	flag.BoolVar(&proxyMode, "proxy-protocol-mode", false, `If true, it will use keepalived to announce the virtual
		IP address/es and HAProxy with proxy protocol to forward traffic to the endpoints.
		Please check http://blog.haproxy.com/haproxy/proxy-protocol
		Be sure that both endpoints of the connection support proxy protocol.
		`)
	flag.IntVar(&vrid, "vrid", 50,
		`The keepalived VRID (Virtual Router Identifier, between 0 and 255 as per
      RFC-5798), which must be different for every Virtual Router (ie. every
      keepalived sets) running on the same network.`)

	flag.StringVar(&configmapLabel, "loadbalancer", "zstack", "watch the configmap with label loadbalancer: balancer-platform")

	flag.BoolVar(&showVersion, "show-version", false,
		`show the version`)

	flag.Parse()
	flag.Set("logtostderr", "true")
}

func main() {

	if showVersion {
		glog.Infof("Version: %s\nBranch: %s\nCommit id: %s\nBuild time: %s\n",
			_version_, _branch_, _commitId_, _buildTime_)
		return
	}

	if vrid < 0 || vrid > 255 {
		glog.Fatalf("Error using VRID %d, only values between 0 and 255 are allowed.", vrid)
	}

	if useUnicast {
		glog.Info("keepalived will use unicast to sync the nodes")
	}

	err := loadIPVModule()
	if err != nil {
		glog.Fatalf("unexpected error: %v", err)
	}

	err = changeSysctl()
	if err != nil {
		glog.Fatalf("unexpected error: %v", err)
	}

	err = resetIPVS()
	if err != nil {
		glog.Fatalf("unexpected error: %v", err)
	}

	if proxyMode {
		copyHaproxyCfg()
	}

	kubeClient, err := createApiserverClient(apiserverHost, kubeConfigFile)
	if err != nil {
		handleFatalInitError(err)
	}

	glog.Info("starting LVS configuration")

	ipvsc, err := controller.NewIPVSController(kubeClient, useUnicast, configmapLabel, vrid, proxyMode)

	if err != nil {
		glog.Fatalf("failed to initial controller: %v", err)
		handleFatalInitError(err)
	}

	ipvsc.Start()
}


const (
	// High enough QPS to fit all expected use cases. QPS=0 is not set here, because
	// client code is overriding it.
	defaultQPS = 1e6
	// High enough Burst to fit all expected use cases. Burst=0 is not set here, because
	// client code is overriding it.
	defaultBurst = 1e6
)

// buildConfigFromFlags builds REST config based on master URL and kubeconfig path.
// If both of them are empty then in cluster config is used.
func buildConfigFromFlags(masterURL, kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" && masterURL == "" {
		kubeconfig, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}

		return kubeconfig, nil
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{
			ClusterInfo: clientcmdapi.Cluster{
				Server: masterURL,
			},
		}).ClientConfig()
}

// createApiserverClient creates new Kubernetes Apiserver client. When kubeconfig or apiserverHost param is empty
// the function assumes that it is running inside a Kubernetes cluster and attempts to
// discover the Apiserver. Otherwise, it connects to the Apiserver specified.
//
// apiserverHost param is in the format of protocol://address:port/pathPrefix, e.g.http://localhost:8001.
// kubeConfig location of kubeconfig file
func createApiserverClient(apiserverHost string, kubeConfig string) (*kubernetes.Clientset, error) {
	cfg, err := buildConfigFromFlags(apiserverHost, kubeConfig)
	if err != nil {
		return nil, err
	}

	cfg.QPS = defaultQPS
	cfg.Burst = defaultBurst
	cfg.ContentType = "application/vnd.kubernetes.protobuf"

	glog.Infof("Creating API server client for %s", cfg.Host)

	client, err := kubernetes.NewForConfig(cfg)

	if err != nil {
		return nil, err
	}
	return client, nil
}

/**
 * Handles fatal init error that prevents server from doing any work. Prints verbose error
 * message and quits the server.
 */
func handleFatalInitError(err error) {
	glog.Fatalf("Error while initializing connection to Kubernetes apiserver. "+
		"This most likely means that the cluster is misconfigured (e.g., it has "+
		"invalid apiserver certificates or service accounts configuration). Reason: %s\n", err)
	panic(err)
}

// loadIPVModule load module require to use keepalived
func loadIPVModule() error {
	out, err := k8sexec.New().Command("modprobe", "ip_vs").CombinedOutput()
	if err != nil {
		glog.V(2).Infof("Error loading ip_vip: %s, %v", string(out), err)
		return err
	}

	_, err = os.Stat("/proc/net/ip_vs")
	return err
}

// changeSysctl changes the required network setting in /proc to get
// keepalived working in the local system.
func changeSysctl() error {
	sys := sysctl.New()
	for k, v := range sysctlAdjustments {
		if err := sys.SetSysctl(k, v); err != nil {
			return err
		}
	}

	return nil
}

func resetIPVS() error {
	glog.Info("cleaning ipvs configuration")
	_, err := k8sexec.New().Command("ipvsadm", "-C").CombinedOutput()
	if err != nil {
		return fmt.Errorf("error removing ipvs configuration: %v", err)
	}

	return nil
}

// copyHaproxyCfg copies the default haproxy configuration file
// to the mounted directory (the mount overwrites the default file)
func copyHaproxyCfg() {
	data, err := ioutil.ReadFile("/haproxy.cfg")
	if err != nil {
		glog.Fatalf("unexpected error reading haproxy.cfg: %v", err)
	}
	err = ioutil.WriteFile("/etc/haproxy/haproxy.cfg", data, 0644)
	if err != nil {
		glog.Fatalf("unexpected error writing haproxy.cfg: %v", err)
	}
}
