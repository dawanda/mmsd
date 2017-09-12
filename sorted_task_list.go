package main

import (
	"sort"

	"github.com/dawanda/go-mesos/marathon"
)

type SortedTaskList struct {
	Tasks     []marathon.Task
	PortIndex int
}

func (tasks SortedTaskList) Len() int {
	return len(tasks.Tasks)
}

func (tasks SortedTaskList) Less(i, j int) bool {
	var a = &tasks.Tasks[i]
	var b = &tasks.Tasks[j]

	if len(a.Ports) > tasks.PortIndex && len(b.Ports) > tasks.PortIndex {
		return a.Host < b.Host || a.Ports[tasks.PortIndex] < b.Ports[tasks.PortIndex]
	}
	// XXX That's a very case; when you redeploy your app with the port count
	// changed, you might run into here.
	return a.Host < b.Host
}

func (tasks SortedTaskList) Swap(i, j int) {
	tmp := tasks.Tasks[i]
	tasks.Tasks[i] = tasks.Tasks[j]
	tasks.Tasks[j] = tmp
}

func sortTasks(tasks []marathon.Task, portIndex int) []marathon.Task {
	stl := SortedTaskList{Tasks: tasks, PortIndex: portIndex}
	sort.Sort(stl)
	return stl.Tasks
}
