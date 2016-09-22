package modules

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/dawanda/mmsd/core"
)

/* EventLoggerModule adds simple event logging to the logger.
 */
type EventLoggerModule struct {
	Verbose bool
}

func (logger *EventLoggerModule) Startup() {
	log.Printf("eventlogger: Initialize\n")
}

func (logger *EventLoggerModule) Shutdown() {
	log.Printf("eventlogger: Shutdown\n")
}

func (logger *EventLoggerModule) Apply(apps []*core.AppCluster) {
	if logger.Verbose {
		out, err := json.MarshalIndent(apps, "", "  ")
		if err != nil {
			log.Printf("eventlogger: Marshal failed. %v\n", err)
		} else {
			fmt.Printf("eventlogger: %v\n", string(out))
		}
	}

	for _, app := range apps {
		log.Printf("eventlogger: Apply: %v\n", app.Id)
	}
}

func (logger *EventLoggerModule) AddTask(task *core.AppBackend, app *core.AppCluster) {
	log.Printf("eventlogger: Task Add: %v: %v %v\n", task.State, app.Id, task.Host)
}

func (logger *EventLoggerModule) RemoveTask(task *core.AppBackend, app *core.AppCluster) {
	log.Printf("eventlogger: Task Remove: %v: %v %v\n", task.State, app.Id, task.Host)
}
