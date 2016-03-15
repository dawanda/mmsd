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
	Proxy    *UdpProxy
}

func NewUdpManager(bindAddr net.IP) *UdpManager {
	return &UdpManager{
		Verbose:  true,
		BindAddr: bindAddr,
		Proxy:    NewUdpProxy(),
	}
}

func (manager *UdpManager) Apply(apps []*marathon.App, force bool) error {
	for _, app := range apps {
		for portIndex := range app.Ports {
			if GetTransportProtocol(app, portIndex) == "udp" {
				if app.Container.Docker != nil {
					// create frontend
					portMapping := app.Container.Docker.PortMappings[portIndex]
					name := PrettifyAppId(app.Id, portIndex, portMapping.ServicePort)
					addr := fmt.Sprintf("%v:%v", manager.BindAddr, portMapping.ServicePort)
					sched := Multicast
					log.Printf("Create UDP frontend %v: %v\n", name, addr)
					fe, err := manager.Proxy.AddOrReplaceFrontend(name, addr, sched)
					if err != nil {
						log.Printf("Error spawning UDP frontend. %v\n", err)
					} else {
						// add backends
						for _, task := range app.Tasks {
							name := fmt.Sprintf("%v-%v", task.Host, task.Id)
							addr := fmt.Sprintf("%v:%v", task.Host, task.Ports[portIndex])
							log.Printf("Adding UDP backend %v (%v)\n", addr, task.Id)
							fe.AddBackend(name, addr)
						}
					}

					// serve in background
					go fe.Serve()
				}
			}
		}
	}

	return nil
}

func (manager *UdpManager) Update(app *marathon.App, task *marathon.Task) error {
	// TODO: only apply changes to given app/task
	return nil
}
