package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"text/template"

	"github.com/golang/glog"
	k8sexec "k8s.io/kubernetes/pkg/util/exec"
)

const (
	keepalivedTmpl = `{{ $iface := .iface }}{{ $netmask := .netmask }}
vrrp_sync_group VG_1 
  group {
    vips
  }
}

vrrp_instance vips {
  state BACKUP
  interface {{ $iface }}
  virtual_router_id 50
  priority {{ .priority }}
  nopreempt
  advert_int 1

  track_interface {
    {{ $iface }}
  }

  # TODO: use unicast instead multicast
  #unicast_src_ip {{ .myIP }}
  #unicast_peer { {{ range $i, $node := .nodes }}
  #  {{ $node }}{{ end }}
  #}

  virtual_ipaddress { {{ range $i, $svc := .svcs }}
    {{ $svc.Ip }}
  {{ end }}}    

  authentication {
    auth_type AH
    auth_pass {{ .authPass }}
  }
}

{{ range $i, $svc := .svcs }}
virtual_server {{ $svc.Ip }} {{ $svc.Port }} {
  delay_loop 5
  lvs_sched wlc
  lvs_method NAT
  persistence_timeout 1800
  protocol TCP
  #{{ $svc.Protocol }}
  alpha

  {{ range $j, $backend := $svc.Backends }}
  real_server {{ $backend.Ip }} {{ $backend.Port }} {
    weight 1
    TCP_CHECK {
      connect_port {{ $backend.Port }}
      connect_timeout 3
    }
  }
{{ end }}
}    
{{ end }}
`
)

type keepalived struct {
	iface      string
	ip         string
	netmask    int
	priority   int
	nodes      []string
	neighbors  []string
	runningCfg []vip
}

func (k *keepalived) WriteCfg(svcs []vip) error {
	k.runningCfg = svcs

	w, err := os.Create("/etc/keepalived/keepalived.conf")
	if err != nil {
		return err
	}
	defer w.Close()

	t, err := template.New("keepalived").Parse(keepalivedTmpl)
	if err != nil {
		return err
	}

	conf := make(map[string]interface{})
	conf["iface"] = k.iface
	conf["myIP"] = k.ip
	conf["netmask"] = k.netmask
	conf["svcs"] = svcs
	conf["nodes"] = k.neighbors
	conf["priority"] = k.priority
	conf["authPass"] = k.getSha()

	b, _ := json.Marshal(conf)
	glog.Infof("%v", string(b))

	return t.Execute(w, conf)
}

func (k *keepalived) Start() {
	cmd := exec.Command("keepalived",
		"--dont-fork",
		"--log-console",
		"-D",
		"--pid", "/keepalived.pid")

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		glog.Errorf("keepalived error: %v", err)
	}

	if err := cmd.Wait(); err != nil {
		glog.Fatalf("keepalived error: %v", err)
	}
}

func (k *keepalived) Reload() error {
	glog.Info("reloading keepalived")
	_, err := k8sexec.New().Command("killall", "-1", "keepalived").CombinedOutput()
	if err != nil {
		return fmt.Errorf("error reloading keepalived: %v", err)
	}

	return nil
}

// getSha returns a sha1 of the list of nodes in the cluster
func (k *keepalived) getSha() string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%v", k.nodes)))
	return hex.EncodeToString(h.Sum(nil))
}