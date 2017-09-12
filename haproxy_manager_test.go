package main

import (
	"testing"
)

func TestUpstreamServerConfig(t *testing.T) {
	var tests = []struct {
		host       string
		port       uint
		serverOpts string
		result     string
	}{
		{
			"localhost",
			9999,
			" check",
			"  server localhost:9999 127.0.0.1:9999 check\n",
		},
	}

	for _, test := range tests {
		conf := upstreamServerConfig(test.host, test.port, test.serverOpts)
		if conf != test.result {
			t.Errorf("Upstream server config does not match '%s' != '%s'", conf, test.result)
		}
	}
}
