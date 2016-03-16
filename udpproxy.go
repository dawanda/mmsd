package main

import (
	"fmt"
	"log"
	"net"
	"time"
)

type Scheduler int

const (
	RoundRobin Scheduler = 1
	Multicast  Scheduler = 2
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

// ----------------------------------------------------------------------------
type UdpBackend struct {
	Name             string
	Addr             *net.UDPAddr
	MessageSentCount uint
	MessageRecvCount uint
	Responder        chan []byte
	Touched          bool
	// Write chan string
}

func NewUdpBackend(name string, addr string, scheduler Scheduler) (*UdpBackend, error) {
	beAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	be := &UdpBackend{Name: name, Addr: beAddr, Touched: false}

	return be, err
}

func (be *UdpBackend) Touch() {
	be.Touched = true
}

func (be *UdpBackend) CreateConnection() (*net.UDPConn, error) {
	conn, err := net.DialUDP("udp", nil, be.Addr)
	return conn, err
}

func (be *UdpBackend) Send(msg []byte, sender *net.UDPAddr, fe *UdpFrontend) error {
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
			var buf []byte = make([]byte, 16*1024)
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

	return nil
}

func (be UdpBackend) String() string {
	return fmt.Sprintf("%v: %v", be.Name, be.Addr)
}

// ----------------------------------------------------------------------------
type Session struct {
	LastActive  time.Time
	Backend     *UdpBackend
	RequestBuf  string
	ResponseBuf string
}

// ----------------------------------------------------------------------------
type UdpFrontend struct {
	Name            string
	Conn            *net.UDPConn
	Scheduler       Scheduler
	Backends        []*UdpBackend
	ResponseTimeout time.Duration
	//Sessions map[net.Addr]Session
}

func NewUdpFrontend(name, addr string, sched Scheduler) (*UdpFrontend, error) {
	udpaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", udpaddr)
	if err != nil {
		return nil, err
	}

	fe := &UdpFrontend{
		Name:            name,
		Conn:            conn,
		Scheduler:       sched,
		ResponseTimeout: time.Second * 1,
	}

	return fe, nil
}

func (fe *UdpFrontend) Close() {
	fe.Conn.Close()
}

func (fe *UdpFrontend) ClearTouch() {
	for _, be := range fe.Backends {
		be.Touched = false
	}
}

func (fe *UdpFrontend) RemoveUntouched() {
	var touched []*UdpBackend

	for _, be := range fe.Backends {
		if be.Touched {
			touched = append(touched, be)
		} else {
			log.Printf("Removing UDP backend %v %v.\n", be.Addr, be.Name)
		}
	}

	fe.Backends = touched
}

func (fe *UdpFrontend) AddBackend(name, addr string) (*UdpBackend, error) {
	for _, be := range fe.Backends {
		if be.Name == name {
			return be, nil
		}
	}

	be, err := NewUdpBackend(name, addr, fe.Scheduler)
	if err != nil {
		return nil, err
	}

	log.Printf("Adding UDP backend %v %v.\n", be.Addr, name)
	fe.Backends = append(fe.Backends, be)
	return be, nil
}

func (fe *UdpFrontend) Serve() {
	for {
		msg := make([]byte, 16*1024)
		n, saddr, err := fe.Conn.ReadFromUDP(msg)
		if err != nil {
			log.Printf("[%v] [%v] Failed to read. %v %v\n", fe.Name, saddr, err)
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

func (fe *UdpFrontend) Multicast(msg []byte, sender *net.UDPAddr) {
	for _, be := range fe.Backends {
		go be.Send(msg, sender, fe)
	}
}

func (fe *UdpFrontend) RoundRobin(msg []byte, sender *net.UDPAddr) {
	be := fe.Backends[0]
	err := be.Send(msg, sender, fe)

	if err != nil {
		log.Printf("[%v] [%v] Error sending message to upstream. %v\n", fe.Name, sender, err)
	}
}

func (fe *UdpFrontend) String() string {
	var sched string
	switch fe.Scheduler {
	case RoundRobin:
		sched = "RR"
	case Multicast:
		sched = "MC"
	default:
		sched = "unknown"
	}

	return fmt.Sprintf("UdpFrontend{%v, %v, %v, %v}",
		fe.Name, fe.Conn.LocalAddr(), sched, fe.Backends)
}

// ----------------------------------------------------------------------------
type UdpProxy struct {
	// listen on downstreams and pass them upstream
	Frontends   map[string]*UdpFrontend
	quitChannel chan bool
}

func NewUdpProxy() *UdpProxy {
	return &UdpProxy{
		Frontends: make(map[string]*UdpFrontend),
	}
}

func (proxy *UdpProxy) AddOrReplaceFrontend(name, addr string, sched Scheduler) (*UdpFrontend, error) {
	if fe, found := proxy.Frontends[addr]; found == true {
		// first close and wipe the frontend, so we can recreate it
		fe.Close()
		delete(proxy.Frontends, addr)
	}

	return proxy.AddFrontend(name, addr, sched)
}

func (proxy *UdpProxy) AddFrontend(name, addr string, sched Scheduler) (*UdpFrontend, error) {
	frontend, err := NewUdpFrontend(name, addr, sched)
	if err != nil {
		return nil, err
	}
	proxy.Frontends[addr] = frontend
	return frontend, nil
}

func (proxy *UdpProxy) Dump() {
	fmt.Printf("UdpProxy: %v frontends:\n", len(proxy.Frontends))
	for i, fe := range proxy.Frontends {
		fmt.Printf(" %02v:  %v\n", i, fe)
	}
}

func (proxy *UdpProxy) Serve() {
	for _, frontend := range proxy.Frontends {
		go frontend.Serve()
	}

	<-proxy.quitChannel
}

func (proxy *UdpProxy) Quit() {
	proxy.quitChannel <- true
}
