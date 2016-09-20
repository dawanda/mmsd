package main

/* TODO:

0. [ ] PrettifyAppId should not need portIndex

1. [x] serve HTTP /v1/instances/:app_id to retrieve ip:port pairs for given app
2. [x] implement UDP proxy (one-way/two-way, fanout (& roundrobin))
3. [x] implement upstream-conf.d file management
4. [x] logging: add readable up/down notices, such as:
5. [ ] implement TCP proxy (with pluggable impls: haproxy, lvs, ...)
7. [ ] tcp-proxy: add `accept-proxy` support
8. [ ] tcp-proxy: add `proxy-protocol` support
9. [ ] implement HTTP(S) gateway support

XXX Changes:

* `--marathon-host` is now named `--marathon-ip` and only accepts IP addresses
* `--reconnect-delay` added
* `--haproxy-cfg` removed
* `--gateway` renamed to `--enable-gateway`
* `--enable-files` added
* `--enable-tcp` added
* `--enable-udp` added
* also exposes service discovery API via HTTP endpoint

*/

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/dawanda/go-mesos/marathon"
	"github.com/dawanda/mmsd/module_api"
	"github.com/dawanda/mmsd/modules"
	"github.com/gorilla/mux"
	flag "github.com/ogier/pflag"
)

type mmsdService struct {
	HttpApiPort       uint
	Verbose           bool
	Handlers          []EventListener
	quitChannel       chan bool
	RunStateDir       string
	FilterGroups      []string
	LocalHealthChecks bool
	ManagementAddr    net.IP

	// common application service discovery configuration
	ServiceAddr net.IP

	// service discovery IP-management
	ManagedIP net.IP

	// file based service discovery
	FilesEnabled bool

	// marathon endpoint configuration
	MarathonScheme string
	MarathonIP     net.IP
	MarathonPort   uint
	ReconnectDelay time.Duration // martahon event stream reconnect delay

	// gateway configuration
	GatewayEnabled   bool
	GatewayAddr      net.IP
	GatewayPortHTTP  uint
	GatewayPortHTTPS uint

	// tcp load balancing (haproxy)
	TCPEnabled     bool
	HaproxyBin     string
	HaproxyTailCfg string
	HaproxyPort    uint

	// udp load balancing
	UDPEnabled bool

	// DNS service discovery
	DNSEnabled  bool
	DNSPort     uint
	DNSBaseName string
	DNSTTL      time.Duration
	DNSPushSRV  bool

	// runtime state
	apps         []*module_api.AppCluster
	killingTasks map[string]bool // set of tasks currently in killing state
}

// {{{ HTTP endpoint
func (mmsd *mmsdService) setupHttpService() {
	router := mux.NewRouter()
	router.HandleFunc("/ping", mmsd.v0Ping)
	router.HandleFunc("/version", mmsd.v0Version)

	v1 := router.PathPrefix("/v1").Subrouter()
	v1.HandleFunc("/apps", mmsd.v1Apps).Methods("GET")
	v1.HandleFunc("/instances{app_id:/.*}", mmsd.v1Instances).Methods("GET")

	serviceAddr := fmt.Sprintf("%v:%v", mmsd.ServiceAddr, mmsd.HttpApiPort)
	log.Printf("Exposing service API on http://%v\n", serviceAddr)

	go http.ListenAndServe(serviceAddr, router)
}

func (mmsd *mmsdService) v0Ping(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "pong\n")
}

func (mmsd *mmsdService) v0Version(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "mmsd %v\n", appVersion)
}

func (mmsd *mmsdService) v1Apps(w http.ResponseWriter, r *http.Request) {
	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("NewService error. %v\n", err)
		return
	}

	apps, err := m.GetApps()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("GetApps error. %v\n", err)
		return
	}

	var appList []string
	for _, app := range apps {
		appList = append(appList, app.Id)
	}
	sort.Strings(appList)

	fmt.Fprintf(w, "%s\n", strings.Join(appList, "\n"))
}

