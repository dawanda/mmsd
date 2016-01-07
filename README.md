# Mesos Marathon Service Discovery Agent

### Goals

- generate load balancer configuration in realtime (with auto-reloading the load
  balancer)
- generate local per-service config files with actual endpoints listed
  for services that do not want to be load balanced by directly spoken to
- filter apps to be exposed by load balancer (or service files) via labels
  from marathon app definitions.
- provide simple rc scripts to run this agent (openrc/upstart/systemd)

### Thoughts

- wrt. docker, handle haproxy container-locally?
