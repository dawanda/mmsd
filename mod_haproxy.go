package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/dawanda/mmsd/core"
)

const (
	LB_PROXY_PROTOCOL      = "lb-proxy-protocol"
	LB_ACCEPT_PROXY        = "lb-accept-proxy"
	LB_VHOST_HTTP          = "lb-vhost"
	LB_VHOST_DEFAULT_HTTP  = "lb-vhost-default"
	LB_VHOST_HTTPS         = "lb-vhost-ssl"
	LB_VHOST_DEFAULT_HTTPS = "lb-vhost-default-ssl"
	LB_DISABLED            = "lb-disabled"
)

var (
	ErrBadExit = errors.New("Bad Process Exit.")
)

type HaproxyModule struct {
	Verbose           bool   // log verbose logging messages?
	LocalHealthChecks bool   // wether or not to perform local health checks or rely on Marathon events.
	ServiceAddr       net.IP // IP address the load balancers should bind on.
	GatewayEnabled    bool   // Enables/disables HTTP/S gateway support
	GatewayAddr       net.IP // HTTP gateway bind IP address
	GatewayPortHTTP   uint   // HTTP gateway HTTP port (usually 80)
	GatewayPortHTTPS  uint   // HTTP gateway HTTPS port (usually 443)
	HaproxyExe        string // path to haproxy executable
	ConfigPath        string // path to generated config file (/var/run/mmsd/haproxy.cfg)
	ConfigTailPath    string // path to appended haproxy config file (/etc/mmsd/haproxy-tail.cfg)
	OldConfigPath     string // path to previousely generated config file
	PidFile           string // path to PID-file for currently running haproxy executable
	ManagementAddr    net.IP // haproxy stats listen addr
	ManagementPort    uint   // haproxy stats port
	AdminSockPath     string // path to haproxy admin socket

	vhostsHTTP        map[string][]string // [appId] = []vhost
	vhostDefaultHTTP  string              // default HTTP vhost
	vhostsHTTPS       map[string][]string // [appId] = []vhost
	vhostDefaultHTTPS string              // default HTTPS vhost
	appConfigCache    map[string]string   // [appId] = haproxy_config_fragment
}

func (module *HaproxyModule) Startup() {
	log.Printf("Haproxy starting")

	module.vhostsHTTP = make(map[string][]string)
	module.vhostDefaultHTTP = ""

	module.vhostsHTTPS = make(map[string][]string)
	module.vhostDefaultHTTPS = ""

	module.appConfigCache = make(map[string]string)
}

func (module *HaproxyModule) Shutdown() {
	log.Printf("Haproxy shutting down")
}

func (module *HaproxyModule) Apply(apps []*core.AppCluster) {
	log.Printf("Haproxy: apply([]AppCluster)")

	for _, app := range apps {
		if module.supportsProtocol(app.Protocol) {
			module.appConfigCache[app.Id] = module.makeConfig(app)
		}
	}

	err := module.writeConfig()
	if err != nil {
		log.Printf("Failed to write config. %v", err)
		return
	}

	err = module.reloadConfig()
	if err != nil {
		log.Printf("Reloading configuration failed. %v", err)
	}
}

func (module *HaproxyModule) AddTask(task *core.AppBackend, app *core.AppCluster) {
	if !module.supportsProtocol(app.Protocol) {
		return
	}
	log.Printf("Haproxy: AddTask()")
	// TODO
}

func (module *HaproxyModule) RemoveTask(task *core.AppBackend, app *core.AppCluster) {
	log.Printf("Haproxy: RemoveTask()")
	// TODO
}

