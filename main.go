package main

/* TODO:

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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/christianparpart/serviced/marathon"
	"github.com/gorilla/mux"
	flag "github.com/ogier/pflag"
)

type mmsdHandler interface {
	Apply(apps []*marathon.App, force bool) error
	Update(app *marathon.App, taskID string) error
	Remove(app *marathon.App, taskID string) error
	IsEnabled() bool
	SetEnabled(value bool)
}

type mmsdService struct {
	MarathonScheme   string
	MarathonIP       net.IP
	MarathonPort     uint
	ReconnectDelay   time.Duration
	RunStateDir      string
	FilterGroups     string
	GatewayEnabled   bool
	GatewayPortHTTP  uint
	GatewayPortHTTPS uint
	ManagedIP        net.IP
	FilesEnabled     bool
	UDPEnabled       bool
	TCPEnabled       bool
	HaproxyBin       string
	HaproxyTailCfg   string
	HaproxyPort      uint
	ServiceBind      net.IP
	ServicePort      uint
	Verbose          bool
	Handlers         []mmsdHandler
	quitChannel      chan bool
}

func (mmsd *mmsdService) setupHttpService() {
	router := mux.NewRouter()
	router.HandleFunc("/ping", mmsd.v0Ping)
	router.HandleFunc("/version", mmsd.v0Version)

	v1 := router.PathPrefix("/v1").Subrouter()
	v1.HandleFunc("/apps", mmsd.v1Apps).Methods("GET")
	v1.HandleFunc("/instances{app_id:/.*}", mmsd.v1Instances).Methods("GET")

	serviceAddr := fmt.Sprintf("%v:%v", mmsd.ServiceBind, mmsd.ServicePort)
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

	var portIndex int
	if sval := r.URL.Query().Get("portIndex"); len(sval) != 0 {
		i, err := strconv.Atoi(sval)
		if err == nil {
			portIndex = i
		}
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

	if portIndex >= len(app.Ports) {
		w.WriteHeader(http.StatusBadRequest)
		return
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
	if len(app.Ports) > portIndex {
		for _, task := range app.Tasks {
			list = append(list, fmt.Sprintf("%v:%v", resolveIPAddr(task.Host, noResolve), task.Ports[portIndex]))
		}
	} else {
		for _, task := range app.Tasks {
			list = append(list, fmt.Sprintf("%v\n", resolveIPAddr(task.Host, noResolve)))
		}
	}

	sort.Strings(list)

	for _, entry := range list {
		fmt.Fprintln(w, entry)
	}
}

func resolveIPAddr(dns string, skip bool) string {
	if skip {
		return dns
	} else {
		ip, err := net.ResolveIPAddr("ip", dns)
		if err != nil {
			return dns
		} else {
			return ip.String()
		}
	}
}

func (mmsd *mmsdService) setupEventBusListener() {
	var url = fmt.Sprintf("http://%v:%v/v2/events",
		mmsd.MarathonIP, mmsd.MarathonPort)

	var sse = NewEventSource(url, mmsd.ReconnectDelay)

	sse.OnOpen = func(event, data string) {
		log.Printf("Listening for events from Marathon on %v\n", url)
	}
	sse.OnError = func(event, data string) {
		log.Printf("Marathon Event Stream Error. %v. %v\n", event, data)
	}

	sse.AddEventListener("status_update_event", func(data string) {
		var event marathon.StatusUpdateEvent
		json.Unmarshal([]byte(data), &event)

		switch event.TaskStatus {
		case marathon.TaskRunning:
			app, err := mmsd.getMarathonApp(event.AppId)
			if err != nil {
				log.Printf("App %v task %v on %v is running but failed to fetch infos. %v\n",
					event.AppId, event.TaskId, event.Host, err)
				return
			}

			// XXX Only update propagate no health checks have been configured.
			// So we consider thie TASK_RUNNING state as healthy-notice.
			if len(app.HealthChecks) == 0 {
				log.Printf("App %v task %v on %v changed status. %v.\n", event.AppId, event.TaskId, event.Host, event.TaskStatus)
				mmsd.Update(event.AppId, event.TaskId, true)
			}
		case marathon.TaskFinished, marathon.TaskFailed, marathon.TaskKilled, marathon.TaskLost:
			log.Printf("App %v task %v on %v changed status. %v.\n", event.AppId, event.TaskId, event.Host, event.TaskStatus)

			app, err := mmsd.getMarathonApp(event.AppId)
			if err != nil {
				log.Printf("Failed to fetch Marathon app. %+v. %v\n", event, err)
				return
			}
			if app == nil {
				log.Printf("App %v not found anymore.\n", event.AppId)
				return
			}

			mmsd.Remove(app, event.TaskId)
		}
	})

	sse.AddEventListener("health_status_changed_event", func(data string) {
		var event marathon.HealthStatusChangedEvent
		json.Unmarshal([]byte(data), &event)
		app, err := mmsd.getMarathonApp(event.AppId)
		if err != nil {
			log.Printf("Failed to fetch Marathon app. %+v. %v\n", event, err)
			return
		}
		if app == nil {
			log.Printf("App %v not found anymore.\n", event.AppId)
			return
		}

		task := app.GetTaskById(event.TaskId)
		if task == nil {
			log.Printf("App %v task %v not found anymore.\n", event.AppId, event.TaskId)
			mmsd.Remove(app, event.TaskId)
			return
		}

		// app & task definitely do exist, so propagate health change event

		if event.Alive {
			log.Printf("App %v task %v on %v is healthy.\n", event.AppId, event.TaskId, task.Host)
		} else {
			log.Printf("App %v task %v on %v is unhealthy.\n", event.AppId, event.TaskId, task.Host)
		}

		mmsd.Update(event.AppId, event.TaskId, event.Alive)
	})

	go sse.RunForever()
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

// enable/disable given app:task
func (mmsd *mmsdService) Update(appID string, taskID string, alive bool) {
	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)
	if err != nil {
		log.Printf("Update: NewService(%q, %v) failed. %v\n", mmsd.MarathonIP, mmsd.MarathonPort, err)
		return
	}

	app, err := m.GetApp(appID)
	if err != nil {
		log.Printf("Update: GetApp(%q) failed. %v\n", appID, err)
		return
	}

	for _, handler := range mmsd.Handlers {
		if handler.IsEnabled() {
			err = handler.Update(app, taskID)
			if err != nil {
				log.Printf("Remove failed. %v\n", err)
			}
		}
	}
}

func (mmsd *mmsdService) Remove(app *marathon.App, taskID string) {
	for _, handler := range mmsd.Handlers {
		if handler.IsEnabled() {
			err := handler.Remove(app, taskID)
			if err != nil {
				log.Printf("Remove failed. %v\n", err)
			}
		}
	}
}

func (mmsd *mmsdService) MaybeResetFromTasks(force bool) error {
	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)
	if err != nil {
		return err
	}

	apps, err := m.GetApps()
	if err != nil {
		return err
	}

	for _, handler := range mmsd.Handlers {
		if handler.IsEnabled() {
			err = handler.Apply(apps, force)
			if err != nil {
				log.Printf("Failed to apply changes to handler. %v\n", err)
			}
		}
	}

	return nil
}

const appVersion = "0.1.0"
const appLicense = "MIT"

func showVersion() {
	fmt.Fprintf(os.Stderr, "mmsd - Mesos Marathon Service Discovery, version %v, licensed under %v\n", appVersion, appLicense)
	fmt.Fprintf(os.Stderr, "Written by Christian Parpart <christian@dawanda.com>\n")
}

func (mmsd *mmsdService) Run() {
	flag.BoolVarP(&mmsd.Verbose, "verbose", "v", mmsd.Verbose, "Set verbosity level")
	flag.IPVar(&mmsd.MarathonIP, "marathon-ip", mmsd.MarathonIP, "Marathon endpoint TCP IP address")
	flag.UintVar(&mmsd.MarathonPort, "marathon-port", mmsd.MarathonPort, "Marathon endpoint TCP port number")
	flag.DurationVar(&mmsd.ReconnectDelay, "reconnect-delay", mmsd.ReconnectDelay, "Marathon reconnect delay")
	flag.StringVar(&mmsd.RunStateDir, "run-state-dir", mmsd.RunStateDir, "Path to directory to keep run-state")
	flag.StringVar(&mmsd.FilterGroups, "filter-groups", mmsd.FilterGroups, "Application group filter")
	flag.IPVar(&mmsd.ManagedIP, "managed-ip", mmsd.ManagedIP, "IP-address to manage for mmsd")
	flag.BoolVar(&mmsd.GatewayEnabled, "enable-gateway", mmsd.GatewayEnabled, "Enables gateway support")
	flag.UintVar(&mmsd.GatewayPortHTTP, "gateway-http-port", mmsd.GatewayPortHTTP, "gateway HTTP port")
	flag.UintVar(&mmsd.GatewayPortHTTPS, "gateway-https-port", mmsd.GatewayPortHTTPS, "gateway HTTP port")
	flag.BoolVar(&mmsd.TCPEnabled, "enable-tcp", mmsd.TCPEnabled, "enables haproxy TCP load balancing")
	flag.BoolVar(&mmsd.FilesEnabled, "enable-files", mmsd.FilesEnabled, "enables file based service discovery")
	flag.BoolVar(&mmsd.UDPEnabled, "enable-udp", mmsd.UDPEnabled, "enables UDP load balancing")
	flag.StringVar(&mmsd.HaproxyBin, "haproxy-bin", mmsd.HaproxyBin, "path to haproxy binary")
	flag.StringVar(&mmsd.HaproxyTailCfg, "haproxy-cfgtail", mmsd.HaproxyTailCfg, "path to haproxy tail config file")
	flag.IPVar(&mmsd.ServiceBind, "haproxy-bind", mmsd.ServiceBind, "haproxy management port")
	flag.UintVar(&mmsd.HaproxyPort, "haproxy-port", mmsd.HaproxyPort, "haproxy management port")
	showVersionAndExit := flag.BoolP("version", "V", false, "Shows version and exits")

	flag.Usage = func() {
		showVersion()
		fmt.Fprintf(os.Stderr, "\nUsage: mmsd [flags ...]\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
	}

	flag.Parse()

	if *showVersionAndExit {
		showVersion()
		os.Exit(0)
	}

	mmsd.setupHandlers()
	mmsd.setupEventBusListener()
	mmsd.setupHttpService()

	<-mmsd.quitChannel
}

func (mmsd *mmsdService) setupHandlers() {
	mmsd.Handlers = []mmsdHandler{
		NewUdpManager(
			mmsd.ServiceBind,
			mmsd.Verbose,
			mmsd.UDPEnabled,
		),
		&HaproxyMgr{
			Enabled:        mmsd.TCPEnabled,
			Verbose:        mmsd.Verbose,
			Executable:     mmsd.HaproxyBin,
			ConfigTailPath: mmsd.HaproxyTailCfg,
			ConfigPath:     filepath.Join(mmsd.RunStateDir, "haproxy.cfg"),
			OldConfigPath:  filepath.Join(mmsd.RunStateDir, "haproxy.cfg.old"),
			PidFile:        filepath.Join(mmsd.RunStateDir, "haproxy.pid"),
			AdminSockPath:  filepath.Join(mmsd.RunStateDir, "haproxy.sock"),
			ManagementAddr: mmsd.ServiceBind,
			ManagementPort: mmsd.HaproxyPort,
		},
		&FilesManager{
			Enabled:  mmsd.FilesEnabled,
			Verbose:  mmsd.Verbose,
			BasePath: mmsd.RunStateDir + "/confd",
		},
	}

	// trigger initial run
	err := mmsd.MaybeResetFromTasks(true)
	if err != nil {
		log.Printf("Could not force task state reset. %v\n", err)
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
		MarathonScheme:   "http",
		MarathonIP:       net.ParseIP("127.0.0.1"),
		MarathonPort:     8080,
		ReconnectDelay:   time.Second * 4,
		RunStateDir:      "/var/run/mmsd",
		FilterGroups:     "*",
		GatewayEnabled:   false,
		GatewayPortHTTP:  80,
		GatewayPortHTTPS: 443,
		FilesEnabled:     true,
		UDPEnabled:       true,
		TCPEnabled:       true,
		HaproxyBin:       locateExe("haproxy"),
		HaproxyTailCfg:   "/etc/mmsd/haproxy-tail.cfg",
		HaproxyPort:      8081,
		ServiceBind:      net.ParseIP("0.0.0.0"),
		ServicePort:      8082,
		Verbose:          false,
		quitChannel:      make(chan bool),
	}

	mmsd.Run()
}
