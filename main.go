package main

/* TODO:

1. [x] serve HTTP /v1/instances/:app_id to retrieve ip:port pairs for given app
3. [x] implement UDP proxy (one-way/two-way, fanout (& roundrobin))
2. [x] implement upstream-conf.d file management
4. [ ] implement TCP proxy (with pluggable impls: haproxy, lvs, ...)
5. [ ] implement HTTP(S) gateway support
6. [ ] tcp-proxy: add `accept-proxy` support
7. [ ] tcp-proxy: add `proxy-protocol` support
8. [ ] logging: add readable up/down notices, such as:
	- "APP_ID: Task $TASK_STATUS on host $HOSTNAME ($TASK_ID)."
		with TASK_STATUS translating into readable ("killed", "finished", "running", ...)
	- "APP_ID: Task on $HOSTNAME:$PORT is unhealthy ($TASK_ID)."
	- "APP_ID: Task on $HOSTNAME:$PORT is healthy ($TASK_ID)."

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
	Update(app *marathon.App, task *marathon.Task) error
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
	v1 := router.PathPrefix("/v1").Subrouter()
	v1.HandleFunc("/apps", mmsd.v1Apps).Methods("GET")
	v1.HandleFunc("/instances{app_id:/.*}", mmsd.v1Instances).Methods("GET")

	serviceAddr := fmt.Sprintf("%v:%v", mmsd.ServiceBind, mmsd.ServicePort)
	log.Printf("Exposing service API on http://%v\n", serviceAddr)

	go http.ListenAndServe(serviceAddr, router)
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
	app_id := vars["app_id"]

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

	app, err := m.GetApp(app_id)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("GetApp error. %v\n", err)
		return
	}

	if app == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// appJson, err := json.MarshalIndent(app, "", " ")
	// w.Write(appJson)
	// fmt.Fprintf(w, "\n")
	// return

	for _, task := range app.Tasks {
		fmt.Fprintf(w, "%v:%v\n", task.Host, task.Ports[portIndex])
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

		var alive = event.TaskStatus == marathon.TaskRunning
		mmsd.Update(event.AppId, event.TaskId, alive)
	})

	sse.AddEventListener("health_status_changed_event", func(data string) {
		var event marathon.HealthStatusChangedEvent
		json.Unmarshal([]byte(data), &event)
		mmsd.Update(event.AppId, event.TaskId, event.Alive)
	})

	go sse.RunForever()
}

// enable/disable given app:task
func (mmsd *mmsdService) Update(appID string, taskID string, alive bool) {
	log.Printf("Update %v: %v (%v)\n", appID, taskID, alive)
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

	task := app.GetTaskById(taskID)

	for _, handler := range mmsd.Handlers {
		if handler.IsEnabled() {
			handler.Update(app, task)
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
			ConfigTailPath: mmsd.HaproxyTailCfg,
			ConfigPath:     filepath.Join(mmsd.RunStateDir, "haproxy.cfg"),
			PidFile:        filepath.Join(mmsd.RunStateDir, "haproxy.pid"),
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
		TCPEnabled:       false,
		HaproxyBin:       "/usr/bin/haproxy",
		HaproxyTailCfg:   "/etc/mmsd/haproxy-tail.cfg",
		HaproxyPort:      8081,
		ServiceBind:      net.ParseIP("0.0.0.0"),
		ServicePort:      8082,
		Verbose:          false,
		quitChannel:      make(chan bool),
	}

	mmsd.Run()
}
