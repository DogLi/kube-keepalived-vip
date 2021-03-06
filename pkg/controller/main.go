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

package controller

import (
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/golang/glog"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	k8sruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"

	utildbus "k8s.io/kubernetes/pkg/util/dbus"
	utilexec "k8s.io/kubernetes/pkg/util/exec"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"

	"github.com/dogli/kube-keepalived-vip/pkg/k8s"
	"github.com/dogli/kube-keepalived-vip/pkg/store"
	"github.com/dogli/kube-keepalived-vip/pkg/task"
	//"github.com/dogli/kube-keepalived-vip/utils"
	"fmt"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
)

const (
	resyncPeriod = 10 * time.Minute
)

type service struct {
	IP   string
	Port int
}

var configMapMutex sync.Mutex

type serviceByIPPort []service

func (c serviceByIPPort) Len() int      { return len(c) }
func (c serviceByIPPort) Swap(i, j int) { c[i], c[j] = c[j], c[i] }
func (c serviceByIPPort) Less(i, j int) bool {
	iIP := c[i].IP
	jIP := c[j].IP
	if iIP != jIP {
		return iIP < jIP
	}

	iPort := c[i].Port
	jPort := c[j].Port
	return iPort < jPort
}

type vip struct {
	Name      string
	VIP       string
	Port      int
	Protocol  string
	LVSMethod string
	Backends  []service
}

type vipByNameIPPort []vip

func (c vipByNameIPPort) Len() int      { return len(c) }
func (c vipByNameIPPort) Swap(i, j int) { c[i], c[j] = c[j], c[i] }
func (c vipByNameIPPort) Less(i, j int) bool {
	iName := c[i].Name
	jName := c[j].Name
	if iName != jName {
		return iName < jName
	}

	iIP := c[i].VIP
	jIP := c[j].VIP
	if iIP != jIP {
		return iIP < jIP
	}

	iPort := c[i].Port
	jPort := c[j].Port
	return iPort < jPort
}

// ipvsControllerController watches the kubernetes api and adds/removes
// services from LVS throgh ipvsadmin.
type ipvsControllerController struct {
	client *kubernetes.Clientset

	epController  cache.Controller
	mapController cache.Controller
	// control the added configmap, only one kube-keepalive-vip can use it with leader election
	mapAddController cache.Controller
	svcController    cache.Controller

	svcLister store.ServiceLister
	epLister  store.EndpointLister
	// indexer used to get the configmap
	indexer cache.Indexer
	// indexer used to get the added configmap
	indexerAdd cache.Indexer

	reloadRateLimiter flowcontrol.RateLimiter
	keepalived        *keepalived

	ruMD5 string

	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock sync.Mutex

	shutdown bool

	endpointSyncQueue  *task.Queue
	configmapSyncQueue *task.Queue
	configmapAddQueue  *task.Queue

	stopCh chan struct{}
}

// NewIPVSController creates a new controller from the given config.
func NewIPVSController(kubeClient *kubernetes.Clientset, useUnicast bool, configmapLabel string, vrid int, proxyMode bool) (*ipvsControllerController, error) {
	ipvsc := ipvsControllerController{
		client:            kubeClient,
		reloadRateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.5, 1),
		stopCh:            make(chan struct{}),
	}
	namespace := apiv1.NamespaceAll
	podInfo, err := k8s.GetPodDetails(kubeClient)
	if err != nil {
		glog.Fatalf("Error getting POD information: %v", err)
	}

	pod, err := kubeClient.CoreV1().Pods(podInfo.Namespace).Get(podInfo.Name, metav1.GetOptions{})

	if err != nil {
		glog.Fatalf("Error getting %v: %v", podInfo.Name, err)
	}

	selector := parseNodeSelector(pod.Spec.NodeSelector)
	clusterNodesIP := getClusterNodesIP(kubeClient, selector)

	nodeNetInfo, err := getNodeNetworkInfo(podInfo.NodeIP)
	if err != nil {
		glog.Fatalf("Error getting local IP from nodes in the cluster: %v", err)
	}

	neighbors := getNodeNeighbors(nodeNetInfo, clusterNodesIP)

	execer := utilexec.New()
	dbus := utildbus.New()
	iptInterface := utiliptables.New(execer, dbus, utiliptables.ProtocolIpv4)

	ipvsc.keepalived = &keepalived{
		iface:      nodeNetInfo.iface,
		ip:         nodeNetInfo.ip,
		netmask:    nodeNetInfo.netmask,
		nodes:      clusterNodesIP,
		neighbors:  neighbors,
		priority:   getNodePriority(nodeNetInfo.ip, clusterNodesIP),
		useUnicast: useUnicast,
		ipt:        iptInterface,
		vrid:       vrid,
		proxyMode:  proxyMode,
		Services:   make(map[string]string),
	}

	ipvsc.configmapSyncQueue = task.NewTaskQueue(ipvsc.syncConfigmap)
	ipvsc.configmapAddQueue = task.NewTaskQueue(ipvsc.AddConfigmap)
	ipvsc.endpointSyncQueue = task.NewTaskQueue(ipvsc.sync)

	err = ipvsc.keepalived.loadTemplates()
	if err != nil {
		glog.Fatalf("Error loading templates: %v", err)
	}

	mapEventHandler := cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			glog.Info("delete configmap")
			ipvsc.configmapSyncQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				ipvsc.OnUpdateConfigmap(old, cur)
			}
		},
	}

	mapAddHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			glog.Info("add configmap")
			ipvsc.configmapAddQueue.Enqueue(obj)
		},
	}

	eventHandlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ipvsc.endpointSyncQueue.Enqueue(obj)
		},
		DeleteFunc: func(obj interface{}) {
			ipvsc.endpointSyncQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				ipvsc.endpointSyncQueue.Enqueue(cur)
			}
		},
	}

	ipvsc.svcLister.Store, ipvsc.svcController = cache.NewInformer(
		cache.NewListWatchFromClient(ipvsc.client.CoreV1().RESTClient(), "services", namespace, fields.Everything()),
		&apiv1.Service{}, resyncPeriod, cache.ResourceEventHandlerFuncs{})

	ipvsc.epLister.Store, ipvsc.epController = cache.NewInformer(
		cache.NewListWatchFromClient(ipvsc.client.CoreV1().RESTClient(), "endpoints", namespace, fields.Everything()),
		&apiv1.Endpoints{}, resyncPeriod, eventHandlers)

	ipvsc.indexer, ipvsc.mapController = cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc:  configMapListFunc(kubeClient, namespace, configmapLabel),
			WatchFunc: configMapWatchFunc(kubeClient, namespace, configmapLabel),
		},
		&apiv1.ConfigMap{}, resyncPeriod, mapEventHandler, cache.Indexers{})

	ipvsc.indexerAdd, ipvsc.mapAddController = cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc:  configMapListFunc(kubeClient, namespace, configmapLabel),
			WatchFunc: configMapWatchFunc(kubeClient, namespace, configmapLabel),
		},
		&apiv1.ConfigMap{}, resyncPeriod, mapAddHandler, cache.Indexers{})

	return &ipvsc, nil
}

