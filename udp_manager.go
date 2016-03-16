package main

import (
	"fmt"
	"github.com/christianparpart/serviced/marathon"
	"log"
	"net"
)

type UdpManager struct {
	Verbose  bool
	BindAddr net.IP
	Servers  map[string]*UdpFrontend
}

func NewUdpManager(bindAddr net.IP, verbose bool) *UdpManager {
	return &UdpManager{
		Verbose:  verbose,
		BindAddr: bindAddr,
		Servers:  make(map[string]*UdpFrontend),
	}
}

func (manager *UdpManager) Apply(apps []*marathon.App, force bool) error {
	for _, app := range apps {
		err := manager.ApplyApp(app)
		if err != nil {
			return err
		}
	}

	return nil
}

func (manager *UdpManager) GetFrontend(app *marathon.App, portIndex int, replace bool) (*UdpFrontend, error) {
	servicePort := app.Container.Docker.PortMappings[portIndex].ServicePort
	name := PrettifyAppId(app.Id, portIndex, servicePort)

	server, ok := manager.Servers[name]
	if ok {
		return server, nil
	}

	addr := fmt.Sprintf("%v:%v", manager.BindAddr, servicePort)
	sched := Multicast // TODO: configurable

	log.Printf("Spawn UDP frontend %v %v %v\n", addr, sched, name)
	fe, err := NewUdpFrontend(name, addr, sched)
	if err != nil {
		return nil, err
	}

	manager.Servers[name] = fe
	return fe, nil
}

func (manager *UdpManager) ApplyApp(app *marathon.App) error {
	for portIndex := range app.Ports {
		if GetTransportProtocol(app, portIndex) == "udp" {
			fe, err := manager.GetFrontend(app, portIndex, true)
			if err != nil {
				log.Printf("Error spawning UDP frontend. %v\n", err)
			} else {
				// add backends
				fe.ClearTouch()
				for _, task := range app.Tasks {
					name := fmt.Sprintf("%v-%v", task.Host, task.Id)
					addr := fmt.Sprintf("%v:%v", task.Host, task.Ports[portIndex])
					be, err := fe.AddBackend(name, addr)
					if err != nil {
						log.Printf("Failed to add backend %v %v. %v\n", name, addr, err)
					} else {
						be.Touch()
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

func (manager *UdpManager) Update(app *marathon.App, task *marathon.Task) error {
	return manager.ApplyApp(app)
}
