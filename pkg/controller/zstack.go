package controller

import (
	"sync"
	"time"
	"github.com/golang/glog"
	"math/rand"
)

var ipMutex sync.Mutex

func (ipvsc *ipvsControllerController) AcquireVip() (string, error) {
	ipMutex.Lock()
	defer ipMutex.Unlock()
	ips := []string{"61", "62", "63", "64", "65", "66", "67", "68", "69", "70", "71", "72", "73"}
	rand.Seed(time.Now().Unix())
	n := rand.Int() % len(ips)
	ip := "10.10.40." + ips[n]

	//ip = "10.10.40.61"
	glog.Info("get IP from zstack: %s", ip)
	return ip, nil
}

func (ipvsc *ipvsControllerController)ReleaseVip(vip string) error {
	glog.Infof("return back the ip {}", vip)
	return nil
}
