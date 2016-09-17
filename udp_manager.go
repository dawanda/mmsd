package main

import (
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/dawanda/go-mesos/marathon"
	"github.com/dawanda/mmsd/udpproxy"
)

type UdpManager struct {
	Verbose  bool
	BindAddr net.IP
	Servers  map[string]*udpproxy.Frontend
}

func NewUdpManager(bindAddr net.IP, verbose bool) *UdpManager {
	return &UdpManager{
		Verbose:  verbose,
		BindAddr: bindAddr,
		Servers:  make(map[string]*udpproxy.Frontend),
	}
}

func (manager *UdpManager) Startup() {
}

func (manager *UdpManager) Shutdown() {
}

func (manager *UdpManager) Apply(apps []*marathon.App, force bool) error {
	for _, app := range apps {
		err := manager.applyApp(app)
		if err != nil {
			return err
		}
	}

	return nil
}

func (manager *UdpManager) GetFrontend(app *marathon.App, portIndex int, replace bool) (*udpproxy.Frontend, error) {
	servicePort := app.PortDefinitions[portIndex].Port
	name := PrettifyAppId(app.Id, portIndex, servicePort)

	server, ok := manager.Servers[name]
	if ok {
		return server, nil
	}

	addr := fmt.Sprintf("%v:%v", manager.BindAddr, servicePort)

	var sched udpproxy.Scheduler
	switch app.Labels["lb-mode"] {
	case "multicast": // or call it "fanout"?
		sched = udpproxy.Multicast
	case "roundrobin":
		sched = udpproxy.RoundRobin
	default:
		sched = udpproxy.RoundRobin
	}

	log.Printf("Spawn UDP frontend %v %v %v\n", addr, sched, name)
	fe, err := udpproxy.NewFrontend(name, addr, sched)
	if err != nil {
		return nil, err
	}

	manager.Servers[name] = fe
	return fe, nil
}

func (manager *UdpManager) removeApp(appID string) error {
	var removals []string

	for name, _ := range manager.Servers {
		if strings.HasPrefix(name, appID) {
			removals = append(removals, name)
		}
	}

	for _, name := range removals {
		manager.Servers[name].Close()
		delete(manager.Servers, name)
	}

	return nil
}

func (manager *UdpManager) applyApp(app *marathon.App) error {
	for portIndex := range app.Ports {
		if GetTransportProtocol(app, portIndex) == "udp" {
			fe, err := manager.GetFrontend(app, portIndex, true)
			if err != nil {
				log.Printf("Error spawning UDP frontend. %v\n", err)
			} else {
				// add backends
				fe.ClearTouch()
				for _, task := range app.Tasks {
					if task.IsAlive() {
						name := fmt.Sprintf("%v-%v", task.Host, task.Id)
						addr := fmt.Sprintf("%v:%v", task.Host, task.Ports[portIndex])
						be, err := fe.AddBackend(name, addr)
						if err != nil {
							log.Printf("Failed to add backend %v %v. %v\n", name, addr, err)
						} else {
							be.Touch()
						}
					}
				}
				fe.RemoveUntouched()
			}

			// serve in background
			go fe.Serve()
		}
	}
	return nil
}

func (manager *UdpManager) Remove(appID string, taskID string, app *marathon.App) error {
	if app != nil {
		return manager.applyApp(app)
	} else {
		return manager.removeApp(appID)
	}
}

func (manager *UdpManager) Update(app *marathon.App, taskID string) error {
	return manager.applyApp(app)
}
