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
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/christianparpart/go-marathon/marathon"
	"github.com/gorilla/mux"
	flag "github.com/ogier/pflag"
)

type mmsdHandler interface {
	Setup() error
	Apply(apps []*marathon.App, force bool) error
	Update(app *marathon.App, taskID string) error
	Remove(appID string, taskID string, app *marathon.App) error
}

type mmsdService struct {
	HttpApiPort       uint
	Verbose           bool
	Handlers          []mmsdHandler
	quitChannel       chan bool
	RunStateDir       string
	FilterGroups      string
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
	DnsEnabled  bool
	DnsPort     uint
	DnsBaseName string
	DnsTTL      time.Duration
	DnsPushSRV  bool
}

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

var ErrInvalidPortRange = errors.New("Invalid port range")

func parseRange(input string) (int, int, error) {
	if len(input) == 0 {
		return 0, 0, nil
	}

	vals := strings.Split(input, ":")
	log.Printf("vals: %+q\n", vals)

	if len(vals) == 1 {
		i, err := strconv.Atoi(input)
		return i, i, err
	}

	if len(vals) > 2 {
		return 0, 0, ErrInvalidPortRange
	}

	var (
		begin int
		end   int
		err   error
	)

	// parse begin
	if vals[0] != "" {
		begin, err = strconv.Atoi(vals[0])
		if err != nil {
			return begin, end, err
		}
	}

	// parse end
	if vals[1] != "" {
		end, err = strconv.Atoi(vals[1])
		if begin > end {
			return begin, end, ErrInvalidPortRange
		}
	} else {
		end = -1 // XXX that is: until the end
	}

	return begin, end, err
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
		err := json.Unmarshal([]byte(data), &event)
		if err != nil {
			log.Printf("Failed to unmarshal status_update_event. %v\n", err)
			log.Printf("status_update_event: %+v\n", data)
		} else {
			mmsd.statusUpdateEvent(&event)
		}
	})

	sse.AddEventListener("health_status_changed_event", func(data string) {
		var event marathon.HealthStatusChangedEvent
		err := json.Unmarshal([]byte(data), &event)
		if err != nil {
			log.Printf("Failed to unmarshal health_status_changed_event. %v\n", err)
		} else {
			mmsd.healthStatusChangedEvent(&event)
		}
	})

	go sse.RunForever()
}

func (mmsd *mmsdService) statusUpdateEvent(event *marathon.StatusUpdateEvent) {
	switch event.TaskStatus {
	case marathon.TaskRunning:
		app, err := mmsd.getMarathonApp(event.AppId)
		if err != nil {
			log.Printf("App %v task %v on %v is running but failed to fetch infos. %v\n",
				event.AppId, event.TaskId, event.Host, err)
			return
		}
		log.Printf("App %v task %v on %v changed status. %v.\n", event.AppId, event.TaskId, event.Host, event.TaskStatus)

		// XXX Only update propagate no health checks have been configured.
		// So we consider thie TASK_RUNNING state as healthy-notice.
		if len(app.HealthChecks) == 0 {
			mmsd.Update(event.AppId, event.TaskId, true)
		}
	case marathon.TaskFinished, marathon.TaskFailed, marathon.TaskKilled, marathon.TaskLost:
		log.Printf("App %v task %v on %v changed status. %v.\n", event.AppId, event.TaskId, event.Host, event.TaskStatus)
		app, err := mmsd.getMarathonApp(event.AppId)
		if err != nil {
			log.Printf("Failed to fetch Marathon app. %+v. %v\n", event, err)
			return
		}
		mmsd.Remove(event.AppId, event.TaskId, app)
	}
}

func (mmsd *mmsdService) healthStatusChangedEvent(event *marathon.HealthStatusChangedEvent) {
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
		mmsd.Remove(event.AppId, event.TaskId, app)
		return
	}

	// app & task definitely do exist, so propagate health change event

	if event.Alive {
		log.Printf("App %v task %v on %v is healthy.\n", event.AppId, event.TaskId, task.Host)
	} else {
		log.Printf("App %v task %v on %v is unhealthy.\n", event.AppId, event.TaskId, task.Host)
	}

	mmsd.Update(event.AppId, event.TaskId, event.Alive)
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
		err = handler.Update(app, taskID)
		if err != nil {
			log.Printf("Update failed. %v\n", err)
		}
	}
}

func (mmsd *mmsdService) Remove(appID string, taskID string, app *marathon.App) {
	for _, handler := range mmsd.Handlers {
		err := handler.Remove(appID, taskID, app)
		if err != nil {
			log.Printf("Remove failed. %v\n", err)
		}
	}
}

