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
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/christianparpart/serviced/marathon"
)

type HaproxyMgr struct {
	Enabled            bool
	Verbose            bool
	LocalHealthChecks  bool
	Executable         string
	ConfigPath         string
	ConfigTailPath     string
	OldConfigPath      string
	PidFile            string
	ManagementAddr     net.IP
	ManagementPort     uint
	AdminSockPath      string
	appConfigFragments map[string]string
	appStateCache      map[string]*marathon.App
}

var (
	ErrBadExit = errors.New("Bad Process Exit.")
)

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
	manager.appStateCache = make(map[string]*marathon.App)

	for _, app := range apps {
		config, err := manager.makeConfig(app)
		if err != nil {
			return err
		}
		manager.appConfigFragments[app.Id] = config
		manager.appStateCache[app.Id] = app
	}

	err := manager.updateConfig()
	if err != nil {
		return err
	}

	return manager.reloadConfig()
}

func (manager *HaproxyMgr) Remove(app *marathon.App, taskID string) error {
	// make sure we *remove* the task from the cluster
	config, err := manager.makeConfig(app)
	if err != nil {
		return err
	}

	manager.appConfigFragments[app.Id] = config
	manager.appStateCache[app.Id] = app

	err = manager.updateConfig()
	if err != nil {
		return err
	}

	return manager.reloadConfig()
}

func (manager *HaproxyMgr) Update(app *marathon.App, taskID string) error {
	// collect list of task labels as we formatted them in haproxy.cfg.
	var instanceNames []string
	for portIndex, portMapping := range app.Container.Docker.PortMappings {
		if portMapping.Protocol == "tcp" {
			servicePort := portMapping.ServicePort
			appID := PrettifyAppId(app.Id, portIndex, servicePort)
			cachedTask := manager.appStateCache[app.Id].GetTaskById(taskID)
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
	manager.appStateCache[app.Id] = app

	err = manager.updateConfig()
	if err != nil {
		return err
	}

	task := app.GetTaskById(taskID)

	// go right away reload the config if that is the first start of the
	// underlying task and we got just health
	if task != nil && task.IsAlive() {
		for _, hsr := range task.HealthCheckResults {
			if hsr.LastFailure == nil { // because we never had a failure before
				return manager.reloadConfig()
			}
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

	var lbAcceptProxy = Atoi(app.Labels["lb-accept-proxy"], 0)
	var lbProxyProtocol = Atoi(app.Labels["lb-proxy-protocol"], 0)

	for portIndex, portMapping := range app.Container.Docker.PortMappings {
		if portMapping.Protocol == "tcp" {
			servicePort := portMapping.ServicePort
			appID := PrettifyAppId(app.Id, portIndex, servicePort)
			bindAddr := manager.ManagementAddr
			healthCheck := getHealthCheckForPortIndex(app.HealthChecks, portIndex)

			appProtocol := app.Labels["proto"]
			if len(appProtocol) == 0 {
				appProtocol = healthCheck.Protocol

				if len(appProtocol) == 0 {
					appProtocol = portMapping.Protocol
				}
			}
			appProtocol = strings.ToLower(appProtocol)

			bindOpts := ""
			//TODO (only on linux) bindOpts += " defer-accept"
			if lbAcceptProxy != 0 {
				bindOpts += " accept-proxy"
			}

			serverOpts := ""

			if manager.LocalHealthChecks {
				serverOpts += " check"
			}

			if healthCheck.IntervalSeconds > 0 {
				serverOpts += fmt.Sprintf(" inter %v", healthCheck.IntervalSeconds*1000)
			}

			switch lbProxyProtocol {
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
						"  option httpchk GET %v HTTP/1.1\\r\\nHost:\\ %v\n",
					appID, bindAddr, servicePort, bindOpts, appID, appID,
					healthCheck.Path, "health-check")
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

			for _, task := range sortTasks(app.Tasks) {
				result += fmt.Sprintf(
					"  server %v:%v %v:%v%v\n",
					task.Host, task.Ports[portIndex], // taskLabel
					SoftResolveIPAddr(task.Host),
					task.Ports[portIndex],
					serverOpts)
			}

			result += "\n"
		}
	}

	return result, nil
}

func getHealthCheckForPortIndex(healthChecks []marathon.HealthCheck, portIndex int) marathon.HealthCheck {
	for _, hs := range healthChecks {
		if hs.PortIndex == portIndex {
			return hs
		}
	}

	return marathon.HealthCheck{}
}

func (manager *HaproxyMgr) updateConfig() error {
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
			"  stats socket %v mode 600 level admin\n"+
			"\n"+
			"defaults\n"+
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

	return headerFragment + mgntFragment, nil
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

func (manager *HaproxyMgr) reloadConfig() error {
	if FileIsIdentical(manager.ConfigPath, manager.OldConfigPath) {
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
	logStarted := false

	exitCode := proc.ProcessState.Sys().(syscall.WaitStatus)
	if exitCode != 0 {
		log.Printf("[haproxy] %v: %v %v\n", logMessage, manager.Executable, args)
		logStarted = true
		log.Printf("[haproxy] Bad exit code %v.\n", exitCode)
		err = ErrBadExit
	}

	if exitCode != 0 || (manager.Verbose && len(output) != 0) {
		if !logStarted {
			log.Printf("[haproxy] %v: %v %v\n", logMessage, manager.Executable, args)
		}
		log.Println("[haproxy] command output:")
		log.Println(strings.TrimSpace(string(output)))
	}

	return err
}

// {{{ SortedTaskList

type SortedTaskList []marathon.Task

func (tasks SortedTaskList) Len() int {
	return len(tasks)
}

func (tasks SortedTaskList) Less(i, j int) bool {
	return tasks[i].Id < tasks[j].Id
}

func (tasks SortedTaskList) Swap(i, j int) {
	tmp := tasks[i]
	tasks[i] = tasks[j]
	tasks[j] = tmp
}

// }}}

func sortTasks(tasks []marathon.Task) []marathon.Task {
	stl := SortedTaskList(tasks)
	sort.Sort(stl)
	return []marathon.Task(stl)
}
