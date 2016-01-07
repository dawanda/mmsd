# Mesos Marathon Service Discovery Agent

(this is still in design / PoC phase)

### Goals

- generate load balancer configuration in realtime (with auto-reloading the load
  balancer)
- generate local per-service config files with actual endpoints listed
  for services that do not want to be load balanced by directly spoken to
- filter apps to be exposed by load balancer (or service files) via labels
  from marathon app definitions.
- provide simple rc scripts to run this agent (openrc/upstart/systemd)

### Start me Up

```!sh
MARATHON_HOST=localhost
MARATHON_PORT=8080

./marathon-service-discovery.rb $MARATHON_HOST $MARATHON_PORT
```

### Thoughts

- wrt. docker, handle haproxy container-locally?
