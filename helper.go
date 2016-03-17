package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/christianparpart/serviced/marathon"
)

func PrettifyAppId(name string, portIndex int, servicePort uint) (app_id string) {
	app_id = strings.Replace(name[1:], "/", ".", -1)
	app_id = fmt.Sprintf("%v-%v-%v", app_id, portIndex, servicePort)

	return
}

// http://stackoverflow.com/a/30038571/386670
func FileIsIdentical(file1, file2 string) bool {
	const chunkSize = 64000

	// check file size ...
	fileInfo1, err := os.Stat(file1)
	if err != nil {
		return false
	}

	fileInfo2, err := os.Stat(file2)
	if err != nil {
		return false
	}

	if fileInfo1.Size() != fileInfo2.Size() {
		return false
	}

	// check file contents ...
	f1, err := os.Open(file1)
	if err != nil {
		log.Fatal(err)
	}

	f2, err := os.Open(file2)
	if err != nil {
		log.Fatal(err)
	}

	for {
		b1 := make([]byte, chunkSize)
		_, err1 := f1.Read(b1)

		b2 := make([]byte, chunkSize)
		_, err2 := f2.Read(b2)

		if err1 != nil || err2 != nil {
			if err1 == io.EOF && err2 == io.EOF {
				return true
			} else if err1 == io.EOF || err2 == io.EOF {
				return false
			} else {
				log.Fatal(err1, err2)
			}
		}

		if !bytes.Equal(b1, b2) {
			return false
		}
	}
}

func Contains(slice []string, item string) bool {
	for _, value := range slice {
		if value == item {
			return true
		}
	}

	return false
}

// Finds all missing items that are found in slice2 but not in slice1.
func FindMissing(slice1, slice2 []string) (missing []string) {
	for _, item := range slice1 {
		if !Contains(slice2, item) {
			missing = append(missing, item)
		}
	}

	return
}

func GetApplicationProtocol(app *marathon.App, portIndex int) (proto string) {
	if proto = strings.ToLower(app.Labels["proto"]); len(proto) != 0 {
		return
	}

	if proto = GetHealthCheckProtocol(app, portIndex); len(proto) != 0 {
		return
	}

	if proto = GetTransportProtocol(app, portIndex); len(proto) != 0 {
		return
	}

	proto = "tcp"
	return
}

func GetTransportProtocol(app *marathon.App, portIndex int) string {
	if app.Container.Docker != nil && len(app.Container.Docker.PortMappings) > portIndex {
		return strings.ToLower(app.Container.Docker.PortMappings[portIndex].Protocol)
	}

	return ""
}

func GetHealthCheckProtocol(app *marathon.App, portIndex int) string {
	for _, hs := range app.HealthChecks {
		if hs.PortIndex == portIndex {
			return strings.ToLower(hs.Protocol)
		}
	}

	return ""
}
