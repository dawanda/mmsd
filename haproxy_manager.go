package main

// TODO: sort cluster names in cfg output
// TODO: sort backend names in cfg output
// TODO: avoid spam-reloading the haproxy binary (due to massive scaling)
// TODO: support local-health checks *or* marathon-based health check propagation (--local-health-checks=false)

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

	"github.com/dawanda/go-mesos/marathon"
)

type HaproxyMgr struct {
	Enabled            bool
	Verbose            bool
	LocalHealthChecks  bool
	FilterGroups       []string
	ServiceAddr        net.IP
	GatewayEnabled     bool
	GatewayAddr        net.IP
	GatewayPortHTTP    uint
	GatewayPortHTTPS   uint
	Executable         string
	ConfigPath         string
	ConfigTailPath     string
	OldConfigPath      string
	PidFile            string
	ManagementAddr     net.IP
	ManagementPort     uint
	AdminSockPath      string
	appConfigFragments map[string]string                    // [appId] = haproxy_config_fragment
	appLabels          map[string]map[string]string         // [appId][key] = value
	appStateCache      map[string]map[string]*marathon.Task // [appId][task] = Task
	vhosts             map[string][]string                  // [appId] = []vhost
	vhostDefault       string
	vhostsHTTPS        map[string][]string
	vhostDefaultHTTPS  string
}

const (
	LB_PROXY_PROTOCOL      = "lb-proxy-protocol"
	LB_ACCEPT_PROXY        = "lb-accept-proxy"
	LB_VHOST_HTTP          = "lb-vhost"
	LB_VHOST_DEFAULT_HTTP  = "lb-vhost-default"
	LB_VHOST_HTTPS         = "lb-vhost-ssl"
	LB_VHOST_DEFAULT_HTTPS = "lb-vhost-default-ssl"
)

var (
	ErrBadExit = errors.New("Bad Process Exit.")
)

func makeStringArray(s string) []string {
	if len(s) == 0 {
		return []string{}
	} else {
		return strings.Split(s, ",")
	}
}

func (manager *HaproxyMgr) Setup() error {
	return nil
}

func (manager *HaproxyMgr) IsEnabled() bool {
	return manager.Enabled
}

func (manager *HaproxyMgr) SetEnabled(value bool) {
	if value != manager.Enabled {
		manager.Enabled = value
	}
}

func (manager *HaproxyMgr) Apply(apps []*marathon.App, force bool) error {
	manager.appConfigFragments = make(map[string]string)
	manager.clearAppStateCache()

	manager.vhosts = make(map[string][]string)
	manager.vhostDefault = ""

	manager.vhostsHTTPS = make(map[string][]string)
	manager.vhostDefaultHTTPS = ""

	for _, app := range apps {
		config, err := manager.makeConfig(app)
		if err != nil {
			return err
		}
		manager.appConfigFragments[app.Id] = config
		for _, task := range app.Tasks {
			manager.setAppStateCacheEntry(&task)
		}
	}

	err := manager.writeConfig()
	if err != nil {
		return err
	}

	return manager.reloadConfig(false)
}

func (manager *HaproxyMgr) Remove(appID string, taskID string, app *marathon.App) error {
	if app != nil {
		// make sure we *remove* the task from the cluster
		config, err := manager.makeConfig(app)
		if err != nil {
			return err
		}
		if len(app.Tasks) > 0 {
			// app removed one task, still at least one alive
			manager.appConfigFragments[appID] = config
		} else {
			// app suspended (or scaled down to zero)
			delete(manager.appConfigFragments, appID)
		}
	} else {
		// app destroyed fully
		delete(manager.appConfigFragments, appID)
	}
	manager.removeAppStateCacheEntry(appID, taskID)

	err := manager.writeConfig()
	if err != nil {
		return err
	}

	return manager.reloadConfig(false)
}

func isAppJustSpawned(app *marathon.App) bool {
	// find out if an app has just been spawned by checking
	// if it ever failed already.

	if len(app.Tasks) == 0 {
		return false
	}

	for _, hsr := range app.Tasks[0].HealthCheckResults {
		if hsr.LastFailure != nil {
			return false
		}
	}

	return true
}

