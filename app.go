package main

// Marathon independant application definitions

type AppCluster struct {
	Name        string
	Id          string
	ServicePort uint
	Protocol    string
	PortName    string
	Labels      map[string]string
	HealthCheck *AppHealthCheck
	Backends    []AppBackend
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
