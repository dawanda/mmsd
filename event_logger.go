package main

import (
	"encoding/json"
	"fmt"
	"log"
)

type EventLogger struct {
	Verbose bool
}

func (logger *EventLogger) Startup() {
	log.Printf("Initialize\n")
}

func (logger *EventLogger) Shutdown() {
	log.Printf("Shutdown\n")
}

func (logger *EventLogger) Apply(apps []AppCluster) {
	if logger.Verbose {
		out, err := json.MarshalIndent(apps, "", "  ")
		if err != nil {
			log.Printf("Marshal failed. %v\n", err)
		} else {
			fmt.Printf("%v\n", string(out))
		}
	}

	for _, app := range apps {
		log.Printf("Apply: %v\n", app.Id)
	}
}

func (logger *EventLogger) AddTask(task AppBackend, app AppCluster) {
	log.Printf("Task Add: %v: %v %v\n", task.State, app.Id, task.Host)
}

func (logger *EventLogger) RemoveTask(task AppBackend, app AppCluster) {
	log.Printf("Task Remove: %v: %v %v\n", task.State, app.Id, task.Host)
}
