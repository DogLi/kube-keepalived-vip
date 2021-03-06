package controller

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"github.com/dogli/kube-keepalived-vip/pkg/constants"
	"github.com/golang/glog"
	"io"
	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"os"
	"os/signal"
	"sort"
	"syscall"
)

func (ipvsc *ipvsControllerController) getConfigMap(ns, name string) (*apiv1.ConfigMap, error) {
	configmap, err := ipvsc.client.CoreV1().ConfigMaps(ns).Get(name, metav1.GetOptions{})
	if err != nil {
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
			for _, epAddress := range ss.NotReadyAddresses {
				endpoints = append(endpoints, service{IP: epAddress.IP, Port: targetPort})
			}
		}
	}

	glog.V(2).Infof("get endpointd: %s", endpoints)
	return endpoints
}

// getServices returns a list of services and their endpoints.
func (ipvsc *ipvsControllerController) getService(cm *apiv1.ConfigMap) ([]vip, error) {
	svcs := []vip{}

	cmData := cm.Data
	bind_ip, ok := cmData[constants.BindIP]
	if !ok {
		glog.Warningf("There is no VIP in configmap: %s", cm.GetName())
		return svcs, nil
	}
	svc := cmData[constants.TargetService]
	ns, ok := cmData[constants.TargetNamespace]
	if !ok {
		ns = "default"
		cm.Data[constants.TargetNamespace] = ns
	}
	lvs_method, ok := cmData[constants.LvsMethod] // ["NAT", "DR"]
	if !ok {
		lvs_method = "NAT"
		cm.Data[constants.LvsMethod] = "NAT"
		glog.Infof("set the lvs_method to NAT for configmap %s", cm.GetName())
	} else {
		if lvs_method != "NAT" && lvs_method != "DR" {
			glog.Warningf("invalid lvs_method '%s' in configmap %s, set it to 'NAT'", lvs_method, cm.GetName())
			cm.Data[constants.LvsMethod] = "NAT"
		}
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
			Name:      fmt.Sprintf("%v/%v", s.GetNamespace(), s.GetName()),
			VIP:       bind_ip,
			Port:      int(servicePort.Port),
			LVSMethod: lvs_method,
			Backends:  ep,
			Protocol:  fmt.Sprintf("%v", servicePort.Protocol),
		})
		glog.V(2).Infof("found service: %v:%v", s.Name, servicePort.Port)
	}
	sort.Sort(vipByNameIPPort(svcs))
	return svcs, nil
}

func getServicename(cfm *apiv1.ConfigMap) string {
	cfmData := cfm.Data
	namespace, ok := cfmData[constants.TargetNamespace]
	if !ok {
		namespace = "default"
	}
	service := cfmData[constants.TargetService]
	return fmt.Sprintf("%v/%v", namespace, service)
}

func (ipvsc *ipvsControllerController) getServices() ([]vip, map[string]string) {
	configmaps := ipvsc.getConfigMaps()
	svcs := []vip{}
	CfmSvc := make(map[string]string)
	for _, configmap := range configmaps {
		key := fmt.Sprintf("%s/%s", configmap.GetNamespace(), configmap.GetName())
		glog.Infof("Find configmap %s", key)
		CfmSvc[key] = getServicename(configmap)
		svc, err := ipvsc.getService(configmap)
		if err != nil {
			glog.Warningf("can not get service info from configmap %s", configmap.Name)
			continue
		}
		svcs = append(svcs, svc...)
	}
	sort.Sort(vipByNameIPPort(svcs))
	return svcs, CfmSvc
}

func (ipvsc *ipvsControllerController) freshKeepalivedConf() error {
	// get all svcs and restart keepalived
	ipvsc.keepalived.VIPs, ipvsc.keepalived.Services = ipvsc.getServices()
	err := ipvsc.keepalived.WriteCfg()
	return err
}

func (ipvsc *ipvsControllerController) reload() error {
	md5, err := checksum(constants.KeepalivedCfg)
	if err == nil && md5 == ipvsc.ruMD5 {
		glog.Infof("get same MD5:  %s", ipvsc.ruMD5)
		return nil
	}

	ipvsc.ruMD5 = md5
	err = ipvsc.keepalived.Reload()
	return err
}