func (mmsd *mmsdService) v1Instances(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	appID := vars["app_id"]
	noResolve := r.URL.Query().Get("noresolve") == "1"
	withServerID := r.URL.Query().Get("withid") == "1"

	portBegin, portEnd, err := parseRange(r.URL.Query().Get("portIndex"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Printf("error parsing range. %v\n", err)
		return
	}

	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("NewService error. %v\n", err)
		return
	}

	app, err := m.GetApp(appID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("GetApp error. %v\n", err)
		return
	}

	if app == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	log.Printf("parseRange: %v .. %v (%v)\n", portBegin, portEnd, len(app.Ports))

	if portEnd >= len(app.Ports) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if portEnd < 0 {
		portEnd = len(app.Ports) - 1
	}

	if len(app.Tasks) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// appJson, err := json.MarshalIndent(app, "", " ")
	// w.Write(appJson)
	// fmt.Fprintf(w, "\n")
	// return

	var list []string
	for _, task := range app.Tasks {
		item := ""
		if withServerID {
			item += fmt.Sprintf("%v:", Hash(task.SlaveId))
		}

		item += resolveIPAddr(task.Host, noResolve)

		for portIndex := portBegin; portIndex <= portEnd; portIndex++ {
			item += fmt.Sprintf(":%d", task.Ports[portIndex])
		}

		list = append(list, item)
	}

	sort.Strings(list)

	for _, entry := range list {
		fmt.Fprintln(w, entry)
	}
}

// }}}

func (mmsd *mmsdService) isGroupIncluded(groupName string) bool {
	for _, filterGroup := range mmsd.FilterGroups {
		if groupName == filterGroup || filterGroup == "*" {
			return true
		}
	}
	return false
}

func (mmsd *mmsdService) getMarathonApps() []marathon.App {
	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)
	if err != nil {
		log.Printf("Failed to get marathon.Service: %v\n", err)
		return nil
	}

	mApps, err := m.GetApps()
	var mApps2 []marathon.App
	for _, mApp := range mApps {
		mApps2 = append(mApps2, *mApp)
	}

	return mApps2
}

func (mmsd *mmsdService) getMarathonApp(appID string) (*marathon.App, error) {
	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)
	if err != nil {
		return nil, err
	}

	app, err := m.GetApp(appID)
	if err != nil {
		return nil, err
	}

	return app, nil
}

// convertMarathonApps converts an array of marathon.App into a []AppCluster.
func (mmsd *mmsdService) convertMarathonApps(mApps []marathon.App) []*module_api.AppCluster {
	var apps []*module_api.AppCluster
	for _, mApp := range mApps {
		for portIndex, portDef := range mApp.PortDefinitions {
			if mmsd.isGroupIncluded(portDef.Labels["lb-group"]) {
				var healthCheck *module_api.AppHealthCheck
				if mHealthCheck := FindHealthCheckForPortIndex(mApp.HealthChecks, portIndex); mHealthCheck != nil {
					var mCommand *string
					if mHealthCheck.Command != nil {
						mCommand = new(string)
						*mCommand = mHealthCheck.Command.Value
					}
					healthCheck = &module_api.AppHealthCheck{
						Protocol:               mHealthCheck.Protocol,
						Path:                   mHealthCheck.Path,
						Command:                mCommand,
						GracePeriodSeconds:     mHealthCheck.GracePeriodSeconds,
						IntervalSeconds:        mHealthCheck.IntervalSeconds,
						TimeoutSeconds:         mHealthCheck.TimeoutSeconds,
						MaxConsecutiveFailures: mHealthCheck.MaxConsecutiveFailures,
						IgnoreHttp1xx:          mHealthCheck.IgnoreHttp1xx,
					}
				}

				var backends []module_api.AppBackend
				for _, mTask := range mApp.Tasks {
					backends = append(backends, module_api.AppBackend{
						Id:    mTask.Id,
						Host:  mTask.Host,
						Port:  mTask.Ports[portIndex],
						State: string(*mTask.State),
					})
				}
				// TODO: sort backends ASC

				labels := make(map[string]string)
				for k, v := range mApp.Labels {
					labels[k] = v
				}
				for k, v := range mApp.PortDefinitions[portIndex].Labels {
					labels[k] = v
				}

				servicePort := mApp.PortDefinitions[portIndex].Port
				app := &module_api.AppCluster{
					Name:        mApp.Id,
					Id:          PrettifyAppId(mApp.Id, portIndex, servicePort),
					ServicePort: servicePort,
					Protocol:    mApp.PortDefinitions[portIndex].Protocol,
					PortName:    mApp.PortDefinitions[portIndex].Name,
					Labels:      labels,
					HealthCheck: healthCheck,
					Backends:    backends,
					PortIndex:   portIndex,
				}
				apps = append(apps, app)
			}
		}
	}
	return apps
}