func (module *HaproxyModule) makeConfig(app *core.AppCluster) string {
	module.updateGatewaySettings(app)

	// generate haproxy config fragment
	result := ""
	serverOpts := ""
	bindOpts := ""

	if runtime.GOOS == "linux" { // only enable on Linux (known to work)
		bindOpts += " defer-accept"
	}

	if Atoi(app.Labels[LB_ACCEPT_PROXY], 0) != 0 {
		bindOpts += " accept-proxy"
	}

	if module.LocalHealthChecks {
		serverOpts += " check"
	}

	if app.HealthCheck != nil && app.HealthCheck.IntervalSeconds > 0 {
		serverOpts += fmt.Sprintf(" inter %v", app.HealthCheck.IntervalSeconds*1000)
	}

	switch Atoi(app.Labels[LB_PROXY_PROTOCOL], 0) {
	case 2:
		serverOpts += " send-proxy-v2"
	case 1:
		serverOpts += " send-proxy"
	case 0:
		// ignore
	default:
		log.Printf("Invalid proxy-protocol given for %v: %v - ignoring.",
			app.Id, app.Labels["lb-proxy-protocol"])
	}

	switch Atoi(app.Labels[LB_PROXY_PROTOCOL], 0) {
	case 2:
		serverOpts += " send-proxy-v2"
	case 1:
		serverOpts += " send-proxy"
	case 0:
		// ignore
	default:
		log.Printf("Invalid proxy-protocol given for %v: %v - ignoring.",
			app.Id, app.Labels["lb-proxy-protocol"])
	}

	var appProtocol = module.getAppProtocol(app)
	switch appProtocol {
	case "HTTP":
		result += fmt.Sprintf(
			"frontend __frontend_%v\n"+
				"  bind %v:%v%v\n"+
				"  option dontlognull\n"+
				"  default_backend %v\n"+
				"\n"+
				"backend %v\n"+
				"  mode http\n"+
				"  balance leastconn\n"+
				"  option forwardfor\n"+
				"  option http-server-close\n"+
				"  option abortonclose\n",
			app.Id, module.ServiceAddr, app.ServicePort, bindOpts, app.Id, app.Id)

		if module.LocalHealthChecks && app.HealthCheck != nil {
			result += fmt.Sprintf(
				"  option httpchk GET %v HTTP/1.1\\r\\nHost:\\ %v\n",
				app.HealthCheck.Path, "health-check")
		}
	default:
		result += fmt.Sprintf(
			"listen %v\n"+
				"  bind %v:%v%v\n"+
				"  option dontlognull\n"+
				"  mode tcp\n"+
				"  balance leastconn\n",
			app.Id, module.ServiceAddr, app.ServicePort, bindOpts)
	}

	result += "  option redispatch\n"
	result += "  retries 1\n"

	for _, task := range app.Backends {
		if task.State == "TASK_RUNNING" {
			result += fmt.Sprintf(
				"  server %v:%v %v:%v%v\n",
				task.Host, task.Port, // taskLabel == "$host:$port"
				SoftResolveIPAddr(task.Host),
				task.Port,
				serverOpts)
		} else {
			log.Printf("Haproxy: skip task %v (%v).", task.Id, task.State)
		}
	}

	result += "\n"

	return result
}

func (module *HaproxyModule) updateGatewaySettings(app *core.AppCluster) {
	// update HTTP virtual hosting
	var lbVirtualHosts = makeStringArray(app.Labels[LB_VHOST_HTTP])
	if len(lbVirtualHosts) != 0 {
		module.vhostsHTTP[app.Id] = lbVirtualHosts
		if app.Labels[LB_VHOST_DEFAULT_HTTP] == "1" {
			module.vhostDefaultHTTP = app.Id
		}
	} else {
		delete(module.vhostsHTTP, app.Id)
		if module.vhostDefaultHTTP == app.Id {
			module.vhostDefaultHTTP = ""
		}
	}

	// update HTTPS virtual hosting
	lbVirtualHosts = makeStringArray(app.Labels[LB_VHOST_HTTPS])
	if len(lbVirtualHosts) != 0 {
		module.vhostsHTTPS[app.Id] = lbVirtualHosts
		if app.Labels[LB_VHOST_DEFAULT_HTTPS] == "1" {
			module.vhostDefaultHTTPS = app.Id
		}
	} else {
		delete(module.vhostsHTTPS, app.Id)
		if module.vhostDefaultHTTPS == app.Id {
			module.vhostDefaultHTTPS = ""
		}
	}
}

func (module *HaproxyModule) writeConfig() error {
	config := ""

	config += module.makeConfigHead()

	// add apps sorted to config
	var clusterNames []string
	for name, _ := range module.appConfigCache {
		clusterNames = append(clusterNames, name)
	}
	sort.Strings(clusterNames)
	for _, name := range clusterNames {
		config += module.appConfigCache[name]
	}

	config += module.makeConfigTail()

	// write file
	tempConfigFile := fmt.Sprintf("%v.tmp", module.ConfigPath)
	err := ioutil.WriteFile(tempConfigFile, []byte(config), 0666)
	if err != nil {
		return err
	}

	err = module.checkConfig(tempConfigFile)
	if err != nil {
		return err
	}

	// if config file previousely did exist, attempt a rename
	if _, err := os.Stat(module.ConfigPath); err == nil {
		if err = os.Rename(module.ConfigPath, module.OldConfigPath); err != nil {
			return err
		}
	}

	return os.Rename(tempConfigFile, module.ConfigPath)
}

