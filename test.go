package main

import "github.com/dawanda/go-mesos/marathon"

type instanceSpec struct {
	Host  string
	Ports []uint
}

func mockApp(id string, ports []uint, instanceSpecs []instanceSpec) (app marathon.App) {
	app.Id = id
	app.Ports = ports

	for i, _ := range instanceSpecs {
		app.Tasks = append(app.Tasks, marathon.Task{
			Host:  instanceSpecs[i].Host,
			Ports: instanceSpecs[i].Ports,
		})
	}

	return
}