func (manager *HaproxyMgr) Update(app *marathon.App, taskID string) error {
	// collect list of task labels as we formatted them in haproxy.cfg.
	var instanceNames []string
	for portIndex, servicePort := range app.Ports {
		if GetTransportProtocol(app, portIndex) == "tcp" {
			appID := PrettifyAppId(app.Id, portIndex, servicePort)
			cachedTask := manager.getAppStateCacheEntry(app.Id, taskID)
			if cachedTask == nil {
				cachedTask = app.GetTaskById(taskID)
			}
			if cachedTask != nil {
				cachedTaskLabel := fmt.Sprintf("%v/%v:%v", appID, cachedTask.Host, cachedTask.Ports[portIndex])
				instanceNames = append(instanceNames, cachedTaskLabel)
			}
		}
	}

	config, err := manager.makeConfig(app)
	if err != nil {
		return err
	}

	manager.appConfigFragments[app.Id] = config
	for _, task := range app.Tasks {
		manager.setAppStateCacheEntry(&task)
	}

	err = manager.writeConfig()
	if err != nil {
		return err
	}

	task := app.GetTaskById(taskID)

	// go right away reload the config if that is the first start of the
	// underlying task and we got just health
	if task != nil && task.IsAlive() {
		// no health checks defined or app got just spawned the first time?
		if len(app.HealthChecks) == 0 || isAppJustSpawned(app) {
			log.Printf("[haproxy] App %v on host %v becomes healthy (or alive) first time. force reload config.\n",
				app.Id, task.Host)
			return manager.reloadConfig(true)
		}
	}

	// upstream server is already present, so send en enable or disable command
	// to all app clusters of this name ($app-$portIndex-$servicePort/$taskLabel)
	var updateCommandFmt string
	if task != nil && task.IsAlive() {
		updateCommandFmt = "enable server %v\n"
	} else {
		updateCommandFmt = "disable server %v\n"
	}

	for _, instanceName := range instanceNames {
		err := manager.sendCommandf(updateCommandFmt, instanceName)
		if err != nil {
			return err
		}
	}

	return nil
}

func (manager *HaproxyMgr) sendCommandf(cmdFmt string, args ...interface{}) error {
	log.Printf("[haproxy] "+cmdFmt, args...)
	cmd := fmt.Sprintf(cmdFmt, args...)
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{manager.AdminSockPath, "unix"})
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.Write([]byte(cmd))
	if err != nil {
		return err
	}

	var response []byte = make([]byte, 32768)
	_, err = conn.Read(response)
	if err != nil {
		return err
	}

	return nil
}

func (manager *HaproxyMgr) makeConfig(app *marathon.App) (string, error) {
	var result string

	// import application labels
	if manager.appLabels == nil {
		manager.appLabels = make(map[string]map[string]string)
	}
	manager.appLabels[app.Id] = make(map[string]string)
	for k, v := range app.Labels {
		manager.appLabels[app.Id][k] = v
	}

	for portIndex, portDef := range app.PortDefinitions {
		if manager.isGroupIncluded(portDef.Labels["lb-group"]) {
			result += manager.makeConfigForPort(app, portIndex)
		}
	}

	return result, nil
}

func (manager *HaproxyMgr) isGroupIncluded(groupName string) bool {
	for _, filterGroup := range manager.FilterGroups {
		if groupName == filterGroup || filterGroup == "*" {
			return true
		}
	}
	return false
}