func (module *HaproxyModule) makeConfigHead() string {
	config := ""

	config += fmt.Sprintf(
		"# This is an auto generated haproxy configuration!!!\n"+
			"global\n"+
			"  maxconn 32768\n"+
			"  maxconnrate 32768\n"+
			"  log 127.0.0.1 local0\n"+
			"  stats socket %v mode 600 level admin\n"+
			"\n"+
			"defaults\n"+
			"  maxconn 32768\n"+
			"  timeout client 90000\n"+
			"  timeout server 90000\n"+
			"  timeout connect 90000\n"+
			"  timeout queue 90000\n"+
			"  timeout http-request 90000\n"+
			"\n", module.AdminSockPath)

	config += fmt.Sprintf(
		"listen haproxy\n"+
			"  bind %v:%v\n"+
			"  mode http\n"+
			"  stats enable\n"+
			"  stats uri /\n"+
			"  stats admin if TRUE\n"+
			"  monitor-uri /haproxy?monitor\n"+
			"\n",
		module.ManagementAddr, module.ManagementPort)

	if module.GatewayEnabled {
		config += module.makeGatewayHTTP()
		config += module.makeGatewayHTTPS()
	}
	return config
}

func (module *HaproxyModule) makeGatewayHTTP() string {
	var (
		suffixRoutes  map[string]string = make(map[string]string)
		suffixMatches []string
		exactRoutes   map[string]string = make(map[string]string)
		exactMatches  []string
		vhostDefault  string
		port          uint = module.GatewayPortHTTP
	)

	for _, appID := range SortedVhostsKeys(module.vhostsHTTP) {
		vhosts := module.vhostsHTTP[appID]
		for _i, vhost := range vhosts {
			log.Printf("[haproxy] appID:%v, vhost:%v, i:%v\n", appID, vhost, _i)
			matchToken := "vhost_" + vhost
			matchToken = strings.Replace(matchToken, ".", "_", -1)
			matchToken = strings.Replace(matchToken, "*", "STAR", -1)

			if len(vhost) >= 3 && vhost[0] == '*' && vhost[1] == '.' {
				suffixMatches = append(suffixMatches,
					fmt.Sprintf("	acl %v	hdr_dom(host) -i %v\n", matchToken, strings.SplitN(vhost, ".", 2)[1]))
				suffixRoutes[matchToken] = appID
			} else {
				exactMatches = append(exactMatches,
					fmt.Sprintf("	acl %v hdr(host) -i %v\n", matchToken, vhost))
				exactRoutes[matchToken] = appID
			}

			if module.vhostDefaultHTTP == appID {
				vhostDefault = appID
			}
		}
	}

	var fragment string
	fragment += fmt.Sprintf(
		"frontend __gateway_http\n"+
			"  bind %v:%v\n"+
			"  mode http\n"+
			"  option dontlognull\n"+
			"  option forwardfor\n"+
			"  option http-server-close\n"+
			"  reqadd X-Forwarded-Proto:\\ http\n"+
			"\n",
		module.GatewayAddr,
		port)

	// write ACL statements
	fragment += strings.Join(exactMatches, "")
	fragment += strings.Join(suffixMatches, "")
	if len(exactMatches) != 0 || len(suffixMatches) != 0 {
		fragment += "\n"
	}

	for _, acl := range SortedStrStrKeys(exactRoutes) {
		appID := exactRoutes[acl]
		fragment += fmt.Sprintf("  use_backend %v if %v\n", appID, acl)
	}

	for _, acl := range SortedStrStrKeys(suffixRoutes) {
		appID := suffixRoutes[acl]
		fragment += fmt.Sprintf("  use_backend %v if %v\n", appID, acl)
	}

	fragment += "\n"

	if len(vhostDefault) != 0 {
		fragment += fmt.Sprintf("  default_backend %v\n\n", vhostDefault)
	}

	return fragment
}

func (module *HaproxyModule) makeGatewayHTTPS() string {
	// SNI vhost selector
	var (
		suffixRoutes  map[string]string = make(map[string]string)
		suffixMatches []string
		exactRoutes   map[string]string = make(map[string]string)
		exactMatches  []string
		vhostDefault  string
		port          uint = module.GatewayPortHTTPS
	)

	for _, appID := range SortedVhostsKeys(module.vhostsHTTPS) {
		vhosts := module.vhostsHTTPS[appID]
		for _, vhost := range vhosts {
			matchToken := "vhost_ssl_" + vhost
			matchToken = strings.Replace(matchToken, ".", "_", -1)
			matchToken = strings.Replace(matchToken, "*", "STAR", -1)

			if len(vhost) >= 3 && vhost[0] == '*' && vhost[1] == '.' {
				suffixMatches = append(suffixMatches,
					fmt.Sprintf("  acl %v req_ssl_sni -m dom %v\n", matchToken, strings.SplitN(vhost, ".", 2)[1]))
				suffixRoutes[matchToken] = appID
			} else {
				exactMatches = append(exactMatches,
					fmt.Sprintf("  acl %v req_ssl_sni -i %v\n", matchToken, vhost))
				exactRoutes[matchToken] = appID
			}

			if module.vhostDefaultHTTPS == appID {
				vhostDefault = appID
			}
		}
	}

	var fragment string
	fragment += fmt.Sprintf(
		"frontend __gateway_https\n"+
			"  bind %v:%v\n"+
			"  mode tcp\n"+
			"  tcp-request inspect-delay 5s\n"+
			"  tcp-request content accept if { req_ssl_hello_type 1 }\n"+
			"\n",
		module.GatewayAddr,
		port)

	// write ACL statements
	fragment += strings.Join(exactMatches, "")
	fragment += strings.Join(suffixMatches, "")
	if len(exactMatches) != 0 || len(suffixMatches) != 0 {
		fragment += "\n"
	}

	for _, acl := range SortedStrStrKeys(exactRoutes) {
		appID := exactRoutes[acl]
		fragment += fmt.Sprintf("  use_backend %v if %v\n", appID, acl)
	}

	for _, acl := range SortedStrStrKeys(suffixRoutes) {
		appID := suffixRoutes[acl]
		fragment += fmt.Sprintf("  use_backend %v if %v\n", appID, acl)
	}

	fragment += "\n"

	if len(vhostDefault) != 0 {
		fragment += fmt.Sprintf("  default_backend %v\n\n", vhostDefault)
	}

	return fragment
}

