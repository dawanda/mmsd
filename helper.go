package main

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dawanda/go-mesos/marathon"
)

func PrettifyAppId(name string, portIndex int, servicePort uint) (appID string) {
	appID = strings.Replace(name[1:], "/", ".", -1)
	appID = fmt.Sprintf("%v-%v-%v", appID, portIndex, servicePort)

	return
}

func PrettifyDnsName(dns string) string {
	return strings.SplitN(dns, ".", 1)[0]
}

func SoftResolveIPAddr(dns string) string {
	if ip, err := net.ResolveIPAddr("ip", dns); err == nil {
		return ip.String()
	} else {
		// fallback to actual dns name
		return dns
	}
}

func resolveIPAddr(dns string, skip bool) string {
	if skip {
		return dns
	}
	return SoftResolveIPAddr(dns)
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

func Atoi(value string, defaultValue int) int {
	if result, err := strconv.Atoi(value); err == nil {
		return result
	}

	return defaultValue
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
	if len(app.PortDefinitions) > portIndex {
		return app.PortDefinitions[portIndex].Protocol
	}

	if app.Container.Docker != nil && len(app.Container.Docker.PortMappings) > portIndex {
		return strings.ToLower(app.Container.Docker.PortMappings[portIndex].Protocol)
	}

	if len(app.PortDefinitions) > 0 {
		return "tcp" // default to TCP if at least one port was exposed (host networking)
	}

	return "" // no ports exposed
}

func GetHealthCheckProtocol(app *marathon.App, portIndex int) string {
	for _, hs := range app.HealthChecks {
		if hs.PortIndex == portIndex {
			return strings.ToLower(hs.Protocol)
		}
	}

	return ""
}

func GetHealthCheckForPortIndex(healthChecks []marathon.HealthCheck, portIndex int) marathon.HealthCheck {
	for _, hs := range healthChecks {
		if hs.PortIndex == portIndex {
			return hs
		}
	}

	return marathon.HealthCheck{}
}

func Hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func makeStringArray(s string) []string {
	if len(s) == 0 {
		return []string{}
	} else {
		return strings.Split(s, ",")
	}
}

func parseRange(input string) (int, int, error) {
	if len(input) == 0 {
		return 0, 0, nil
	}

	vals := strings.Split(input, ":")
	log.Printf("vals: %+q\n", vals)

	if len(vals) == 1 {
		i, err := strconv.Atoi(input)
		return i, i, err
	}

	if len(vals) > 2 {
		return 0, 0, ErrInvalidPortRange
	}

	var (
		begin int
		end   int
		err   error
	)

	// parse begin
	if vals[0] != "" {
		begin, err = strconv.Atoi(vals[0])
		if err != nil {
			return begin, end, err
		}
	}

	// parse end
	if vals[1] != "" {
		end, err = strconv.Atoi(vals[1])
		if begin > end {
			return begin, end, ErrInvalidPortRange
		}
	} else {
		end = -1 // XXX that is: until the end
	}

	return begin, end, err
}

func locateExe(name string) string {
	for _, prefix := range strings.Split(os.Getenv("PATH"), ":") {
		path := filepath.Join(prefix, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return name // default to name only
}
