package controller

import (
	"fmt"
	"io"
	"os"
	"crypto/md5"
	"encoding/hex"
	"os/signal"
	"syscall"
	"github.com/golang/glog"
	"sort"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)


func (ipvsc *ipvsControllerController) getConfigMap(ns, name string) (*apiv1.ConfigMap, error) {
	configmap, err := ipvsc.client.ConfigMaps(ns).Get(name, metav1.GetOptions{})
	if err != nil{
		return nil, err
	}
	return configmap, nil
}

func (ipvsc *ipvsControllerController) getConfigMaps() []*apiv1.ConfigMap {
	objs := ipvsc.indexer.List()
	configmaps := []*apiv1.ConfigMap{}
	for _, obj := range objs {
		configmaps = append(configmaps, obj.(*apiv1.ConfigMap))
	}
	return configmaps
}

func checksum(filename string) (string, error) {
	var result []byte
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := md5.New()
	_, err = io.Copy(hash, file)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(result)), nil
}

func handleSigterm(ipvsc *ipvsControllerController) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM)
	<-signalChan
	glog.Infof("Received SIGTERM, shutting down")

	exitCode := 0
	if err := ipvsc.Stop(); err != nil {
		glog.Infof("Error during shutdown %v", err)
		exitCode = 1
	}

	glog.Infof("Exiting with %v", exitCode)
	os.Exit(exitCode)
}

// getEndpoints returns a list of <endpoint ip>:<port> for a given service/target port combination.
func (ipvsc *ipvsControllerController) getEndpoints(
	s *apiv1.Service, servicePort *apiv1.ServicePort) []service {
	ep, err := ipvsc.epLister.GetServiceEndpoints(s)
	if err != nil {
		glog.Warningf("unexpected error getting service endpoints: %v", err)
		return []service{}
	}

	var endpoints []service

	// The intent here is to create a union of all subsets that match a targetPort.
	// We know the endpoint already matches the service, so all pod ips that have
	// the target port are capable of service traffic for it.
	for _, ss := range ep.Subsets {
		for _, epPort := range ss.Ports {
			var targetPort int
			switch servicePort.TargetPort.Type {
			case intstr.Int:
				if int(epPort.Port) == servicePort.TargetPort.IntValue() {
					targetPort = int(epPort.Port)
				}
			case intstr.String:
				if epPort.Name == servicePort.TargetPort.StrVal {
					targetPort = int(epPort.Port)
				}
			}
			if targetPort == 0 {
				continue
			}
			for _, epAddress := range ss.Addresses {
				endpoints = append(endpoints, service{IP: epAddress.IP, Port: targetPort})
			}
		}
	}

	return endpoints
}

// getServices returns a list of services and their endpoints.
func (ipvsc *ipvsControllerController) getService(cm *apiv1.ConfigMap) ([]vip, error) {
	svcs := []vip{}

	cmData := cm.Data
	bind_ip := cmData["bind_ip"]
	svc := cmData["target_svc"]
	ns, ok := cmData["target_namespace"]
	if !ok {
		ns = "default"
		cm.Data["target_namespace"] = ns
	}
	kind, ok := cmData["kind"] // ["NAT", "DR", "PROXY"]
	if !ok {
		kind = "NAT"
		cm.Data["kind"] = "NAT"
	}

	nsSvc := fmt.Sprintf("%v/%v", ns, svc)
	svcObj, svcExists, err := ipvsc.svcLister.Store.GetByKey(nsSvc)
	if err != nil {
		glog.Warningf("error getting service %v: %v", nsSvc, err)
		return nil, err
	}

	if !svcExists {
		glog.Warningf("service %v/%v not found", ns, svc)
		return nil, fmt.Errorf("service %v not found")
	}

	s := svcObj.(*apiv1.Service)
	for _, servicePort := range s.Spec.Ports {
		ep := ipvsc.getEndpoints(s, &servicePort)
		if len(ep) == 0 {
			glog.Warningf("no endpoints found for service %v, port %+v", s.Name, servicePort)
			continue
		}

		sort.Sort(serviceByIPPort(ep))

		svcs = append(svcs, vip{
			Name:      fmt.Sprintf("%v-%v", s.Namespace, s.Name),
			IP:        bind_ip,
			Port:      int(servicePort.Port),
			LVSMethod: kind,
			Backends:  ep,
			Protocol:  fmt.Sprintf("%v", servicePort.Protocol),
		})
		glog.V(2).Infof("found service: %v:%v", s.Name, servicePort.Port)
	}
	sort.Sort(vipByNameIPPort(svcs))
	return svcs, nil
}