func (module *HaproxyModule) makeConfigTail() string {
	if len(module.ConfigTailPath) == 0 {
		return ""
	}

	tail, err := ioutil.ReadFile(module.ConfigTailPath)
	if err != nil {
		log.Printf("Failed to read config tail %v. %v", module.ConfigTailPath, err)
		return ""
	}

	return string(tail)
}

func (module *HaproxyModule) reloadConfig() error {
	if FileIsIdentical(module.ConfigPath, module.OldConfigPath) {
		log.Printf("[haproxy] config file not changed. ignoring reload\n")
		return nil
	}

	pidStr, err := ioutil.ReadFile(module.PidFile)
	if err != nil {
		return module.startProcess()
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidStr)))
	if err != nil {
		return err
	}

	err = syscall.Kill(pid, syscall.Signal(0))
	if err != nil {
		// process doesn't exist; start up process
		return module.startProcess()
	} else {
		// process does exist; send SIGHUP to reload
		return module.reloadProcess(pid)
	}
}

func (module *HaproxyModule) checkConfig(path string) error {
	return module.exec("checking configuration",
		"-f", path, "-p", module.PidFile, "-c")
}

func (module *HaproxyModule) startProcess() error {
	return module.exec("starting up process",
		"-f", module.ConfigPath, "-p", module.PidFile, "-D", "-q")
}

func (module *HaproxyModule) reloadProcess(pid int) error {
	return module.exec("reloading configuration",
		"-f", module.ConfigPath, "-p", module.PidFile, "-D", "-sf", fmt.Sprint(pid))
}

func (module *HaproxyModule) exec(logMessage string, args ...string) error {
	proc := exec.Command(module.HaproxyExe, args...)
	output, err := proc.CombinedOutput()

	log.Printf("[haproxy] %v: %v %v\n", logMessage, module.HaproxyExe, args)

	exitCode := proc.ProcessState.Sys().(syscall.WaitStatus)
	if exitCode != 0 {
		log.Printf("[haproxy] Bad exit code %v.\n", exitCode)
		err = ErrBadExit
	}

	if len(output) != 0 && module.Verbose {
		log.Println("[haproxy] command output:")
		log.Println(strings.TrimSpace(string(output)))
	}

	return err
}

func (module *HaproxyModule) getAppProtocol(app *core.AppCluster) string {
	if app.HealthCheck != nil {
		if app.HealthCheck.Protocol != "COMMAND" {
			return strings.ToUpper(app.HealthCheck.Protocol)
		}
	}

	return strings.ToUpper(app.Protocol)
}

func (module *HaproxyModule) supportsProtocol(proto string) bool {
	proto = strings.ToUpper(proto)

	if proto == "TCP" || proto == "HTTP" {
		return true
	} else {
		log.Printf("Haproxy: Protocol not supported: %v", proto)
		return false
	}
}

// {{{ SortedVhostsKeys
type sortedVhosts struct {
	m map[string][]string
	s []string
}

func (sm *sortedVhosts) Len() int {
	return len(sm.m)
}

func (sm *sortedVhosts) Less(a, b int) bool {
	// return sm.m[sm.s[a]] > sm.m[sm.s[b]]
	return sm.s[a] < sm.s[b]
}

func (sm *sortedVhosts) Swap(a, b int) {
	sm.s[a], sm.s[b] = sm.s[b], sm.s[a]
}

func SortedVhostsKeys(m map[string][]string) []string {
	sm := new(sortedVhosts)
	sm.m = m
	sm.s = make([]string, len(m))

	i := 0
	for key, _ := range m {
		sm.s[i] = key
		i++
	}
	sort.Sort(sm)

	return sm.s
}

// }}}
