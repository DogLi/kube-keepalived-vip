package controller

import (
	"github.com/golang/glog"
	"math/rand"
	"time"
)

func acquire_vip() (string, error) {
	ips := []string{"61", "62", "63", "64", "65", "66", "67", "68", "69", "70", "71", "72", "73"}
	rand.Seed(time.Now().Unix())
	n := rand.Int() % len(ips)
	ip := "10.10.40." + ips[n]

	//ip = "10.10.40.61"
	glog.Info("get IP from zstack: %s", ip)
	return ip, nil
}

func release_vip(vip string) error {
	glog.Infof("return back the ip {}", vip)
	return nil
}
