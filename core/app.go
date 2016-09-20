package core

// Marathon independant application definitions

type AppCluster struct {
	Name        string            // Marathon's human reaable AppId (i.e. /path/to/app)
	Id          string            // globally unique ID that identifies this application
	ServicePort uint              // service port to listen to in service discovery
	Protocol    string            // service protocol
	PortName    string            // user filled port name
	Labels      map[string]string // key/value pairs of labels (port-local | global)
	HealthCheck *AppHealthCheck   // healthcheck, if available, or nil
	Backends    []AppBackend      // ordered list of backend tasks
	PortIndex   int               // Marathon's application port index
}

type AppHealthCheck struct {
	Protocol               string
	Path                   string
	Command                *string
	GracePeriodSeconds     uint
	IntervalSeconds        uint
	TimeoutSeconds         uint
	MaxConsecutiveFailures uint
	IgnoreHttp1xx          bool
}

type AppBackend struct {
	Id    string
	Host  string
	Port  uint
	State string
}
