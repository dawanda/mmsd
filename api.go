package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/dawanda/go-mesos/marathon"
	"github.com/gorilla/mux"
)

type apiConfig struct {
	Version      string
	MarathonIP   net.IP
	MarathonPort uint
	Addr         net.IP
	Port         uint
}

func NewAPI(version string, marathonIP net.IP, marathonPort uint, addr net.IP, port uint) {
	api := apiConfig{version, marathonIP, marathonPort, addr, port}
	router := mux.NewRouter()
	router.HandleFunc("/ping", api.v0Ping)
	router.HandleFunc("/version", api.v0Version)

	v1 := router.PathPrefix("/v1").Subrouter()
	v1.HandleFunc("/apps", api.v1Apps).Methods("GET")
	v1.HandleFunc("/instances{app_id:/.*}", api.v1Instances).Methods("GET")
	v1.HandleFunc("/service_ports{app_id:/.*}", api.v1ServicePorts).Methods("GET")

	serviceAddr := fmt.Sprintf("%v:%v", api.Addr, api.Port)
	log.Printf("Exposing service API on http://%v\n", serviceAddr)

	go http.ListenAndServe(serviceAddr, router)
}

func (api *apiConfig) v0Ping(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "pong\n")
}

func (api *apiConfig) v0Version(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "api %v\n", api.Version)
}

func (api *apiConfig) v1Apps(w http.ResponseWriter, r *http.Request) {
	m, err := marathon.NewService(api.MarathonIP, api.MarathonPort)

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

func (api *apiConfig) v1Instances(w http.ResponseWriter, r *http.Request) {
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

	m, err := marathon.NewService(api.MarathonIP, api.MarathonPort)
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

func (api *apiConfig) v1ServicePorts(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	appID := vars["app_id"]

	m, err := marathon.NewService(api.MarathonIP, api.MarathonPort)
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

	for _, port := range app.Ports {
		fmt.Fprintln(w, port)
	}
}
