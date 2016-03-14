package main

/* TODO:

1. [x] serve HTTP /v1/instances/:app_id to retrieve ip:port pairs for given app
2. [ ] implement upstream-conf.d file management
3. [ ] implement UDP proxy (one-way/two-way, fanout & roundrobin)
4. [ ] implement TCP proxy (with pluggable impls: haproxy, lvs, ...)

XXX Changes:

* `--marathon-host` is now named `--marathon-ip` and only accepts IP addresses

*/

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/christianparpart/serviced/marathon"
	"github.com/gorilla/mux"
	flag "github.com/ogier/pflag"
)

type MmsdHandler interface {
	Apply(apps []*marathon.App, force bool) error
}

type MmsdService struct {
	MarathonScheme   string
	MarathonIP       net.IP
	MarathonPort     uint
	RunStateDir      string
	GatewayEnabled   bool
	GatewayPortHTTP  uint
	GatewayPortHTTPS uint
	ManagedIP        net.IP
	HaproxyCfg       string
	HaproxyCfgTail   string
	HaproxyPort      uint
	ServiceBind      net.IP
	ServicePort      uint
	Handlers         []MmsdHandler
}

func (mmsd *MmsdService) v1_apps(w http.ResponseWriter, r *http.Request) {
	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)

	if err != nil {
		log.Printf("NewService error. %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
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

	portIndex := 0
	if sval := r.URL.Query().Get("portIndex"); len(sval) != 0 {
		i, err := strconv.Atoi(sval)
		if err == nil {
			portIndex = i
		}
	}

	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)
	if err != nil {
		log.Printf("NewService error. %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
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

type HealthStatusChangedEvent struct {
	AppId  string
	TaskId string
	Alive  bool
}

func (mmsd *MmsdService) SetupSSE() {
	var url string = fmt.Sprintf("http://%v:%v/v2/events",
		mmsd.MarathonIP, mmsd.MarathonPort)

	var sse *EventSource = NewEventSource(url)

	sse.OnOpen = func(event, data string) {
		log.Printf("OnOpen event '%v': %+v\n", event, data)
	}
	sse.OnMessage = func(event, data string) {
		//log.Printf("OnMessage event '%v': %+v\n", event, data)
		log.Printf("OnMessage event '%v'\n", event)
	}
	sse.OnError = func(event, data string) {
		log.Printf("OnError: '%v': %+v\n", event, data)
	}

	sse.AddEventListener("event_stream_attached", func(data string) {
		log.Printf("sse client attached: %+v\n", data)
	})

	sse.AddEventListener("event_stream_detached", func(data string) {
		log.Printf("sse client detached: %+v\n", data)
	})

	sse.AddEventListener("health_status_changed_event", func(data string) {
		var event HealthStatusChangedEvent
		json.Unmarshal([]byte(data), &event)
		log.Printf("Health Status Changed: %+v\n", event)
	})

	go sse.RunForever()
}

func (mmsd *MmsdService) ResetFromTasks() error {
	return mmsd.MaybeResetFromTasks(false)
}

func (mmsd *MmsdService) ForcedResetFromTasks() error {
	return mmsd.MaybeResetFromTasks(true)
}

func (mmsd *MmsdService) MaybeResetFromTasks(force bool) error {
	log.Printf("MaybeResetFromTasks(%v)\n", force)
	m, err := marathon.NewService(mmsd.MarathonIP, mmsd.MarathonPort)
	if err != nil {
		return err
	}

	apps, err := m.GetApps()
	if err != nil {
		return err
	}

	for _, handler := range mmsd.Handlers {
		log.Printf("Calling handler\n")
		err = handler.Apply(apps, force)
		if err != nil {
			log.Printf("Failed to apply changes to handler. %v\n", err)
		}
	}

	return nil
}

func PrettifyAppId(name string) string {
	return name
}

func (mmsd *MmsdService) Run() {
	flag.IPVar(&mmsd.MarathonIP, "marathon-ip", mmsd.MarathonIP, "Marathon endpoint TCP IP address")
	flag.UintVar(&mmsd.MarathonPort, "marathon-port", mmsd.MarathonPort, "Marathon endpoint TCP port number")
	flag.StringVar(&mmsd.RunStateDir, "run-state-dir", mmsd.RunStateDir, "Path to directory to keep run-state")
	flag.IPVar(&mmsd.ManagedIP, "managed-ip", mmsd.ManagedIP, "IP-address to manage for mmsd")
	flag.BoolVar(&mmsd.GatewayEnabled, "gateway", mmsd.GatewayEnabled, "Enables gateway support")
	flag.UintVar(&mmsd.GatewayPortHTTP, "gateway-http-port", mmsd.GatewayPortHTTP, "gateway HTTP port")
	flag.UintVar(&mmsd.GatewayPortHTTPS, "gateway-https-port", mmsd.GatewayPortHTTPS, "gateway HTTP port")

	flag.StringVar(&mmsd.HaproxyCfg, "haproxy-cfg", mmsd.HaproxyCfg, "path to haproxy config file")
	flag.StringVar(&mmsd.HaproxyCfgTail, "haproxy-cfgtail", mmsd.HaproxyCfgTail, "path to haproxy tail config file")
	flag.IPVar(&mmsd.ServiceBind, "haproxy-bind", mmsd.ServiceBind, "haproxy management port")
	flag.UintVar(&mmsd.HaproxyPort, "haproxy-port", mmsd.HaproxyPort, "haproxy management port")

	flag.Parse()

	// handlers
	mmsd.Handlers = append(mmsd.Handlers, &UpstreamFilesBuilder{
		BasePath: mmsd.RunStateDir + "/conf.d"})

	err := mmsd.ForcedResetFromTasks()
	if err != nil {
		log.Printf("Could not force task state reset. %v\n", err)
	}

	// SSE
	mmsd.SetupSSE()

	// HTTP service
	router := mux.NewRouter()
	v1 := router.PathPrefix("/v1").Subrouter()

	v1.HandleFunc("/apps", mmsd.v1_apps).Methods("GET")
	v1.HandleFunc("/instances{name:/.*}", mmsd.v1_instances).Methods("GET")

	serviceAddr := fmt.Sprintf("%v:%v", mmsd.ServiceBind, mmsd.ServicePort)
	log.Printf("Service listening on http://%v\n", serviceAddr)

	http.ListenAndServe(serviceAddr, router)
}

func main() {
	var mmsd MmsdService = MmsdService{
		MarathonScheme:   "http",
		MarathonIP:       net.ParseIP("127.0.0.1"),
		MarathonPort:     8080,
		RunStateDir:      "/var/run/mmsd",
		GatewayEnabled:   false,
		GatewayPortHTTP:  80,
		GatewayPortHTTPS: 443,
		HaproxyCfg:       "/var/run/mmsd/haproxy.cfg",
		HaproxyCfgTail:   "/etc/mmsd/haproxy-tail.cfg",
		HaproxyPort:      8081,
		ServiceBind:      net.ParseIP("0.0.0.0"),
		ServicePort:      8082,
	}

	mmsd.Run()
}
