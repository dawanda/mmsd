package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

/*
	stream        = [ bom ] *event
	event         = *( comment / field ) end-of-line
	comment       = colon *any-char end-of-line
	field         = 1*name-char [ colon [ space ] *any-char ] end-of-line
	end-of-line   = ( cr lf / cr / lf )

	; characters
	lf            = %x000A ; U+000A LINE FEED (LF)
	cr            = %x000D ; U+000D CARRIAGE RETURN (CR)
	space         = %x0020 ; U+0020 SPACE
	colon         = %x003A ; U+003A COLON (:)
	bom           = %xFEFF ; U+FEFF BYTE ORDER MARK
	name-char     = %x0000-0009 / %x000B-000C / %x000E-0039 / %x003B-10FFFF
									; a Unicode character other than U+000A LINE FEED (LF), U+000D CARRIAGE RETURN (CR), or U+003A COLON (:)
	any-char      = %x0000-0009 / %x000B-000C / %x000E-10FFFF
									; a Unicode character other than U+000A LINE FEED (LF) or U+000D CARRIAGE RETURN (CR)
*/

type EventHandler func(string, string)

const (
	CONNECTING int = 0
	OPEN       int = 1
	CLOSED     int = 2
)

// Server Sent Events, in Go ;-)

type EventSource struct {
	Url            string
	ReadyState     int
	Handlers       map[string]EventHandler
	OnOpen         EventHandler
	OnMessage      EventHandler
	OnError        EventHandler
	ReconnectDelay time.Duration
	LastEventID    string
}

func NewEventSource(url string, reconnectDelay time.Duration) *EventSource {
	var sse = &EventSource{Url: url,
		ReadyState:     CONNECTING,
		ReconnectDelay: reconnectDelay,
		Handlers:       make(map[string]EventHandler)}

	return sse
}

func (sse *EventSource) AddEventListener(eventType string, cb func(string)) {
	sse.Handlers[eventType] = func(_, data string) { cb(data) }
}

func (sse *EventSource) dispatchEvent(event, data string) {
	switch sse.ReadyState {
	case CLOSED:
		return
	case OPEN:
		break
	default:
		sse.ReadyState = OPEN
	}

	if sse.Handlers[event] != nil {
		sse.Handlers[event](event, data)
	} else if sse.OnMessage != nil {
		sse.OnMessage(event, data)
	}
}

func (sse *EventSource) Run() {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	var client = &http.Client{tr, nil, nil, 0 * time.Second}
	req, err := http.NewRequest("GET", sse.Url, nil)
	if err != nil {
		if sse.OnError != nil {
			sse.OnError("error", err.Error())
		}
		return
	}

	req.Header.Add("Accept", "text/event-stream")
	req.Header.Add("Cache-Control", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		if sse.OnError != nil {
			sse.OnError("error", err.Error())
		}
		return
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		err := fmt.Errorf("SSE: unexpected response status code: %v\n", resp.StatusCode)
		if sse.OnError != nil {
			sse.OnError("error", err.Error())
		}
		return
	}

	if sse.OnOpen != nil {
		sse.OnOpen("open", "Connected.")
	}

	reader := bufio.NewReader(resp.Body)

	var (
		eventType  string // "event"
		dataBuffer []byte // "data"
	)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if sse.OnError != nil {
				sse.OnError("error", err.Error())
			}
			return
		}

		line = strings.Trim(line, "\n\r")

		if len(line) == 0 {
			if len(eventType) != 0 || len(dataBuffer) != 0 {
				sse.dispatchEvent(eventType, string(dataBuffer))
				dataBuffer = dataBuffer[0:0]
				eventType = ""
			}
		} else if line[0] != ':' {
			var (
				name  string
				value string
			)
			if strings.Contains(line, ":") {
				args := strings.SplitN(line, ": ", 2)
				name = args[0]
				value = args[1]
			} else {
				name = line
			}

			switch name {
			case "event":
				eventType = value
			case "data":
				dataBuffer = append(dataBuffer, []byte(value)...)
				dataBuffer = append(dataBuffer, 0x0D)
			case "id":
				sse.LastEventID = value
			case "retry":
				if number, err := strconv.Atoi(value); err == nil {
					sse.ReconnectDelay = time.Millisecond * time.Duration(number)
				}
			}
		}
	}
}

func (sse *EventSource) RunForever() {
	sse.Run()

	for sse.ReadyState != CLOSED {
		sse.ReadyState = CONNECTING

		if sse.OnError != nil {
			sse.OnError("error", "Reconnecting")
		}

		if sse.ReconnectDelay != 0 {
			time.Sleep(sse.ReconnectDelay)
		}

		sse.Run()
	}
}

func (sse *EventSource) Close() {
	sse.ReadyState = CLOSED

	// TODO: wakeup run-loop if active
}
