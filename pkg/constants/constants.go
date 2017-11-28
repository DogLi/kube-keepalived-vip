package constants

const (
	BindIP          = "vip"
	TargetService   = "target-service"
	TargetNamespace = "target-namespace"
	LvsMethod       = "lvs-method"

	IptablesChain = "KUBE-KEEPALIVED-VIP"
	KeepalivedCfg = "/etc/keepalived/keepalived.conf"
	HaproxyCfg    = "/etc/haproxy/haproxy.cfg"

	KeepalivedTmpl = "keepalived.tmpl"
	HaproxyTmpl    = "haproxy.tmpl"
)

