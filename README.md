# kube-keepalived-vip
Kubernetes Virtual IP address/es using [keepalived](http://www.keepalived.org)

AKA "how to set up virtual IP addresses in kubernetes using [IPVS - The Linux Virtual Server Project](http://www.linuxvirtualserver.org/software/ipvs.html)".


## Overview

There are 2 ways to expose a service in the current kubernetes service model:

- **L4 LoadBalancer**: [Available only on cloud providers](https://kubernetes.io/docs/tasks/access-application-cluster/create-external-load-balancer/) such as GCE and AWS
- **Service via NodePort**: The [NodePort directive](https://kubernetes.io/docs/concepts/services-networking/service/#type-nodeport) allocates a port on every worker node, which proxy the traffic to the respective Pod.
- **L7 Ingress**: The [Ingress](https://kubernetes.io/docs/concepts/services-networking/ingress/) is a dedicated loadbalancer (eg. nginx, HAProxy, traefik, vulcand) that redirects incoming HTTP/HTTPS traffic to the respective endpoints


This just works. What's the issue then?

```
                                                  ___________________
                                                 |                   |
                                           |-----| Host IP: 10.4.0.3 |
                                           |     |___________________|
                                           |
                                           |      ___________________
                                           |     |                   |
Public ----(example.com = 10.4.0.3/4/5)----|-----| Host IP: 10.4.0.4 |
                                           |     |___________________|
                                           |
                                           |      ___________________
                                           |     |                   |
                                           |-----| Host IP: 10.4.0.5 |
                                                 |___________________|
```

The issue is that it does not provide High Availability because beforehand is required to know the IP addresss of the node where is running and in case of a failure the pod can be be moved to a different node.The sysadmin has to step in and delist the faulty node from `example.com`. Since there will be intermittent downtime until the sysadmin intervenes, this isn't true High Availability (HA). Here is where ipvs could help.

```
                                               ___________________
                                              |                   |
                                              | VIP: 10.4.0.50    |
                                        |-----| Host IP: 10.4.0.3 |
                                        |     | Role: Master      |
                                        |     |___________________|
                                        |
                                        |      ___________________
                                        |     |                   |
                                        |     | VIP: Unassigned   |
Public ----(example.com = 10.4.0.50)----|-----| Host IP: 10.4.0.3 |
                                        |     | Role: Slave       |
                                        |     |___________________|
                                        |
                                        |      ___________________
                                        |     |                   |
                                        |     | VIP: Unassigned   |
                                        |-----| Host IP: 10.4.0.3 |
                                              | Role: Slave       |
                                              |___________________|
```

The idea is to define an IP address per service to expose it outside the Kubernetes cluster and use vrrp to announce this "mapping" in the local network.
With 2 or more instance of the pod running in the cluster is possible to provide high availabity using a single IP address.

### What is the difference between this and [service-loadbalancer](https://github.com/kubernetes/contrib/tree/master/service-loadbalancer) or [nginx-alpha](https://github.com/kubernetes/contrib/tree/master/Ingress/controllers/nginx-alpha) to expose one or more services?

This should be considered a complement, not a replacement for HAProxy or nginx. The goal using keepalived is to provide high availability and to bring certainty about how an exposed service can be reached (beforehand we know the ip address independently of the node where is running). For instance keepalived can use used to expose the service-loadbalancer or nginx ingress controller in the LAN using one IP address.


## Requirements

[Daemonsets](https://github.com/kubernetes/kubernetes/blob/master/docs/design/daemon.md) enabled is the only requirement. Check this [guide](https://github.com/kubernetes/kubernetes/blob/master/docs/api.md#enabling-resources-in-the-extensions-group) with the required flags in kube-apiserver.


## Configuration
To expose a service(`service-namespace/service-name`) with a `vip`, use a configmap which labled with `loadbalancer:zstack`, the value `zstack` is the default label value, which can be replaced with other name such as `--loadbalancer othername` when start the keepalived control.

The `Data` field of the configmap should be:

```
target-service: service-name
target-namespace: service-namespace
lvs-method: NAT
vip: `IP`
```

* `target-service`: required, used to keep the service name you want to expose with vip.
* `target-namespace`: optional, use to set the service namespace, if not set, default is `default`.
* `lvs-method`: optional, used to set the lvs-method in keepalived configmap, the valid options are `NAT` and `DR`. By default, if the method is not specified it will use `NAT`.
* `vip`: optinal, you can manually set the vip, and `IP` must be routable inside the LAN and must be available. If not set, the kube-keepalived-vip will get vip from zstack, bind the vip with the service, and set vip into the configmap's `data` field.

## get vip and release vip
When a new configmap created, and the `vip` is not set in the `Data` filed, kube-keepalived-vip will require a ip from zstack. **Only** when the configmap deleted, kube-keepalived-vip will release the vip.

## Communicate with ZStack
Currently kube-keepalived-vip communicate with ZStack directly by using the RabbitMQ, so you should set the `RABBITMQ` env in the `Daemonset`, which is a list of RabbitMQ url. A single url is in format of `account:password@ip:port/virtual_host_name` or `account:password@ip:port`. Multiple urls are split by ','.


## Watches

* `configmaps`: Watch the configmaps with label `loadbalancer zstack`, then kube-keepalived-vip will bind a `vip` to the `service` which specified in the configmap, the keepalived configureation file will be updated and reloaded.
* `endpoints`: When one pod dies or a new one is created by a scale event which associated with the `service`, then the `endpoints` will change, keepalived configuration file will be updated and reloaded.

## Example

First we create a new replication controller and service

```
$ kubectl create -f examples/echoheaders.yaml
replicationcontroller "echoheaders" created
You have exposed your service on an external port on all nodes in your
cluster.  If you want to expose this service to the external internet, you may
need to set up firewall rules for the service port(s) (tcp:30302) to serve traffic.

See http://releases.k8s.io/HEAD/docs/user-guide/services-firewalls.md for more details.
service "echoheaders" created
```

Next add the required annotation to expose the service using a local IP

```
$ kubectl create -f examples/vip-configmap.yaml
```

Configure the DaemonSet in `vip-daemonset.yaml` to use the ServiceAccount. Add the `serviceAccount` to the file as shown:

```
spec:
      hostNetwork: true
      serviceAccount: kube-keepalived-vip
```

Configure its ClusterRole. _keepalived_ needs to read the pods, nodes, endpoints, configmaps and services.

```
$ kubectl create -f vip-role.yaml
```


Now the creation of the daemonset

```
$ kubectl create -f examples/vip-daemonset.yaml
daemonset "kube-keepalived-vip" created
$ kubectl get daemonset
NAME                  DESIRED   CURRENT   READY     UP-TO-DATE   AVAILABLE   NODE-SELECTOR   AGE
kube-keepalived-vip   2         2         2         2            2           type=worker     10d
```

**Note: the daemonset yaml file contains a node selector. This is not required, is just an example to show how is possible to limit the nodes where keepalived can run**

To verify if everything is working we should check if a `kube-keepalived-vip` pod is in each node of the cluster

```
$ kubectl get nodes
NAME       LABELS                                        STATUS    AGE
10.4.0.3   kubernetes.io/hostname=10.4.0.3,type=worker   Ready     1d
10.4.0.4   kubernetes.io/hostname=10.4.0.4,type=worker   Ready     1d
10.4.0.5   kubernetes.io/hostname=10.4.0.5,type=worker   Ready     1d
```

```
$ kubectl get pods
NAME                        READY     STATUS    RESTARTS   AGE
echoheaders-co4g4           1/1       Running   0          5m
kube-keepalived-vip-a90bt   1/1       Running   0          53s
kube-keepalived-vip-g3nku   1/1       Running   0          52s
kube-keepalived-vip-gd18l   1/1       Running   0          54s
```

Onece the kube-keepalived-vip pod run, you can get the **vip** generated by the loadbalancer controller from the configmap.

```
$ kubectl get configmap vip-configmap -o yaml
apiVersion: v1
data:
  lvs-method: NAT
  target-namespace: default
  target-service: echoheaders
  vip: 10.4.0.50
kind: ConfigMap
metadata:
  creationTimestamp: 2017-12-07T08:42:01Z
  labels:
    loadbalancer: zstack
  name: vip-test
  namespace: default
  resourceVersion: "5941043"
  selfLink: /api/v1/namespaces/default/configmaps/vip-test
  uid: 7e7d24bf-db2a-11e7-909b-525400e717a6
```

```
$ kubectl logs kube-keepalived-vip-a90bt
I0410 14:24:45.860119       1 keepalived.go:161] cleaning ipvs configuration
I0410 14:24:45.873095       1 main.go:109] starting LVS configuration
I0410 14:24:45.894664       1 main.go:119] starting keepalived to announce VIPs
Starting Healthcheck child process, pid=17
Starting VRRP child process, pid=18
Initializing ipvs 2.6
Registering Kernel netlink reflector
Registering Kernel netlink reflector
Registering Kernel netlink command channel
Registering gratuitous ARP shared channel
Registering Kernel netlink command channel
Using LinkWatch kernel netlink reflector...
Using LinkWatch kernel netlink reflector...
I0410 14:24:56.017590       1 keepalived.go:151] reloading keepalived
Got SIGHUP, reloading checker configuration
Registering Kernel netlink reflector
Initializing ipvs 2.6
Registering Kernel netlink command channel
Registering gratuitous ARP shared channel
Registering Kernel netlink reflector
Opening file '/etc/keepalived/keepalived.conf'.
Registering Kernel netlink command channel
Opening file '/etc/keepalived/keepalived.conf'.
Using LinkWatch kernel netlink reflector...
VRRP_Instance(vips) Entering BACKUP STATE
Using LinkWatch kernel netlink reflector...
Activating healthchecker for service [10.2.68.5]:8080
VRRP_Instance(vips) Transition to MASTER STATE
VRRP_Instance(vips) Entering MASTER STATE
VRRP_Instance(vips) using locally configured advertisement interval (1000 milli-sec)
```

```
$ kubectl exec kube-keepalived-vip-a90bt cat /etc/keepalived/keepalived.conf

global_defs {
  vrrp_version 3
  vrrp_iptables KUBE-KEEPALIVED-VIP
}

vrrp_instance vips {
  state BACKUP
  interface eth1
  virtual_router_id 50
  priority 100
  nopreempt
  advert_int 1

  track_interface {
    eth1
  }



  virtual_ipaddress {
    172.17.4.90
  }
}


# Service: default/echoheaders
virtual_server 10.4.0.50 80 {
  delay_loop 5
  lvs_sched wlc
  lvs_method NAT
  persistence_timeout 1800
  protocol TCP


  real_server 10.2.68.5 8080 {
    weight 1
    TCP_CHECK {
      connect_port 8080
      connect_timeout 3
    }
  }

}

```


```
$ curl -v 10.4.0.50
* Rebuilt URL to: 10.4.0.50/
*   Trying 10.4.0.50...
* Connected to 10.4.0.50 (10.4.0.50) port 80 (#0)
> GET / HTTP/1.1
> Host: 10.4.0.50
> User-Agent: curl/7.43.0
> Accept: */*
>
* HTTP 1.0, assume close after body
< HTTP/1.0 200 OK
< Server: BaseHTTP/0.6 Python/3.5.0
< Date: Wed, 30 Dec 2015 19:52:39 GMT
<
CLIENT VALUES:
client_address=('10.4.0.148', 52178) (10.4.0.148)
command=GET
path=/
real path=/
query=
request_version=HTTP/1.1

SERVER VALUES:
server_version=BaseHTTP/0.6
sys_version=Python/3.5.0
protocol_version=HTTP/1.0

HEADERS RECEIVED:
Accept=*/*
Host=10.4.0.50
User-Agent=curl/7.43.0
* Closing connection 0

```

Scaling the replication controller should update and reload keepalived

```
$ kubectl scale --replicas=5 replicationcontroller echoheaders
replicationcontroller "echoheaders" scaled
```


```
$ kubectl exec kube-keepalived-vip-a90bt cat /etc/keepalived/keepalived.conf

global_defs {
  vrrp_version 3
  vrrp_iptables KUBE-KEEPALIVED-VIP
}

vrrp_instance vips {
  state BACKUP
  interface eth1
  virtual_router_id 50
  priority 100
  nopreempt
  advert_int 1

  track_interface {
    eth1
  }



  virtual_ipaddress {
    172.17.4.90
  }
}


# Service: default/echoheaders
virtual_server 10.4.0.50 80 {
  delay_loop 5
  lvs_sched wlc
  lvs_method NAT
  persistence_timeout 1800
  protocol TCP


  real_server 10.2.68.5 8080 {
    weight 1
    TCP_CHECK {
      connect_port 8080
      connect_timeout 3
    }
  }

  real_server 10.2.68.6 8080 {
    weight 1
    TCP_CHECK {
      connect_port 8080
      connect_timeout 3
    }
  }

  real_server 10.2.68.7 8080 {
    weight 1
    TCP_CHECK {
      connect_port 8080
      connect_timeout 3
    }
  }

  real_server 10.2.68.8 8080 {
    weight 1
    TCP_CHECK {
      connect_port 8080
      connect_timeout 3
    }
  }

  real_server 10.2.68.9 8080 {
    weight 1
    TCP_CHECK {
      connect_port 8080
      connect_timeout 3
    }
  }

}
```

## Building

```
$ make controller
```

## Related projects

- https://github.com/kobolog/gorb
- https://github.com/qmsk/clusterf
- https://github.com/osrg/gobgp
