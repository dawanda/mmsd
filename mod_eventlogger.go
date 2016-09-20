package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/dawanda/mmsd/module_api"
)

/* EventLoggerModule adds simple event logging to the logger.
 */
type EventLoggerModule struct {
	Verbose bool
}

func (logger *EventLoggerModule) Startup() {
	log.Printf("Initialize\n")
}

func (logger *EventLoggerModule) Shutdown() {
	log.Printf("Shutdown\n")
}

func (logger *EventLoggerModule) Apply(apps []*module_api.AppCluster) {
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

func (logger *EventLoggerModule) AddTask(task *module_api.AppBackend, app *module_api.AppCluster) {
	log.Printf("Task Add: %v: %v %v\n", task.State, app.Id, task.Host)
}

func (logger *EventLoggerModule) RemoveTask(task *module_api.AppBackend, app *module_api.AppCluster) {
	log.Printf("Task Remove: %v: %v %v\n", task.State, app.Id, task.Host)
}
