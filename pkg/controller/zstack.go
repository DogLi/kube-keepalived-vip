package controller

import (
	"sync"
	"strings"
	"time"
	"math/rand"
	"github.com/golang/glog"
)

var ipMutex sync.Mutex

var ipPool = []string{"61", "62", "63", "64", "65", "66", "67", "68", "69", "70", "71", "72", "73"}

func (ipvsc *ipvsControllerController) AcquireVip() (string, error) {
	ipMutex.Lock()
	defer ipMutex.Unlock()
	rand.Seed(time.Now().UnixNano())
	index := rand.Intn(10)
	ipTail := ipPool[index]
	ipPool =  append(ipPool[:index], ipPool[index+1:]...)
	ip := "10.10.40." + ipTail

	//ip = "10.10.40.61"
	glog.Info("get IP from zstack(%s): %s",ipPool, ip)
	return ip, nil
}

func (ipvsc *ipvsControllerController)ReleaseVip(vip string) error {
	glog.Infof("return back the ip {}", vip)
	glog.Infof("ip pool: %s", ipPool)
	ipTail := strings.SplitAfter(vip,".")[3]
	ipPool = append(ipPool, ipTail)
	return nil
}
