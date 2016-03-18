// Package udpproxy provides an API for proxying UDP messages.
package udpproxy

import (
	"fmt"
	"log"
	"net"
	"time"
)

// Scheduler algorithm to use for proxying the UDP message.
type Scheduler int

const (
	// RoundRobin is the round-robin scheduler
	RoundRobin Scheduler = 1

	// Multicast (aka. fanout) scheduler. sends message to *all* backends.
	Multicast Scheduler = 2
)

func (s Scheduler) String() string {
	switch s {
	case RoundRobin:
		return "RoundRobin"
	case Multicast:
		return "Multicast"
	default:
		return fmt.Sprintf("%d", s)
	}
}

// Backend is the UDP backend
type Backend struct {
	Name             string
	Addr             *net.UDPAddr
	MessageSentCount uint
	MessageRecvCount uint
	Responder        chan []byte
	Touched          bool
	// Write chan string
}

// NewBackend spawns a new UDP backend.
func NewBackend(name string, addr string, scheduler Scheduler) (*Backend, error) {
	beAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	be := &Backend{Name: name, Addr: beAddr, Touched: false}

	return be, err
}

// Touch touches the given backend.
func (be *Backend) Touch() {
	be.Touched = true
}

// CreateConnection spawnes a new UDP connection for given Backend.
func (be *Backend) CreateConnection() (*net.UDPConn, error) {
	conn, err := net.DialUDP("udp", nil, be.Addr)
	return conn, err
}

// Send sends a single UDP message to this backend.
func (be *Backend) Send(msg []byte, sender *net.UDPAddr, fe *Frontend) error {
	be.MessageSentCount++

	// create BE connection
	conn, err := be.CreateConnection()
	if err != nil {
		return err
	}
	defer conn.Close()

	// send message to BE
	log.Printf("[%v] Sending message to backend %v. \"%v\"\n", fe.Name, be.Name, string(msg))
	_, err = conn.Write(msg)
	if err != nil {
		return err
	}

	if fe.ResponseTimeout == 0 {
		return nil
	}

	// read response(s) from BE
	errorChannel := make(chan error)
	responseChannel := make(chan []byte)

	for {
		go func() {
			var buf = make([]byte, 16*1024)
			n, err := conn.Read(buf)
			if err != nil {
				errorChannel <- err
			} else {
				responseChannel <- buf[0:n]
			}
		}()

		select {
		case msg := <-responseChannel:
			// response received
			log.Printf("[%v.%v] Response received. \"%v\"\n", fe.Name, be.Name, string(msg))
			fe.Conn.WriteToUDP(msg, sender)
		case <-time.After(fe.ResponseTimeout):
			// operation timed out
			log.Printf("Closing upstream connection for sender %v.", sender)
			return nil
		}
	}
}

func (be Backend) String() string {
	return fmt.Sprintf("%v: %v", be.Name, be.Addr)
}

// Session represents a UDP communication session
type Session struct {
	LastActive  time.Time
	Backend     *Backend
	RequestBuf  string
	ResponseBuf string
}

// Frontend represents a UDP frontend
type Frontend struct {
	Name            string
	Conn            *net.UDPConn
	Scheduler       Scheduler
	Backends        []*Backend
	ResponseTimeout time.Duration
	//Sessions map[net.Addr]Session
}

// NewFrontend spawns a new UDP frontend.
func NewFrontend(name, addr string, sched Scheduler) (*Frontend, error) {
	udpaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", udpaddr)
	if err != nil {
		return nil, err
	}

	fe := &Frontend{
		Name:            name,
		Conn:            conn,
		Scheduler:       sched,
		ResponseTimeout: time.Second * 1,
	}

	return fe, nil
}

// Close releases underlying sockets to given UDP frontend.
func (fe *Frontend) Close() {
	fe.Conn.Close()
}

// ClearTouch clears out any touch-flags on all backends to this frontend.
func (fe *Frontend) ClearTouch() {
	for _, be := range fe.Backends {
		be.Touched = false
	}
}

// RemoveUntouched kills all backends that haven't been touched with Touch().
func (fe *Frontend) RemoveUntouched() {
	var touched []*Backend

	for _, be := range fe.Backends {
		if be.Touched {
			touched = append(touched, be)
		} else {
			log.Printf("Removing UDP backend %v %v.\n", be.Addr, be.Name)
		}
	}

	fe.Backends = touched
}

