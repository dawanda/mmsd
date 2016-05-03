package main

// DNS based service discovery
// ---------------------------------------------------------------------------
//
// Marathon: "/path/to/application"
// DNS-query: application.to.path.$basedomain (A, AAAA, TXT, SRV)
// DNS-reply:
//   A 		=> list of IPv4 addresses
//   AAAA => list of IPv6 addresses
//   SRV 	=> ip:port array per task
//   TXT 	=> app labels

// TODO: DNS forwarding
// TODO: DNS proxy cache (for speeding up)

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/christianparpart/go-marathon/marathon"
	"github.com/miekg/dns"
)

type DnsManager struct {
	Verbose     bool
	ServiceAddr net.IP
	ServicePort uint
	BaseName    string
	DnsTTL      time.Duration
	PushSRV     bool
	udpServer   *dns.Server
	tcpServer   *dns.Server
	db          map[string]*appEntry
	dbMutex     sync.Mutex
}

type appEntry struct {
	ipAddresses []net.IP
	app         *marathon.App
}

func (manager *DnsManager) Setup() error {
	dns.HandleFunc(manager.BaseName, manager.dnsHandler)

	go func() {
		manager.udpServer = &dns.Server{
			Addr:       fmt.Sprintf("%v:%v", manager.ServiceAddr, manager.ServicePort),
			Net:        "udp",
			TsigSecret: nil,
		}
		err := manager.udpServer.ListenAndServe()
		if err != nil {
			log.Fatal(err)
		}
	}()

	go func() {
		manager.tcpServer = &dns.Server{
			Addr:       fmt.Sprintf("%v:%v", manager.ServiceAddr, manager.ServicePort),
			Net:        "tcp",
			TsigSecret: nil,
		}
		err := manager.tcpServer.ListenAndServe()
		if err != nil {
			log.Fatal(err)
		}
	}()

	return nil
}

func (manager *DnsManager) Shutdown() {
	manager.udpServer.Shutdown()
	manager.tcpServer.Shutdown()
}

func (manager *DnsManager) Log(msg string) {
	if manager.Verbose {
		log.Printf("[dns]: %v\n", msg)
	}
}

func (manager *DnsManager) Apply(apps []*marathon.App, force bool) error {
	manager.dbMutex.Lock()
	manager.db = make(map[string]*appEntry)
	manager.dbMutex.Unlock()

	for _, app := range apps {
		if err := manager.updateApp(app); err != nil {
			return err
		}
	}

	return nil
}

func (manager *DnsManager) Update(app *marathon.App, taskID string) error {
	return manager.updateApp(app)
}

func (manager *DnsManager) updateApp(app *marathon.App) error {
	var ipAddresses []net.IP

	for _, task := range app.Tasks {
		ip, err := net.ResolveIPAddr("ip", task.Host)
		if err != nil {
			return err
		}
		ipAddresses = append(ipAddresses, ip.IP)
	}

	var reversed = manager.makeDnsNameFromAppId(app.Id)
	var entry = &appEntry{
		ipAddresses: ipAddresses,
		app:         app,
	}

	manager.dbMutex.Lock()
	manager.db[reversed] = entry
	manager.dbMutex.Unlock()

	return nil
}

func (manager *DnsManager) makeDnsNameFromAppId(appID string) string {
	var parts = strings.Split(appID, "/")[1:]
	var reversedParts []string
	for i := range parts {
		reversedParts = append(reversedParts, parts[len(parts)-i-1])
	}
	var reversed = strings.Join(reversedParts, ".")

	return reversed
}

func (manager *DnsManager) Remove(appID string, taskID string, app *marathon.App) error {
	if app == nil {
		manager.dbMutex.Lock()
		delete(manager.db, manager.makeDnsNameFromAppId(appID))
		manager.dbMutex.Unlock()
		return nil
	}
	return manager.updateApp(app)
}

func (manager *DnsManager) dnsHandler(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)

	name := req.Question[0].Name
	name = strings.TrimSuffix(name, "."+manager.BaseName)

	manager.dbMutex.Lock()
	entry, ok := manager.db[name]
	manager.dbMutex.Unlock()

	if ok {
		switch req.Question[0].Qtype {
		case dns.TypeSRV:
			m.Answer = manager.makeAllSRV(entry)
		case dns.TypeA:
			m.Answer = manager.makeAllA(entry)
			if manager.PushSRV {
				m.Extra = manager.makeAllSRV(entry)
			}
		}
	}

	w.WriteMsg(m)
}

func (manager *DnsManager) makeAllA(entry *appEntry) []dns.RR {
	var result []dns.RR

	for _, ip := range entry.ipAddresses {
		rr := &dns.A{
			Hdr: dns.RR_Header{
				Ttl:    uint32(manager.DnsTTL.Seconds()),
				Name:   manager.BaseName,
				Class:  dns.ClassINET,
				Rrtype: dns.TypeA,
			},
			A: ip.To4(),
		}
		result = append(result, rr)
	}

	return result
}

func (manager *DnsManager) makeAllSRV(entry *appEntry) []dns.RR {
	var result []dns.RR

	for _, task := range entry.app.Tasks {
		for _, port := range task.Ports {
			rr := &dns.SRV{
				Hdr: dns.RR_Header{
					Ttl:    uint32(manager.DnsTTL.Seconds()),
					Name:   manager.BaseName,
					Class:  dns.ClassINET,
					Rrtype: dns.TypeSRV,
				},
				Port:     uint16(port),
				Target:   task.Host + ".",
				Weight:   1,
				Priority: 1,
			}
			result = append(result, rr)
		}
	}

	return result
}
