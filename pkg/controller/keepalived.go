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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"text/template"

	"github.com/golang/glog"

	"bytes"
	"github.com/dogli/kube-keepalived-vip/pkg/constants"
	"k8s.io/kubernetes/pkg/util/iptables"
	k8sexec "k8s.io/utils/exec"
	"sort"
)

var keepaliveMutex sync.Mutex

type keepalived struct {
	iface          string
	ip             string
	netmask        int
	priority       int
	nodes          []string
	neighbors      []string
	useUnicast     bool
	started        bool
	VIPs           []vip
	Services       map[string]string
	keepalivedTmpl *template.Template
	haproxyTmpl    *template.Template
	cmd            *exec.Cmd
	ipt            iptables.Interface
	vrid           int
	proxyMode      bool
}

// WriteCfg creates a new keepalived configuration file.
// In case of an error with the generation it returns the error
func (k *keepalived) WriteCfg() error {
	w, err := os.Create(constants.KeepalivedCfg)
	if err != nil {
		return err
	}

	VIPs := k.VIPs
	sort.Sort(vipByNameIPPort(VIPs))
	vips := k.getVIPs()
	defer w.Close()

	conf := make(map[string]interface{})
	conf["iptablesChain"] = constants.IptablesChain
	conf["iface"] = k.iface
	conf["myIP"] = k.ip
	conf["netmask"] = k.netmask
	conf["svcs"] = VIPs
	conf["vips"] = vips
	conf["nodes"] = k.neighbors
	conf["priority"] = k.priority
	conf["useUnicast"] = k.useUnicast
	conf["vrid"] = k.vrid
	conf["proxyMode"] = k.proxyMode
	conf["vipIsEmpty"] = len(vips) == 0

	if glog.V(2) {
		b, _ := json.Marshal(conf)
		glog.Infof("%v", string(b))
	}

	if err = k.keepalivedTmpl.Execute(w, conf); err != nil {
		glog.Infof("error to generate keepalived config: %s", constants.KeepalivedCfg)
		return err
	}

	// just for debug
	var buffer bytes.Buffer
	k.keepalivedTmpl.Execute(&buffer, conf)
	content := buffer.String()
	glog.Infof("ip: %s, VIPs:   %s",k.ip, VIPs)
	glog.Infof("Services: %s", k.Services)
	glog.Infof("content:\n%s", content)

	if k.proxyMode {
		w, err := os.Create(constants.HaproxyCfg)
		if err != nil {
			return err
		}
		defer w.Close()
		err = k.haproxyTmpl.Execute(w, conf)
		if err != nil {
			return fmt.Errorf("unexpected error creating haproxy.cfg: %v", err)
		}
	}

	return nil
}

func (k *keepalived) getVIPs() []string {
	result := []string{}
	for _, VIP := range k.VIPs {
		result = appendIfMissing(result, VIP.VIP)
	}
	return result
}
func (k *keepalived) resetIPVS() error {
	glog.Info("cleaning ipvs configuration")
	_, err := k8sexec.New().Command("ipvsadm", "-C").CombinedOutput()
	if err != nil {
		return fmt.Errorf("error removing ipvs configuration: %v", err)
	}
	return nil
}

// Start starts a keepalived process in foreground.
// In case of any error it will terminate the execution with a fatal error
func (k *keepalived) Start() {
	ae, err := k.ipt.EnsureChain(iptables.TableFilter, iptables.Chain(constants.IptablesChain))
	if err != nil {
		glog.Fatalf("unexpected error: %v", err)
	}
	if ae {
		glog.V(2).Infof("chain %v already existed", constants.IptablesChain)
	}

	k.cmd = exec.Command("keepalived",
		"--dont-fork",
		"--log-console",
		"--release-vips",
		"--pid", "/keepalived.pid")

	k.cmd.Stdout = os.Stdout
	k.cmd.Stderr = os.Stderr

	k.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	k.started = true
	if err := k.cmd.Start(); err != nil {
		glog.Errorf("keepalived error: %v", err)
	}

	if err := k.cmd.Wait(); err != nil {
		glog.Fatalf("keepalived error: %v", err)
	}
}

// Reload sends SIGHUP to keepalived to reload the configuration.
func (k *keepalived) Reload() error {
	if !k.started {
		// TODO: add a warning indicating that keepalived is not started?
		return nil
	}

	glog.Info("reloading keepalived")
	err := syscall.Kill(k.cmd.Process.Pid, syscall.SIGHUP)
	if err != nil {
		return fmt.Errorf("error reloading keepalived: %v", err)
	}

	return nil
}

// Stop keepalived process
func (k *keepalived) Stop() error {
	vips := k.getVIPs()
	for _, vip := range vips {
		k.removeVIP(vip)
	}

	err := k.ipt.FlushChain(iptables.TableFilter, iptables.Chain(constants.IptablesChain))
	if err != nil {
		glog.V(2).Infof("unexpected error flushing iptables chain %v: %v", err, constants.IptablesChain)
	}

	err = syscall.Kill(k.cmd.Process.Pid, syscall.SIGTERM)
	if err != nil {
		glog.Errorf("error stopping keepalived: %v", err)
	}
	return err
}

func (k *keepalived) removeVIP(vip string) error {
	glog.Infof("removing configured VIP %v", vip)
	out, err := k8sexec.New().Command("ip", "addr", "del", vip+"/32", "dev", k.iface).CombinedOutput()
	if string(out) == "RTNETLINK answers: Cannot assign requested address" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("error reloading keepalived: %v\n%s", err, out)
	}
	return nil
}

// DeleteVIP removes a VIP from the keepalived config
func (k *keepalived) DeleteVIP(v string) error {
	newVIP := []vip{}
	glog.Infof("Deleing VIP %v", v)
	for index, VIP := range k.VIPs {
		if VIP.VIP == v {
			newVIP = append(k.VIPs[:index], k.VIPs[index+1:]...)
			k.VIPs = newVIP
		}
	}
	err := k.WriteCfg()
	return err
}

// AddVIP add a VIP from the keepalived config
func (k *keepalived) AddVIPs(bindIP string, VIPs []vip) error {

	glog.Infof("add vips to keepalived: %s: %s", bindIP, VIPs)
	// delete the old VIP first
	for index, V := range k.VIPs {
		if V.VIP == bindIP {
			k.VIPs = append(k.VIPs[:index], k.VIPs[index+1:]...)
		}
	}

	// add the new VIPs
	k.VIPs = append(k.VIPs, VIPs...)
	err := k.WriteCfg()
	return err
}

func (k *keepalived) loadTemplates() error {
	tmpl, err := template.ParseFiles(constants.KeepalivedTmpl)
	if err != nil {
		return err
	}
	k.keepalivedTmpl = tmpl

	tmpl, err = template.ParseFiles(constants.HaproxyTmpl)
	if err != nil {
		return err
	}
	k.haproxyTmpl = tmpl

	return nil
}