func (mmsd *mmsdService) getAppByMarathonId(appId string, portIndex int) *module_api.AppCluster {
	log.Printf("Application %v with port index %v not found.",
		appId, portIndex)
	return nil
}

// findAppsByMarathonId returns list of all applications that belong to the
// given Marathon App mAppId.
func (mmsd *mmsdService) findAppsByMarathonId(mAppId string) []*module_api.AppCluster {
	var apps []*module_api.AppCluster
	for _, app := range mmsd.apps {
		if app.Name == mAppId {
			apps = append(apps, app)
		}
	}
	return apps
}

func (mmsd *mmsdService) setupEventBusListener() {
	var url = fmt.Sprintf("http://%v:%v/v2/events",
		mmsd.MarathonIP, mmsd.MarathonPort)

	var sse = NewEventSource(url, mmsd.ReconnectDelay)

	sse.OnOpen = mmsd.OnMarathonConnected
	sse.OnError = mmsd.OnMarathonConnectionFailure
	//sse.AddEventListener("deployment_info", mmsd.DeploymentStart)
	sse.AddEventListener("status_update_event", mmsd.StatusUpdateEvent)
	sse.AddEventListener("health_status_changed_event", mmsd.HealthStatusChangedEvent)

	go sse.RunForever()
}

func (mmsd *mmsdService) OnMarathonConnected(event, data string) {
	log.Printf("Listening for events from Marathon on %v:%v\n", mmsd.MarathonIP, mmsd.MarathonPort)
	mmsd.applyApps(mmsd.convertMarathonApps(mmsd.getMarathonApps()))
}

func (mmsd *mmsdService) OnMarathonConnectionFailure(event, data string) {
	log.Printf("Marathon Event Stream Error. %v. %v\n", event, data)
}

// StatusUpdateEvent is invoked by SSE when exactly this named event is fired.
func (mmsd *mmsdService) StatusUpdateEvent(data string) {
	var event marathon.StatusUpdateEvent
	err := json.Unmarshal([]byte(data), &event)
	if err != nil {
		log.Printf("Failed to unmarshal status_update_event. %v\n", err)
		log.Printf("status_update_event: %+v\n", data)
		return
	}

	switch event.TaskStatus {
	case marathon.TaskRunning:
		app, err := mmsd.getMarathonApp(event.AppId)
		if err != nil {
			log.Printf("App %v task %v on %v is running but failed to fetch infos. %v\n",
				event.AppId, event.TaskId, event.Host, err)
			return
		}

		log.Printf(
			"App %v task %v on %v changed status. %v. %v\n",
			event.AppId, event.TaskId, event.Host, event.TaskStatus, event.Message)

		// XXX Only update propagate no health checks have been configured.
		// So we consider thie TASK_RUNNING state as healthy-notice.
		if len(app.HealthChecks) == 0 {
			mmsd.AddTask(
				event.AppId,
				event.TaskId,
				string(event.TaskStatus),
				event.Host,
				event.Ports)
		} else {
			// TODO: add to mmsd.unhealthyTasks[] to remember host:port mappings
			// for the moment when this task becomes live
		}
	case marathon.TaskKilling:
		log.Printf("App %v task %v on %v changed status. %v.\n", event.AppId, event.TaskId, event.Host, event.TaskStatus)
		mmsd.killingTasks[event.TaskId] = true
		mmsd.RemoveTask(event.AppId, event.TaskId, event.TaskStatus)
	case marathon.TaskFinished, marathon.TaskFailed, marathon.TaskKilled, marathon.TaskLost:
		log.Printf("App %v task %v on %v changed status. %v.\n", event.AppId, event.TaskId, event.Host, event.TaskStatus)
		if !mmsd.killingTasks[event.TaskId] {
			mmsd.RemoveTask(event.AppId, event.TaskId, event.TaskStatus)
		} else {
			delete(mmsd.killingTasks, event.TaskId)
		}
	}
}

