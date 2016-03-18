package main

/* TODO:

1. [x] serve HTTP /v1/instances/:app_id to retrieve ip:port pairs for given app
3. [x] implement UDP proxy (one-way/two-way, fanout (& roundrobin))
2. [x] implement upstream-conf.d file management
4. [ ] implement TCP proxy (with pluggable impls: haproxy, lvs, ...)
5. [ ] implement HTTP(S) gateway support
6. [ ] tcp-proxy: add `accept-proxy` support
7. [ ] tcp-proxy: add `proxy-protocol` support

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

type MmsdHandler interface {
	Apply(apps []*marathon.App, force bool) error
	Update(app *marathon.App, task *marathon.Task) error
	IsEnabled() bool
	SetEnabled(value bool)
}

type MmsdService struct {
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
	UdpEnabled       bool
	TcpEnabled       bool
	HaproxyBin       string
	HaproxyTailCfg   string
	HaproxyPort      uint
	ServiceBind      net.IP
	ServicePort      uint
	Verbose          bool
	Handlers         []MmsdHandler
}

func (mmsd *MmsdService) v1_apps(w http.ResponseWriter, r *http.Request) {
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

func (mmsd *MmsdService) v1_instances(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]

	var portIndex int = 0
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

	app, err := m.GetApp(name)
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

func (mmsd *MmsdService) SetupEventBusListener() {
	var url string = fmt.Sprintf("http://%v:%v/v2/events",
		mmsd.MarathonIP, mmsd.MarathonPort)

	var sse *EventSource = NewEventSource(url, mmsd.ReconnectDelay)

	sse.OnOpen = func(event, data string) {
		log.Printf("Listening for events from Marathon on %v\n", url)
	}
	sse.OnError = func(event, data string) {
		log.Printf("Marathon Event Stream Error. %v. %v\n", event, data)
	}

	sse.AddEventListener("status_update_event", func(data string) {
		var event marathon.StatusUpdateEvent
		json.Unmarshal([]byte(data), &event)

		var alive bool = event.TaskStatus == marathon.TaskRunning
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
func (mmsd *MmsdService) Update(appId string, taskId string, alive bool) {
	// log.Printf("Update %v: %v (%v)\n", appId, taskId, alive)
	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)
	if err != nil {
		log.Printf("Update: NewService(%q, %v) failed. %v\n", mmsd.MarathonIP, mmsd.MarathonPort, err)
		return
	}

	app, err := m.GetApp(appId)
	if err != nil {
		log.Printf("Update: GetApp(%q) failed. %v\n", appId, err)
		return
	}

	task := app.GetTaskById(taskId)

	for _, handler := range mmsd.Handlers {
		if handler.IsEnabled() {
			handler.Update(app, task)
		}
	}
}

func (mmsd *MmsdService) MaybeResetFromTasks(force bool) error {
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

const AppVersion = "0.1.0"
const AppLicense = "MIT"

func (mmsd *MmsdService) Run() {
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
	flag.BoolVar(&mmsd.TcpEnabled, "enable-tcp", mmsd.TcpEnabled, "enables haproxy TCP load balancing")
	flag.BoolVar(&mmsd.FilesEnabled, "enable-files", mmsd.FilesEnabled, "enables file based service discovery")
	flag.BoolVar(&mmsd.UdpEnabled, "enable-udp", mmsd.UdpEnabled, "enables UDP load balancing")
	flag.StringVar(&mmsd.HaproxyBin, "haproxy-bin", mmsd.HaproxyBin, "path to haproxy binary")
	flag.StringVar(&mmsd.HaproxyTailCfg, "haproxy-cfgtail", mmsd.HaproxyTailCfg, "path to haproxy tail config file")
	flag.IPVar(&mmsd.ServiceBind, "haproxy-bind", mmsd.ServiceBind, "haproxy management port")
	flag.UintVar(&mmsd.HaproxyPort, "haproxy-port", mmsd.HaproxyPort, "haproxy management port")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "mmsd - Mesos Marathon Service Discovery, version %v, licensed under %v\n", AppVersion, AppLicense)
		fmt.Fprintf(os.Stderr, "Written by Christian Parpart <christian@dawanda.com>\n\n")
		fmt.Fprintf(os.Stderr, "Usage: mmsd [flags ...]\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
	}

	flag.Parse()

	mmsd.SetupHandlers()
	mmsd.SetupEventBusListener()

	// HTTP service
	router := mux.NewRouter()
	v1 := router.PathPrefix("/v1").Subrouter()
	v1.HandleFunc("/apps", mmsd.v1_apps).Methods("GET")
	v1.HandleFunc("/instances{name:/.*}", mmsd.v1_instances).Methods("GET")

	serviceAddr := fmt.Sprintf("%v:%v", mmsd.ServiceBind, mmsd.ServicePort)
	log.Printf("Exposing service API on http://%v\n", serviceAddr)

	http.ListenAndServe(serviceAddr, router)
}

func (mmsd *MmsdService) SetupHandlers() {
	mmsd.Handlers = []MmsdHandler{
		NewUdpManager(
			mmsd.ServiceBind,
			mmsd.Verbose,
			mmsd.UdpEnabled,
		),
		&HaproxyMgr{
			Enabled:        mmsd.TcpEnabled,
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
	var mmsd MmsdService = MmsdService{
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
		UdpEnabled:       true,
		TcpEnabled:       false,
		HaproxyBin:       "/usr/bin/haproxy",
		HaproxyTailCfg:   "/etc/mmsd/haproxy-tail.cfg",
		HaproxyPort:      8081,
		ServiceBind:      net.ParseIP("0.0.0.0"),
		ServicePort:      8082,
		Verbose:          false,
	}

	mmsd.Run()
}
