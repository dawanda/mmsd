package modules

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

	"github.com/dawanda/mmsd/module_api"
	"github.com/miekg/dns"
)

type DNSModule struct {
	Verbose     bool
	ServiceAddr net.IP
	ServicePort uint
	BaseName    string
	DNSTTL      time.Duration
	PushSRV     bool
	udpServer   *dns.Server
	tcpServer   *dns.Server
	db          map[string]*dbEntry
	dbMutex     sync.Mutex
}

type dbEntry struct {
	ipAddresses []net.IP
	app         *module_api.AppCluster
}

func (module *DNSModule) Startup() {
	dns.HandleFunc(module.BaseName, module.dnsHandler)

	go func() {
		module.udpServer = &dns.Server{
			Addr:       fmt.Sprintf("%v:%v", module.ServiceAddr, module.ServicePort),
			Net:        "udp",
			TsigSecret: nil,
		}
		err := module.udpServer.ListenAndServe()
		if err != nil {
			log.Fatal(err)
		}
	}()

	go func() {
		module.tcpServer = &dns.Server{
			Addr:       fmt.Sprintf("%v:%v", module.ServiceAddr, module.ServicePort),
			Net:        "tcp",
			TsigSecret: nil,
		}
		err := module.tcpServer.ListenAndServe()
		if err != nil {
			log.Fatal(err)
		}
	}()
}

func (module *DNSModule) Shutdown() {
	module.udpServer.Shutdown()
	module.tcpServer.Shutdown()
}

func (module *DNSModule) Apply(apps []*module_api.AppCluster) {
	module.dbMutex.Lock()
	module.db = make(map[string]*dbEntry)
	module.dbMutex.Unlock()

	for _, app := range apps {
		if err := module.update(app); err != nil {
			return
		}
	}
}

func (module *DNSModule) AddTask(task *module_api.AppBackend, app *module_api.AppCluster) {
	module.update(app)
}

func (module *DNSModule) update(app *module_api.AppCluster) error {
	var ipAddresses []net.IP

	for _, backend := range app.Backends {
		ip, err := net.ResolveIPAddr("ip", backend.Host)
		if err != nil {
			return err
		}
		ipAddresses = append(ipAddresses, ip.IP)
	}

	var reversed = module.makeDnsNameFromAppId(app.Id)
	var entry = &dbEntry{
		ipAddresses: ipAddresses,
		app:         app,
	}

	module.dbMutex.Lock()
	module.db[reversed] = entry
	module.dbMutex.Unlock()

	return nil
}

func (module *DNSModule) RemoveTask(task *module_api.AppBackend, app *module_api.AppCluster) {
	module.update(app)
}

func (module *DNSModule) dnsHandler(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)

	name := req.Question[0].Name
	name = strings.TrimSuffix(name, "."+module.BaseName)

	module.dbMutex.Lock()
	entry, ok := module.db[name]
	module.dbMutex.Unlock()

	if ok {
		switch req.Question[0].Qtype {
		case dns.TypeSRV:
			m.Answer = module.makeAllSRV(entry)
		case dns.TypeA:
			m.Answer = module.makeAllA(entry)
			if module.PushSRV {
				m.Extra = module.makeAllSRV(entry)
			}
		}
	}

	w.WriteMsg(m)
}

func (module *DNSModule) makeAllA(entry *dbEntry) []dns.RR {
	var result []dns.RR

	for _, ip := range entry.ipAddresses {
		rr := &dns.A{
			Hdr: dns.RR_Header{
				Ttl:    uint32(module.DNSTTL.Seconds()),
				Name:   module.BaseName,
				Class:  dns.ClassINET,
				Rrtype: dns.TypeA,
			},
			A: ip.To4(),
		}
		result = append(result, rr)
	}

	return result
}

func (module *DNSModule) makeAllSRV(entry *dbEntry) []dns.RR {
	var result []dns.RR

	for _, task := range entry.app.Backends {
		rr := &dns.SRV{
			Hdr: dns.RR_Header{
				Ttl:    uint32(module.DNSTTL.Seconds()),
				Name:   module.BaseName,
				Class:  dns.ClassINET,
				Rrtype: dns.TypeSRV,
			},
			Port:     uint16(task.Port),
			Target:   task.Host + ".",
			Weight:   1,
			Priority: 1,
		}
		result = append(result, rr)
	}

	return result
}

func (module *DNSModule) makeDnsNameFromAppId(appID string) string {
	var parts = strings.Split(appID, "/")[1:]
	var reversedParts []string
	for i := range parts {
		reversedParts = append(reversedParts, parts[len(parts)-i-1])
	}
	var reversed = strings.Join(reversedParts, ".")

	return reversed
}