// HealthStatusChangedEvent is invoked by SSE when exactly this named event is fired.
func (mmsd *mmsdService) HealthStatusChangedEvent(data string) {
	var event marathon.HealthStatusChangedEvent
	err := json.Unmarshal([]byte(data), &event)
	if err != nil {
		log.Printf("Failed to unmarshal health_status_changed_event. %v\n", err)
	} else if event.Alive {
		// TODO mmsd.AddTask(event.AppId, event.TaskId)
	} else {
		mmsd.RemoveTask(event.AppId, event.TaskId, "TASK_RUNNING")
	}
}

// AddTask ensures the given task is added to the app and all handlers are
// notified as well.
func (mmsd *mmsdService) AddTask(appId, taskId, taskStatus, host string, ports []uint) {
	for portIndex, port := range ports {
		if app := mmsd.getAppByMarathonId(appId, portIndex); app != nil {
			task := module_api.AppBackend{
				Id:    taskId,
				Host:  host,
				Port:  port,
				State: string(taskStatus),
			}
			app.Backends = append(app.Backends, task)

			for _, handler := range mmsd.Handlers {
				handler.AddTask(&task, app)
			}
		}
	}
}

func (mmsd *mmsdService) RemoveTask(appId, taskId string, newStatus marathon.TaskStatus) bool {
	var found uint
	for _, app := range mmsd.findAppsByMarathonId(appId) {
		for i, task := range app.Backends {
			if task.Id == taskId {
				// update task state, and remove task out of app cluster's task list
				task.State = string(newStatus)
				app.Backends = append(app.Backends[:0], app.Backends[i+1:]...)

				for _, handler := range mmsd.Handlers {
					handler.RemoveTask(&task, app)
				}
				found++
			}
		}
	}
	return found > 0
}

const appVersion = "0.9.12"
const appLicense = "MIT"

func showVersion() {
	fmt.Fprintf(os.Stderr, "mmsd - Mesos Marathon Service Discovery, version %v, licensed under %v\n", appVersion, appLicense)
	fmt.Fprintf(os.Stderr, "Written by Christian Parpart <christian@dawanda.com>\n")
}