func (mmsd *mmsdService) MaybeResetFromTasks(force bool) error {
	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)
	if err != nil {
		return fmt.Errorf("Could not create new marathon service. %v", err)
	}

	apps, err := m.GetApps()
	if err != nil {
		return fmt.Errorf("Could not get apps. %v", err)
	}

	for _, handler := range mmsd.Handlers {
		err = handler.Apply(apps, force)
		if err != nil {
			log.Printf("Failed to apply changes to handler. %v\n", err)
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
	flag.BoolVar(&mmsd.DnsEnabled, "enable-dns", mmsd.DnsEnabled, "Enables DNS-based service discovery")
	flag.UintVar(&mmsd.DnsPort, "dns-port", mmsd.DnsPort, "DNS service discovery port")
	flag.BoolVar(&mmsd.DnsPushSRV, "dns-push-srv", mmsd.DnsPushSRV, "DNS service discovery to also push SRV on A")
	flag.StringVar(&mmsd.DnsBaseName, "dns-basename", mmsd.DnsBaseName, "DNS service discovery's base name")
	flag.DurationVar(&mmsd.DnsTTL, "dns-ttl", mmsd.DnsTTL, "DNS service discovery's reply message TTL")
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

	mmsd.setupManagedIP()
	mmsd.setupHandlers()
	mmsd.setupEventBusListener()
	mmsd.setupHttpService()

	<-mmsd.quitChannel
}

func (mmsd *mmsdService) setupManagedIP() {
	if runtime.GOOS == "linux" { // only enable on Linux
		var serviceIP = ServiceIP{VirtualIP: mmsd.ManagedIP.String()}
		serviceIP.Up()
	}
}

func (mmsd *mmsdService) setupHandlers() {
	if mmsd.DnsEnabled {
		mmsd.Handlers = append(mmsd.Handlers, &DnsManager{
			Verbose:     mmsd.Verbose,
			ServiceAddr: mmsd.ServiceAddr,
			ServicePort: mmsd.DnsPort,
			PushSRV:     mmsd.DnsPushSRV,
			BaseName:    mmsd.DnsBaseName,
			DnsTTL:      mmsd.DnsTTL,
		})
	}

	if mmsd.UDPEnabled {
		mmsd.Handlers = append(mmsd.Handlers, NewUdpManager(
			mmsd.ServiceAddr,
			mmsd.Verbose,
			mmsd.UDPEnabled,
		))
	}

	if mmsd.TCPEnabled {
		mmsd.Handlers = append(mmsd.Handlers, &HaproxyMgr{
			Enabled:           mmsd.TCPEnabled,
			Verbose:           mmsd.Verbose,
			LocalHealthChecks: mmsd.LocalHealthChecks,
			FilterGroups:      strings.Split(mmsd.FilterGroups, ","),
			ServiceAddr:       mmsd.ServiceAddr,
			GatewayEnabled:    mmsd.GatewayEnabled,
			GatewayAddr:       mmsd.GatewayAddr,
			GatewayPortHTTP:   mmsd.GatewayPortHTTP,
			GatewayPortHTTPS:  mmsd.GatewayPortHTTPS,
			Executable:        mmsd.HaproxyBin,
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
			Enabled:  mmsd.FilesEnabled,
			Verbose:  mmsd.Verbose,
			BasePath: mmsd.RunStateDir + "/confd",
		})
	}

	for _, handler := range mmsd.Handlers {
		err := handler.Setup()
		if err != nil {
			log.Fatalf("Failed to setup handlers. %v\n", err)
		}
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
		MarathonScheme:    "http",
		MarathonIP:        net.ParseIP("127.0.0.1"),
		MarathonPort:      8080,
		ReconnectDelay:    time.Second * 4,
		RunStateDir:       "/var/run/mmsd",
		FilterGroups:      "*",
		GatewayEnabled:    false,
		GatewayAddr:       net.ParseIP("0.0.0.0"),
		GatewayPortHTTP:   80,
		GatewayPortHTTPS:  443,
		FilesEnabled:      true,
		UDPEnabled:        true,
		TCPEnabled:        true,
		LocalHealthChecks: true,
		HaproxyBin:        locateExe("haproxy"),
		HaproxyTailCfg:    "/etc/mmsd/haproxy-tail.cfg",
		HaproxyPort:       8081,
		ManagementAddr:    net.ParseIP("0.0.0.0"),
		ServiceAddr:       net.ParseIP("0.0.0.0"),
		HttpApiPort:       8082,
		Verbose:           false,
		DnsEnabled:        false,
		DnsPort:           53,
		DnsPushSRV:        false,
		DnsBaseName:       "mmsd.",
		DnsTTL:            time.Second * 5,
		quitChannel:       make(chan bool),
	}

	mmsd.Run()
}