func (manager *HaproxyMgr) makeConfigForPort(app *marathon.App, portIndex int) string {
	if GetTransportProtocol(app, portIndex) != "tcp" {
		return ""
	}

	var portDef = app.PortDefinitions[portIndex]
	var servicePort = portDef.Port
	var appID = PrettifyAppId(app.Id, portIndex, servicePort)
	var bindAddr = manager.ServiceAddr
	var healthCheck = GetHealthCheckForPortIndex(app.HealthChecks, portIndex)
	var appProtocol = GetApplicationProtocol(app, portIndex)

	var lbVirtualHosts = makeStringArray(portDef.Labels[LB_VHOST_HTTP])
	if len(lbVirtualHosts) != 0 {
		manager.vhosts[appID] = lbVirtualHosts
		if portDef.Labels[LB_VHOST_DEFAULT_HTTP] == "1" {
			manager.vhostDefault = appID
		}
	} else {
		delete(manager.vhosts, appID)
		if manager.vhostDefault == appID {
			manager.vhostDefault = ""
		}
	}

	lbVirtualHosts = makeStringArray(portDef.Labels[LB_VHOST_HTTPS])
	if len(lbVirtualHosts) != 0 {
		manager.vhostsHTTPS[appID] = lbVirtualHosts
		if portDef.Labels[LB_VHOST_DEFAULT_HTTPS] == "1" {
			manager.vhostDefaultHTTPS = appID
		}
	} else {
		delete(manager.vhostsHTTPS, appID)
		if manager.vhostDefaultHTTPS == appID {
			manager.vhostDefaultHTTPS = ""
		}
	}

	result := ""
	bindOpts := ""

	if runtime.GOOS == "linux" { // only enable on Linux (known to work)
		bindOpts += " defer-accept"
	}

	if Atoi(portDef.Labels[LB_ACCEPT_PROXY], 0) != 0 {
		bindOpts += " accept-proxy"
	}

	serverOpts := ""

	if manager.LocalHealthChecks {
		serverOpts += " check"
	}

	if healthCheck.IntervalSeconds > 0 {
		serverOpts += fmt.Sprintf(" inter %v", healthCheck.IntervalSeconds*1000)
	}

	switch Atoi(portDef.Labels[LB_PROXY_PROTOCOL], 0) {
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

	switch appProtocol {
	case "http":
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
				"  option abortonclose\n"+
				"  option httpchk GET %v HTTP/1.1\\r\\nHost:\\ %v\n",
			appID, bindAddr, servicePort, bindOpts, appID, appID,
			healthCheck.Path, "health-check")
	case "redis-master", "redis-server", "redis":
		result += fmt.Sprintf(
			"listen %v\n"+
				"  bind %v:%v%v\n"+
				"  option dontlognull\n"+
				"  mode tcp\n"+
				"  balance leastconn\n"+
				"  option tcp-check\n"+
				"  tcp-check connect\n"+
				"  tcp-check send PING\\r\\n\n"+
				"  tcp-check expect string +PONG\n"+
				"  tcp-check send info\\ replication\\r\\n\n"+
				"  tcp-check expect string role:master\n"+
				"  tcp-check send QUIT\\r\\n\n"+
				"  tcp-check expect string +OK\n",
			appID, bindAddr, servicePort, bindOpts)
	case "smtp":
		result += fmt.Sprintf(
			"listen %v\n"+
				"  bind %v:%v%v\n"+
				"  option dontlognull\n"+
				"  mode tcp\n"+
				"  balance leastconn\n"+
				"  option tcp-check\n"+
				"  option smtpchk EHLO localhost\n",
			appID, bindAddr, servicePort, bindOpts)
	default:
		result += fmt.Sprintf(
			"listen %v\n"+
				"  bind %v:%v%v\n"+
				"  option dontlognull\n"+
				"  mode tcp\n"+
				"  balance leastconn\n",
			appID, bindAddr, servicePort, bindOpts)
	}

	result += "  option redispatch\n"
	result += "  retries 1\n"

	for _, task := range sortTasks(app.Tasks, portIndex) {
		result += fmt.Sprintf(
			"  server %v:%v %v:%v%v\n",
			task.Host, task.Ports[portIndex], // taskLabel == "$host:$port"
			SoftResolveIPAddr(task.Host),
			task.Ports[portIndex],
			serverOpts)
	}

	result += "\n"

	return result
}

func (manager *HaproxyMgr) writeConfig() error {
	config, err := manager.makeConfigHead()
	if err != nil {
		return err
	}

	var clusterNames []string
	for name, _ := range manager.appConfigFragments {
		clusterNames = append(clusterNames, name)
	}
	sort.Strings(clusterNames)
	for _, name := range clusterNames {
		config += manager.appConfigFragments[name]
	}

	tail, err := manager.makeConfigTail()
	if err != nil {
		return err
	}
	config += tail

	tempConfigFile := fmt.Sprintf("%v.tmp", manager.ConfigPath)
	err = ioutil.WriteFile(tempConfigFile, []byte(config), 0666)
	if err != nil {
		return err
	}

	err = manager.checkConfig(tempConfigFile)
	if err != nil {
		return err
	}

	// if config file previousely did exist, attempt a rename
	if _, err := os.Stat(manager.ConfigPath); err == nil {
		if err = os.Rename(manager.ConfigPath, manager.OldConfigPath); err != nil {
			return err
		}
	}

	return os.Rename(tempConfigFile, manager.ConfigPath)
}