func (ipvsc *ipvsControllerController) getServices() []vip {
	configmaps := ipvsc.getConfigMaps()
	svcs := []vip{}
	for _, configmap := range configmaps {
		svc, err := ipvsc.getService(configmap)
		if err != nil {
			glog.Warningf("can not get service info from configmap %s", configmap.Name)
			continue
		}
		svcs = append(svcs, svc...)
	}
	sort.Sort(vipByNameIPPort(svcs))
	return svcs
}

func (ipvsc *ipvsControllerController) freshKeepalivedConf() error {
	// get all svcs and restart keepalived
	ipvsc.keepalived.VIPs = ipvsc.getServices()
	err := ipvsc.keepalived.WriteCfg()
	return err
}

func acquire_vip() (string, error) {
	// TODO: get vip from zstack
	return "10.10.40.45", nil
}

func restore_vip(vip string) error {
	// TODO: return vip to zstack
	return nil
}

func (ipvsc *ipvsControllerController) reload() error {
	md5, err := checksum(keepalivedCfg)
	if err == nil && md5 == ipvsc.ruMD5 {
		glog.Infof("get same MD5:  %s", ipvsc.ruMD5)
		return nil
	}

	ipvsc.ruMD5 = md5
	err = ipvsc.keepalived.Reload()
	return err
}

func (ipvsc *ipvsControllerController) OnSyncConfigmap(cfm *apiv1.ConfigMap) error {
	cfm = cfm.DeepCopy()
	cmData := cfm.Data
	svc := cmData["target_service"]
	_, ok := cmData["bind_ip"]
	if !ok {
		bindIp, err := acquire_vip()
		if err != nil {
			glog.Errorf("acquire vip failed for service %s", svc)
			return fmt.Errorf("error when acquire vip: %s", err)
		}
		cfm.Data["bind_ip"] = bindIp
		_, err = ipvsc.client.ConfigMaps(cfm.GetObjectMeta().GetNamespace()).Update(cfm)
		glog.Errorf("updata configmap failed: %s", err)
		return err
	}

	// reload
	bindIP := cfm.Data["bind_ip"]
	VIPs, err := ipvsc.getService(cfm)
	err = ipvsc.keepalived.AddVIPs(bindIP, VIPs)
	if err != nil {
		glog.Errorf("error reloading keepalived: %v", err)
	}
	glog.Infof("reload keepalived!")
	err = ipvsc.reload()
	return err
}

func (ipvsc *ipvsControllerController) OnDeleteConfigmap(cfm *apiv1.ConfigMap) error {
	cmData := cfm.Data
	vip, ok := cmData["bind_ip"]
	if ok {
		err := ipvsc.keepalived.DeleteVIP(vip)
		if err != nil {
			return err
		}
		err = restore_vip(vip)
		if err != nil {
			glog.Errorf("error to restore vip: %s", vip)
		}
		err = ipvsc.reload()
		return err
	}
	return nil
}

func (ipvsc *ipvsControllerController) sync(obj interface{}) error {
	//glog.Infof("SSSSSSSSSSSSSS: %s", key)
	return nil
	//err := ipvsc.freshKeepalivedConf()
	//if err == nil {
	//	err = ipvsc.reload()
	//}
	//return err
}

func (ipvsc *ipvsControllerController) syncConfigmap(oldobj interface{}) error {

	ipvsc.reloadRateLimiter.Accept()

	oldCM := oldobj.(*apiv1.ConfigMap)
	cmName := oldCM.GetObjectMeta().GetName()
	cmNamespace := oldCM.GetObjectMeta().GetNamespace()
	cmData := oldCM.Data
	_, err := ipvsc.getConfigMap(cmNamespace, cmName)
	glog.Infof("in sync configmap %s, data: %s", oldCM.Name, cmData)
	// on delete configmap
	if err != nil {
		if apierrors.IsNotFound(err) {
			err = ipvsc.OnDeleteConfigmap(oldCM)
		}
		return err
	}

	// on create configmap, bind_ip is not set
	err = ipvsc.OnSyncConfigmap(oldCM)
	return err
}
