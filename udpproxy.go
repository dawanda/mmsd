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

// ----------------------------------------------------------------------------
type Backend struct {
	Name             string
	Addr             *net.UDPAddr
	MessageSentCount uint
	MessageRecvCount uint
	Responder        chan []byte
	// Write chan string
}

func NewBackend(name string, addr string, scheduler Scheduler) (*Backend, error) {
	beAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	be := &Backend{Name: name, Addr: beAddr}

	return be, err
}

func (be *Backend) CreateConnection() (*net.UDPConn, error) {
	conn, err := net.DialUDP("udp", nil, be.Addr)
	return conn, err
}

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

func (be Backend) String() string {
	return fmt.Sprintf("%v: %v", be.Name, be.Addr)
}

// ----------------------------------------------------------------------------
type Session struct {
	LastActive  time.Time
	Backend     *Backend
	RequestBuf  string
	ResponseBuf string
}

// ----------------------------------------------------------------------------
type Frontend struct {
	Name            string
	Conn            *net.UDPConn
	Scheduler       Scheduler
	Backends        []*Backend
	ResponseTimeout time.Duration
	//Sessions map[net.Addr]Session
}

func NewFrontend(name, addr string, sched Scheduler) (*Frontend, error) {
	udpaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", udpaddr)
	if err != nil {
		return nil, err
	}

	return &Frontend{Name: name, Conn: conn, Scheduler: sched, ResponseTimeout: time.Second * 1}, nil
}

func (fe *Frontend) Close() {
	fe.Conn.Close()
}

func (fe *Frontend) AddBackend(name, addr string) (*Backend, error) {
	be, err := NewBackend(name, addr, fe.Scheduler)
	if err != nil {
		return nil, err
	}

	fe.Backends = append(fe.Backends, be)
	return be, nil
}

func (fe *Frontend) Serve() {
	for {
		msg := make([]byte, 16*1024)
		n, saddr, err := fe.Conn.ReadFromUDP(msg)
		if err != nil {
			log.Printf("[%v] [%v] Failed to read. %v\n", fe.Name, saddr, fe.Conn.LocalAddr())
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

func (fe *Frontend) Multicast(msg []byte, sender *net.UDPAddr) {
	for _, be := range fe.Backends {
		go be.Send(msg, sender, fe)
	}
}

func (fe *Frontend) RoundRobin(msg []byte, sender *net.UDPAddr) {
	be := fe.Backends[0]
	err := be.Send(msg, sender, fe)

	if err != nil {
		log.Printf("[%v] [%v] Error sending message to upstream. %v\n", fe.Name, sender, err)
	}
}

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

// ----------------------------------------------------------------------------
type UdpProxy struct {
	// listen on downstreams and pass them upstream
	Frontends   map[string]*Frontend
	quitChannel chan bool
}

func NewUdpProxy() *UdpProxy {
	return &UdpProxy{
		Frontends: make(map[string]*Frontend),
	}
}

func (proxy *UdpProxy) AddOrReplaceFrontend(name, addr string, sched Scheduler) (*Frontend, error) {
	if fe, found := proxy.Frontends[addr]; found == true {
		// first close and wipe the frontend, so we can recreate it
		fe.Close()
		delete(proxy.Frontends, addr)
	}

	return proxy.AddFrontend(name, addr, sched)
}

func (proxy *UdpProxy) AddFrontend(name, addr string, sched Scheduler) (*Frontend, error) {
	frontend, err := NewFrontend(name, addr, sched)
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