func (manager *HaproxyMgr) makeConfigHead() (string, error) {
	headerFragment := fmt.Sprintf(
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
			"\n", manager.AdminSockPath)

	mgntFragment := fmt.Sprintf(
		"listen haproxy\n"+
			"  bind %v:%v\n"+
			"  mode http\n"+
			"  stats enable\n"+
			"  stats uri /\n"+
			"  stats admin if TRUE\n"+
			"  monitor-uri /haproxy?monitor\n"+
			"\n",
		manager.ManagementAddr, manager.ManagementPort)

	if manager.GatewayEnabled {
		gatewayHTTP := manager.makeGatewayHTTP()
		gatewayHTTPS := manager.makeGatewayHTTPS()
		return headerFragment + mgntFragment + gatewayHTTP + gatewayHTTPS, nil
	} else {
		return headerFragment + mgntFragment, nil
	}
}

func (manager *HaproxyMgr) makeGatewayHTTP() string {
	var (
		suffixRoutes  map[string]string = make(map[string]string)
		suffixMatches []string
		exactRoutes   map[string]string = make(map[string]string)
		exactMatches  []string
		vhostDefault  string
		port          uint = manager.GatewayPortHTTP
	)

	for appID, vhosts := range manager.vhosts {
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

			if manager.vhostDefault == appID {
				vhostDefault = appID
			}
		}
	}

	var fragment string
	fragment += fmt.Sprintf(
		"frontend __gateway_http\n"+
			"  bind %v:%v\n"+
			"  mode http\n"+
			"  option httplog\n"+
			"  option dontlognull\n"+
			"  option forwardfor\n"+
			"  option http-server-close\n"+
			"  reqadd X-Forwarded-Proto:\\ http\n"+
			"\n",
		manager.GatewayAddr,
		port)

	// write ACL statements
	fragment += strings.Join(exactMatches, "")
	fragment += strings.Join(suffixMatches, "")
	if len(exactMatches) != 0 || len(suffixMatches) != 0 {
		fragment += "\n"
	}

	for acl, appID := range exactRoutes {
		fragment += fmt.Sprintf("  use_backend %v if %v\n", appID, acl)
	}

	for acl, appID := range suffixRoutes {
		fragment += fmt.Sprintf("  use_backend %v if %v\n", appID, acl)
	}

	fragment += "\n"

	if len(vhostDefault) != 0 {
		fragment += fmt.Sprintf("  default_backend %v\n\n", vhostDefault)
	}

	return fragment
}

func (manager *HaproxyMgr) makeGatewayHTTPS() string {
	// SNI vhost selector
	var (
		suffixRoutes  map[string]string = make(map[string]string)
		suffixMatches []string
		exactRoutes   map[string]string = make(map[string]string)
		exactMatches  []string
		vhostDefault  string
		port          uint = manager.GatewayPortHTTPS
	)

	for appID, vhosts := range manager.vhostsHTTPS {
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

			if manager.vhostDefaultHTTPS == appID {
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
		manager.GatewayAddr,
		port)

	// write ACL statements
	fragment += strings.Join(exactMatches, "")
	fragment += strings.Join(suffixMatches, "")
	if len(exactMatches) != 0 || len(suffixMatches) != 0 {
		fragment += "\n"
	}

	for acl, appID := range exactRoutes {
		fragment += fmt.Sprintf("  use_backend %v if %v\n", appID, acl)
	}

	for acl, appID := range suffixRoutes {
		fragment += fmt.Sprintf("  use_backend %v if %v\n", appID, acl)
	}

	fragment += "\n"

	if len(vhostDefault) != 0 {
		fragment += fmt.Sprintf("  default_backend %v\n\n", vhostDefault)
	}

	return fragment
}

func (manager *HaproxyMgr) makeConfigTail() (string, error) {
	if len(manager.ConfigTailPath) == 0 {
		return "", nil
	}

	tail, err := ioutil.ReadFile(manager.ConfigTailPath)
	if err != nil {
		return "", err
	}

	return string(tail), nil
}

func (manager *HaproxyMgr) reloadConfig(force bool) error {
	if !force && FileIsIdentical(manager.ConfigPath, manager.OldConfigPath) {
		log.Printf("[haproxy] config file not changed. ignoring reload\n")
		return nil
	}

	pidStr, err := ioutil.ReadFile(manager.PidFile)
	if err != nil {
		return manager.startProcess()
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidStr)))
	if err != nil {
		return err
	}

	err = syscall.Kill(pid, syscall.Signal(0))
	if err != nil {
		// process doesn't exist; start up process
		return manager.startProcess()
	} else {
		// process does exist; send SIGHUP to reload
		return manager.reloadProcess(pid)
	}
}

