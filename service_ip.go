package main

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"syscall"
)

type ServiceIP struct {
	VirtualIP string
}

func (service *ServiceIP) Up() {
	proc := exec.Command("/bin/sh", "-c", "ip route | grep -w src | grep -v -w linkdown | awk '{print $NF}' | tail -n1")
	output, err := proc.CombinedOutput()
	if err != nil {
		log.Fatalf("Failed to get host ip. %v\n", err)
	}
	hostIP := strings.TrimSpace(string(output))
	service.ensureDNAT("PREROUTING", hostIP)
	service.ensureDNAT("OUTPUT", hostIP)
}

func (service *ServiceIP) ensureDNAT(chain, hostIP string) {
	service.sh("iptables -t nat -A %v -d %v -j DNAT --to %v", chain, service.VirtualIP, hostIP)
}

func (service *ServiceIP) sh(cmd string, args ...interface{}) int {
	cmdline := fmt.Sprintf(cmd, args...)
	proc := exec.Command("/bin/sh", "-c", cmdline)
	output, err := proc.CombinedOutput()
	if err != nil {
		log.Printf("Error executing: %v\n", cmdline)
		log.Println(err)
		return 1
	} else {
		exitCode := proc.ProcessState.Sys().(syscall.WaitStatus)
		if exitCode != 0 {
			log.Printf("Error executing (exit code %v): %v\n", exitCode, cmdline)
			log.Println(strings.TrimSpace(string(output)))
		}
		return int(exitCode)
	}
}
