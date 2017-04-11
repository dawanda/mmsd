package main

import (
	"testing"

	"github.com/dawanda/go-mesos/marathon"
)

func TestSortTasks(t *testing.T) {
	app := marathon.App{
		Tasks: []marathon.Task{
			marathon.Task{
				Host:  "ccc",
				Ports: []uint{80},
			},
			marathon.Task{
				Host:  "aaa",
				Ports: []uint{80},
			},
		},
	}
	t.Log("Simple compare")
	sorted := sortTasks(app.Tasks, 0)

	if sorted[0].Host+sorted[1].Host != "aaaccc" {
		t.Error("Expect 'aaa ccc' to be order for port index 0, result:", sorted[0].Host, sorted[1].Host)
	}

	app.Tasks = append(app.Tasks, marathon.Task{
		Host:  "bbb",
		Ports: []uint{80, 443},
	})

	t.Log("Compare after new instance added")
	sorted = sortTasks(app.Tasks, 0)

	if sorted[0].Host+sorted[1].Host+sorted[2].Host != "aaabbbccc" {
		t.Error("Expect 'aaa bbb ccc' to be order for port index 0, result:", sorted[0].Host, sorted[1].Host, sorted[2].Host)
	}

	t.Log("Compare a new added port")
	sorted = sortTasks(app.Tasks, 1)

	if sorted[0].Host+sorted[1].Host+sorted[2].Host != "aaabbbccc" {
		t.Error("Expect 'aaa bbb ccc' to be order for port index 1, result:", sorted[0].Host, sorted[1].Host, sorted[2].Host)
	}
}