func configMapListFunc(c *kubernetes.Clientset, ns string, labelValue string) func(metav1.ListOptions) (runtime.Object, error) {
	return func(options metav1.ListOptions) (runtime.Object, error) {
		options.LabelSelector = labels.Set{"loadbalancer": labelValue}.AsSelector().String()
		return c.CoreV1().ConfigMaps(ns).List(options)
	}
}

func configMapWatchFunc(c *kubernetes.Clientset, ns string, labelValue string) func(options metav1.ListOptions) (watch.Interface, error) {
	return func(options metav1.ListOptions) (watch.Interface, error) {
		options.LabelSelector = labels.Set{"loadbalancer": labelValue}.AsSelector().String()
		return c.CoreV1().ConfigMaps(ns).Watch(options)
	}
}

// start the loadbalancer controller.
func (ipvsc *ipvsControllerController) Start() {
	go ipvsc.epController.Run(ipvsc.stopCh)
	go ipvsc.svcController.Run(ipvsc.stopCh)
	go ipvsc.endpointSyncQueue.Run(time.Second, ipvsc.stopCh)

	go ipvsc.mapController.Run(ipvsc.stopCh)
	go ipvsc.configmapSyncQueue.Run(time.Second, ipvsc.stopCh)

	go handleSigterm(ipvsc)


	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(ipvsc.stopCh,
		ipvsc.epController.HasSynced,
		ipvsc.svcController.HasSynced,
		ipvsc.mapController.HasSynced,
	) {
		k8sruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
	}

	glog.Info("starting keepalived to announce VIPs")
	err := ipvsc.freshKeepalivedConf()
	if err == nil {
		go ipvsc.keepalived.Start()
	}

	// leader election for the configmap add event
	run := func(stop <-chan struct{}) {
		go ipvsc.mapAddController.Run(ipvsc.stopCh)
		go ipvsc.configmapAddQueue.Run(time.Second, ipvsc.stopCh)
		glog.Infof("leader election success!")
	}

	podName := os.Getenv("POD_NAME")
	namespace := os.Getenv("POD_NAMESPACE")
	rl, err := resourcelock.New(
		resourcelock.EndpointsResourceLock,
		namespace,
		"kube-keepalived-vip",
		ipvsc.client.CoreV1(),
		resourcelock.ResourceLockConfig{
			Identity:      podName,
			EventRecorder: createRecorder(ipvsc.client, podName, namespace),
		},
	)

	leaderelection.RunOrDie(leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				glog.Fatalf("leader election lost!")
			},
		},
	})
}

// Stop stops the loadbalancer controller.
func (ipvsc *ipvsControllerController) Stop() error {
	ipvsc.stopLock.Lock()
	defer ipvsc.stopLock.Unlock()

	close(ipvsc.stopCh)
	if !ipvsc.endpointSyncQueue.IsShuttingDown() {
		glog.Infof("shutting down controller sync queues")
		go ipvsc.endpointSyncQueue.Shutdown()

	}

	if !ipvsc.configmapSyncQueue.IsShuttingDown() {
		glog.Infof("shutting down controller configmap sysnc queues")
		go ipvsc.configmapSyncQueue.Shutdown()
	}
	err := ipvsc.keepalived.Stop()
	return err
}


func createRecorder(kubecli kubernetes.Interface, name, namespace string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: corev1.New(kubecli.Core().RESTClient()).Events(namespace)})
	return eventBroadcaster.NewRecorder(scheme.Scheme, apiv1.EventSource{Component: name})
}
