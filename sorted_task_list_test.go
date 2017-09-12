package main

import (
	"testing"

	"github.com/dawanda/go-mesos/marathon"
)

func joinHosts(tasks []marathon.Task) (hosts string) {
	for _, task := range tasks {
		hosts = hosts + task.Host
	}
	return
}

func TestSortTasks(t *testing.T) {
	var tests = []struct {
		app       marathon.App
		portIndex int
		result    string
		message   string
	}{
		{
			mockApp(
				"/test/foo",
				[]uint{80},
				[]instanceSpec{
					instanceSpec{"ccc", []uint{80}},
					instanceSpec{"aaa", []uint{80}},
				}),
			0,
			"aaaccc",
			"Simple compare",
		},
		{
			mockApp(
				"/test/foo",
				[]uint{80},
				[]instanceSpec{
					instanceSpec{"ccc", []uint{80}},
					instanceSpec{"aaa", []uint{80}},
					instanceSpec{"bbb", []uint{80, 443}},
				}),
			0,
			"aaabbbccc",
			"New instance added",
		},
		{
			mockApp(
				"/test/foo",
				[]uint{80, 443},
				[]instanceSpec{
					instanceSpec{"ccc", []uint{80}},
					instanceSpec{"aaa", []uint{80}},
					instanceSpec{"bbb", []uint{80, 443}},
				}),
			1,
			"aaabbbccc",
			"Number of ports mismatch between tasks",
		},
	}

	for _, test := range tests {
		t.Log(test.message)
		sorted := sortTasks(test.app.Tasks, test.portIndex)
		if joinHosts(sorted) != test.result {
			t.Errorf("Expect '%s' to be order for port index %d, result: %s", test.result, test.portIndex, joinHosts(sorted))
		}
	}
}
