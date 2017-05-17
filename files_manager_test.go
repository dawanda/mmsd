package main

import (
	"testing"

	"github.com/dawanda/go-mesos/marathon"
)

func TestWriteApp(t *testing.T) {
	var tests = []struct {
		app     marathon.App
		message string
	}{
		{
			mockApp(
				"/test/foo",
				[]uint{80},
				[]instanceSpec{
					instanceSpec{"ccc", []uint{80}},
					instanceSpec{"aaa", []uint{80}},
				}),
			"A simple app with one port",
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
			"An app gets a second port",
		},
	}

	manager := FilesManager{
		BasePath: ".",
	}

	for _, test := range tests {
		t.Log(test.message)
		_, err := manager.writeApp(&test.app)

		if err != nil {
			t.Errorf("%v", err)
		}
	}
}