func (manager *HaproxyMgr) checkConfig(path string) error {
	return manager.exec("checking configuration",
		"-f", path, "-p", manager.PidFile, "-c")
}

func (manager *HaproxyMgr) startProcess() error {
	return manager.exec("starting up process",
		"-f", manager.ConfigPath, "-p", manager.PidFile, "-D", "-q")
}

func (manager *HaproxyMgr) reloadProcess(pid int) error {
	return manager.exec("reloading configuration",
		"-f", manager.ConfigPath, "-p", manager.PidFile, "-D", "-sf", fmt.Sprint(pid))
}

func (manager *HaproxyMgr) exec(logMessage string, args ...string) error {
	proc := exec.Command(manager.Executable, args...)
	output, err := proc.CombinedOutput()

	log.Printf("[haproxy] %v: %v %v\n", logMessage, manager.Executable, args)

	exitCode := proc.ProcessState.Sys().(syscall.WaitStatus)
	if exitCode != 0 {
		log.Printf("[haproxy] Bad exit code %v.\n", exitCode)
		err = ErrBadExit
	}

	if len(output) != 0 && manager.Verbose {
		log.Println("[haproxy] command output:")
		log.Println(strings.TrimSpace(string(output)))
	}

	return err
}

func (manager *HaproxyMgr) clearAppStateCache() {
	manager.appStateCache = make(map[string]map[string]*marathon.Task)
}

func (manager *HaproxyMgr) getAppStateCacheEntry(appID, taskID string) *marathon.Task {
	if app, ok := manager.appStateCache[appID]; ok {
		if task, ok := app[taskID]; ok {
			return task
		}
	}
	return nil
}

func (manager *HaproxyMgr) setAppStateCacheEntry(task *marathon.Task) {
	app, ok := manager.appStateCache[task.AppId]
	if !ok {
		app = make(map[string]*marathon.Task)
		manager.appStateCache[task.AppId] = app
	}
	app[task.Id] = task
}

func (manager *HaproxyMgr) removeAppStateCacheEntry(appID, taskID string) {
	if len(manager.appStateCache[appID]) != 0 {
		delete(manager.appStateCache[appID], taskID)

		if len(manager.appStateCache[appID]) == 0 {
			delete(manager.appStateCache, appID)
		}
	}
}

// {{{ SortedTaskList

type SortedTaskList struct {
	Tasks     []marathon.Task
	PortIndex int
}

func (tasks SortedTaskList) Len() int {
	return len(tasks.Tasks)
}

func (tasks SortedTaskList) Less(i, j int) bool {
	var a = &tasks.Tasks[i]
	var b = &tasks.Tasks[j]

	if len(a.Ports) < tasks.PortIndex || len(b.Ports) < tasks.PortIndex {
		// XXX That's a very case; when you redeploy your app with the port count
		// changed, you might run into here.
		return a.Host < b.Host
	}

	return a.Host < b.Host || a.Ports[tasks.PortIndex] < b.Ports[tasks.PortIndex]
}

func (tasks SortedTaskList) Swap(i, j int) {
	tmp := tasks.Tasks[i]
	tasks.Tasks[i] = tasks.Tasks[j]
	tasks.Tasks[j] = tmp
}

// }}}

func sortTasks(tasks []marathon.Task, portIndex int) []marathon.Task {
	stl := SortedTaskList{Tasks: tasks, PortIndex: portIndex}
	sort.Sort(stl)
	return stl.Tasks
}
