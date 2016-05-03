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

type appEntry struct {
	ipAddresses []net.IP
	app         *marathon.App
}

type DnsManager struct {
	Verbose     bool
	ServiceAddr net.IP
	ServicePort uint
	DnsBaseName string
	DnsTTL      time.Duration
	server      *dns.Server
	db          map[string]*appEntry
	dbMutex     sync.Mutex
}

func (manager *DnsManager) Setup() error {
	dns.HandleFunc(manager.DnsBaseName, manager.dnsHandler)

	manager.server = &dns.Server{
		Addr:       fmt.Sprintf("%v:%v", manager.ServiceAddr, manager.ServicePort),
		Net:        "udp",
		TsigSecret: nil,
	}

	go func() {
		err := manager.server.ListenAndServe()
		if err != nil {
			log.Fatal(err)
		}
	}()
	return nil
}

func (manager *DnsManager) Shutdown() {
	manager.server.Shutdown()
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
	name = strings.TrimSuffix(name, "."+manager.DnsBaseName)

	manager.dbMutex.Lock()
	entry, ok := manager.db[name]
	manager.dbMutex.Unlock()

	if ok {
		switch req.Question[0].Qtype {
		case dns.TypeTXT:
			// p0,p1,p2:p0,p1,p2
		case dns.TypeSRV:
			for _, task := range entry.app.Tasks {
				for _, port := range task.Ports {
					rr := &dns.SRV{
						Hdr: dns.RR_Header{
							Ttl:    uint32(manager.DnsTTL.Seconds()),
							Name:   manager.DnsBaseName,
							Class:  dns.ClassINET,
							Rrtype: req.Question[0].Qtype,
						},
						Port:     uint16(port),
						Target:   task.Host + ".",
						Weight:   1,
						Priority: 1,
					}
					m.Answer = append(m.Answer, rr)
				}
			}
		case dns.TypeA:
			for _, ip := range entry.ipAddresses {
				rr := &dns.A{
					Hdr: dns.RR_Header{
						Ttl:    uint32(manager.DnsTTL.Seconds()),
						Name:   manager.DnsBaseName,
						Class:  dns.ClassINET,
						Rrtype: req.Question[0].Qtype,
					},
					A: ip.To4(),
				}
				m.Answer = append(m.Answer, rr)
			}
		}
	}

	w.WriteMsg(m)
}