// AddBackend extends given UDP Frontend with a new backend.
func (fe *Frontend) AddBackend(name, addr string) (*Backend, error) {
	for _, be := range fe.Backends {
		if be.Name == name {
			return be, nil
		}
	}

	be, err := NewBackend(name, addr, fe.Scheduler)
	if err != nil {
		return nil, err
	}

	log.Printf("UDP %v adding backend %v %v.\n", fe.Name, be.Addr, name)
	fe.Backends = append(fe.Backends, be)
	return be, nil
}

// Serve serves given UDP frontend on current thread. Blocking.
func (fe *Frontend) Serve() {
	for {
		msg := make([]byte, 16*1024)
		n, saddr, err := fe.Conn.ReadFromUDP(msg)
		if err != nil {
			log.Printf("[%v] [%v] Failed to read. %v\n", fe.Name, saddr, err)
			return
		}

		msg = msg[0:n]

		fmt.Printf("[%v] [%v] Received \"%v\"\n", fe.Name, saddr, string(msg))

		switch fe.Scheduler {
		case RoundRobin:
			go fe.RoundRobin(msg, saddr)
		case Multicast:
			go fe.Multicast(msg, saddr)
		}
	}
}

// Multicast sends the given message to this fronend to all backends.
func (fe *Frontend) Multicast(msg []byte, sender *net.UDPAddr) {
	for _, be := range fe.Backends {
		go be.Send(msg, sender, fe)
	}
}

// RoundRobin sends the given message in round-robin fasion to exactly one backend.
func (fe *Frontend) RoundRobin(msg []byte, sender *net.UDPAddr) {
	be := fe.Backends[0]
	err := be.Send(msg, sender, fe)

	if err != nil {
		log.Printf("[%v] [%v] Error sending message to upstream. %v\n", fe.Name, sender, err)
	}
}

// String high-level view of this object as string.
func (fe *Frontend) String() string {
	var sched string
	switch fe.Scheduler {
	case RoundRobin:
		sched = "RR"
	case Multicast:
		sched = "MC"
	default:
		sched = "unknown"
	}

	return fmt.Sprintf("Frontend{%v, %v, %v, %v}",
		fe.Name, fe.Conn.LocalAddr(), sched, fe.Backends)
}

// Proxy represents a generic UDP proxy for many UDP frontends
type Proxy struct {
	// listen on downstreams and pass them upstream
	Frontends   map[string]*Frontend
	quitChannel chan bool
}

// NewProxy spawns a new UDP proxy.
func NewProxy() *Proxy {
	return &Proxy{
		Frontends: make(map[string]*Frontend),
	}
}

// AddOrReplaceFrontend does what it says. ;-)
func (proxy *Proxy) AddOrReplaceFrontend(name, addr string, sched Scheduler) (*Frontend, error) {
	if fe, found := proxy.Frontends[addr]; found == true {
		// first close and wipe the frontend, so we can recreate it
		fe.Close()
		delete(proxy.Frontends, addr)
	}

	return proxy.AddFrontend(name, addr, sched)
}

// AddFrontend does what it says ;-)
func (proxy *Proxy) AddFrontend(name, addr string, sched Scheduler) (*Frontend, error) {
	frontend, err := NewFrontend(name, addr, sched)
	if err != nil {
		return nil, err
	}
	proxy.Frontends[addr] = frontend
	return frontend, nil
}

// Dump dumps the UDP proxy to stdout.
func (proxy *Proxy) Dump() {
	fmt.Printf("UdpProxy: %v frontends:\n", len(proxy.Frontends))
	for i, fe := range proxy.Frontends {
		fmt.Printf(" %02v:  %v\n", i, fe)
	}
}

// Serve serves all frontends in backend and waits for them to terminate.
func (proxy *Proxy) Serve() {
	for _, frontend := range proxy.Frontends {
		go frontend.Serve()
	}

	<-proxy.quitChannel
}

// Quit triggers a termination of Serve()
func (proxy *Proxy) Quit() {
	proxy.quitChannel <- true
}