func (mmsd *mmsdService) Run() {
	var filterGroups = "*"
	flag.BoolVarP(&mmsd.Verbose, "verbose", "v", mmsd.Verbose, "Set verbosity level")
	flag.IPVar(&mmsd.MarathonIP, "marathon-ip", mmsd.MarathonIP, "Marathon endpoint TCP IP address")
	flag.UintVar(&mmsd.MarathonPort, "marathon-port", mmsd.MarathonPort, "Marathon endpoint TCP port number")
	flag.DurationVar(&mmsd.ReconnectDelay, "reconnect-delay", mmsd.ReconnectDelay, "Marathon reconnect delay")
	flag.StringVar(&mmsd.RunStateDir, "run-state-dir", mmsd.RunStateDir, "Path to directory to keep run-state")
	flag.StringVar(&filterGroups, "filter-groups", filterGroups, "Application group filter")
	flag.IPVar(&mmsd.ManagedIP, "managed-ip", mmsd.ManagedIP, "IP-address to manage for mmsd")
	flag.BoolVar(&mmsd.GatewayEnabled, "enable-gateway", mmsd.GatewayEnabled, "Enables gateway support")
	flag.IPVar(&mmsd.GatewayAddr, "gateway-bind", mmsd.GatewayAddr, "gateway bind address")
	flag.UintVar(&mmsd.GatewayPortHTTP, "gateway-port-http", mmsd.GatewayPortHTTP, "gateway port for HTTP")
	flag.UintVar(&mmsd.GatewayPortHTTPS, "gateway-port-https", mmsd.GatewayPortHTTPS, "gateway port for HTTPS")
	flag.BoolVar(&mmsd.FilesEnabled, "enable-files", mmsd.FilesEnabled, "enables file based service discovery")
	flag.BoolVar(&mmsd.UDPEnabled, "enable-udp", mmsd.UDPEnabled, "enables UDP load balancing")
	flag.BoolVar(&mmsd.TCPEnabled, "enable-tcp", mmsd.TCPEnabled, "enables haproxy TCP load balancing")
	flag.BoolVar(&mmsd.LocalHealthChecks, "enable-health-checks", mmsd.LocalHealthChecks, "Enable local health checks (if available) instead of relying on Marathon health checks alone.")
	flag.StringVar(&mmsd.HaproxyBin, "haproxy-bin", mmsd.HaproxyBin, "path to haproxy binary")
	flag.StringVar(&mmsd.HaproxyTailCfg, "haproxy-cfgtail", mmsd.HaproxyTailCfg, "path to haproxy tail config file")
	flag.IPVar(&mmsd.ServiceAddr, "haproxy-bind", mmsd.ServiceAddr, "haproxy management port")
	flag.UintVar(&mmsd.HaproxyPort, "haproxy-port", mmsd.HaproxyPort, "haproxy management port")
	flag.BoolVar(&mmsd.DNSEnabled, "enable-dns", mmsd.DNSEnabled, "Enables DNS-based service discovery")
	flag.UintVar(&mmsd.DNSPort, "dns-port", mmsd.DNSPort, "DNS service discovery port")
	flag.BoolVar(&mmsd.DNSPushSRV, "dns-push-srv", mmsd.DNSPushSRV, "DNS service discovery to also push SRV on A")
	flag.StringVar(&mmsd.DNSBaseName, "dns-basename", mmsd.DNSBaseName, "DNS service discovery's base name")
	flag.DurationVar(&mmsd.DNSTTL, "dns-ttl", mmsd.DNSTTL, "DNS service discovery's reply message TTL")
	showVersionAndExit := flag.BoolP("version", "V", false, "Shows version and exits")

	flag.Usage = func() {
		showVersion()
		fmt.Fprintf(os.Stderr, "\nUsage: mmsd [flags ...]\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
	}

	flag.Parse()

	mmsd.FilterGroups = strings.Split(filterGroups, ",")

	if *showVersionAndExit {
		showVersion()
		os.Exit(0)
	}

	mmsd.setupHandlers()
	mmsd.setupEventBusListener()
	mmsd.setupHttpService()

	<-mmsd.quitChannel

	for _, handler := range mmsd.Handlers {
		handler.Shutdown()
	}
}

func (mmsd *mmsdService) Shutdown() {
	mmsd.quitChannel <- true
}

func (mmsd *mmsdService) applyApps(apps []*module_api.AppCluster) {
	mmsd.apps = apps

	for _, handler := range mmsd.Handlers {
		handler.Apply(apps)
	}
}

func (mmsd *mmsdService) setupHandlers() {
	// mmsd.Handlers = append(mmsd.Handlers, &EventLoggerModule{
	// 	Verbose: true,
	// })

	if mmsd.DNSEnabled {
		mmsd.Handlers = append(mmsd.Handlers, &modules.DNSModule{
			Verbose:     mmsd.Verbose,
			ServiceAddr: mmsd.ServiceAddr,
			ServicePort: mmsd.DNSPort,
			PushSRV:     mmsd.DNSPushSRV,
			BaseName:    mmsd.DNSBaseName,
			DNSTTL:      mmsd.DNSTTL,
		})
	}

	// if mmsd.UDPEnabled {
	// 	mmsd.Handlers = append(mmsd.Handlers, NewUdpManager(
	// 		mmsd.ServiceAddr,
	// 		mmsd.Verbose,
	// 		mmsd.UDPEnabled,
	// 	))
	// }

	if mmsd.TCPEnabled {
		mmsd.Handlers = append(mmsd.Handlers, &HaproxyModule{
			Verbose:           mmsd.Verbose,
			LocalHealthChecks: mmsd.LocalHealthChecks,
			ServiceAddr:       mmsd.ServiceAddr,
			GatewayEnabled:    mmsd.GatewayEnabled,
			GatewayAddr:       mmsd.GatewayAddr,
			GatewayPortHTTP:   mmsd.GatewayPortHTTP,
			GatewayPortHTTPS:  mmsd.GatewayPortHTTPS,
			HaproxyExe:        mmsd.HaproxyBin,
			ConfigTailPath:    mmsd.HaproxyTailCfg,
			ConfigPath:        filepath.Join(mmsd.RunStateDir, "haproxy.cfg"),
			OldConfigPath:     filepath.Join(mmsd.RunStateDir, "haproxy.cfg.old"),
			PidFile:           filepath.Join(mmsd.RunStateDir, "haproxy.pid"),
			AdminSockPath:     filepath.Join(mmsd.RunStateDir, "haproxy.sock"),
			ManagementAddr:    mmsd.ManagementAddr,
			ManagementPort:    mmsd.HaproxyPort,
		})
	}

	if mmsd.FilesEnabled {
		mmsd.Handlers = append(mmsd.Handlers, &FilesManager{
			Verbose:  mmsd.Verbose,
			BasePath: mmsd.RunStateDir + "/confd",
		})
	}

	for _, handler := range mmsd.Handlers {
		handler.Startup()
	}
}

func locateExe(name string) string {
	for _, prefix := range strings.Split(os.Getenv("PATH"), ":") {
		path := filepath.Join(prefix, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return name // default to name only
}

func main() {
	var mmsd = mmsdService{
		MarathonScheme:    "http",
		MarathonIP:        net.ParseIP("127.0.0.1"),
		MarathonPort:      8080,
		ReconnectDelay:    time.Second * 4,
		RunStateDir:       "/var/run/mmsd",
		GatewayEnabled:    false,
		GatewayAddr:       net.ParseIP("0.0.0.0"),
		GatewayPortHTTP:   80,
		GatewayPortHTTPS:  443,
		FilesEnabled:      false,
		UDPEnabled:        false,
		TCPEnabled:        false,
		LocalHealthChecks: true,
		HaproxyBin:        locateExe("haproxy"),
		HaproxyTailCfg:    "/etc/mmsd/haproxy-tail.cfg",
		HaproxyPort:       8081,
		ManagementAddr:    net.ParseIP("0.0.0.0"),
		ServiceAddr:       net.ParseIP("0.0.0.0"),
		HttpApiPort:       8082,
		Verbose:           false,
		DNSEnabled:        false,
		DNSPort:           53,
		DNSPushSRV:        false,
		DNSBaseName:       "mmsd.",
		DNSTTL:            time.Second * 5,
		quitChannel:       make(chan bool),
		killingTasks:      make(map[string]bool),
	}

	// trap SIGTERM and SIGINT
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigc
		log.Printf("Caught signal %v. Terminating", s)
		mmsd.Shutdown()
	}()

	mmsd.Run()
}