func (ipvsc *ipvsControllerController) OnAddConfigmap(cfm *apiv1.ConfigMap) error {
	configMapMutex.Lock()
	defer configMapMutex.Unlock()

	cfm = cfm.DeepCopy()
	bindIP, ok := cfm.Data[constants.BindIP]
	// add configmap without bind ip
	if !ok {
		err := fmt.Errorf("can not find vip in configmap %s", cfm.GetName())
		//return err
		glog.Errorf("%s", err)
		return nil
	}
	VIPs, err := ipvsc.getService(cfm)
	err = ipvsc.keepalived.AddVIPs(bindIP, VIPs)
	if err != nil {
		glog.Errorf("error reloading keepalived: %v", err)
	}
	glog.Infof("reload keepalived!")
	err = ipvsc.reload()
	return err
}

func (ipvsc *ipvsControllerController) OnUpdateConfigmap(old, cur interface{}) error {
	configMapMutex.Lock()
	defer configMapMutex.Unlock()
	oldCM := old.(*apiv1.ConfigMap)
	curCM := cur.(*apiv1.ConfigMap)

	glog.Infof("update configmap, old configmap data: %v, current configmap data: %v", oldCM.Data, curCM.Data)

	// we should not change the target service
	if curCM.Data[constants.TargetService] != oldCM.Data[constants.TargetService] || curCM.Data["constants.TargetNamespace"] != oldCM.Data["constants.TargetNamespace"] {
		glog.Errorf("you are trying to change the target service or target service namespace, forbidden!")
		curCM.Data = oldCM.Data
		ipvsc.client.CoreV1().ConfigMaps(curCM.GetNamespace()).Update(curCM)
		return nil
	}

	old_bind_ip, oldOk := oldCM.Data[constants.BindIP]
	_, curOk := curCM.Data[constants.BindIP]
	if !oldOk  && !curOk{
		glog.Errorf("vip is not set in configmap %s", oldCM.GetName())
		return nil
	} else if oldOk && !curOk { // someone want to delete the bind ip
		curCM.Data[constants.BindIP] = old_bind_ip
		_, err := ipvsc.client.CoreV1().ConfigMaps(curCM.GetNamespace()).Update(curCM)
		return err
	} else {
		if err := ipvsc.freshKeepalivedConf(); err != nil {
			return err
		} else {
			return ipvsc.reload()
		}
	}
	return nil
}

func (ipvsc *ipvsControllerController) OnDeleteConfigmap(cfm *apiv1.ConfigMap) error {
	configMapMutex.Lock()
	defer configMapMutex.Unlock()
	delete(ipvsc.keepalived.Services, fmt.Sprintf("%s/%s", cfm.GetNamespace(), cfm.GetName()))
	cmData := cfm.Data
	vip, ok := cmData[constants.BindIP]
	if ok {
		err := ipvsc.keepalived.DeleteVIP(vip)
		if err != nil {
			return err
		}
		err = ipvsc.reload()
		return err
	}
	return nil
}


func (ipvsc *ipvsControllerController) ObjectFilter(obj metav1.Object) bool {
	name := fmt.Sprintf("%v/%v", obj.GetNamespace(), obj.GetName())
	Services := ipvsc.keepalived.Services
	for _, value := range Services {
		if name == value {
			return true
		}
	}
	return false
}

func (ipvsc *ipvsControllerController) sync(obj interface{}) error {
	objMeta, err := meta.Accessor(obj)
	if err == nil {
		if ipvsc.ObjectFilter(objMeta) {
			configMapMutex.Lock()
			defer configMapMutex.Unlock()
			err := ipvsc.freshKeepalivedConf()
			if err == nil {
				err = ipvsc.reload()
			}
			return err
		}
	}
	return err
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
		glog.Infof("sync: delete configmap ...")
		if apierrors.IsNotFound(err) {
			err = ipvsc.OnDeleteConfigmap(oldCM)
		}
		return err
	}
	return nil
}

func (ipvsc *ipvsControllerController) AddConfigmap(oldobj interface{}) error {

	ipvsc.reloadRateLimiter.Accept()

	oldCM := oldobj.(*apiv1.ConfigMap)
	cmName := oldCM.GetObjectMeta().GetName()
	cmNamespace := oldCM.GetObjectMeta().GetNamespace()
	cmData := oldCM.Data
	_, err := ipvsc.getConfigMap(cmNamespace, cmName)
	glog.Infof("in sync configmap %s, data: %s", oldCM.Name, cmData)
	// on delete configmap
	if err != nil {
		return nil
	}

	// on create configmap
	err = ipvsc.OnAddConfigmap(oldCM)
	return err
}