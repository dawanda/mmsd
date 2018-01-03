# Mesos Marathon Service Discovery Agent

`mmsd` links your cloud together.

### Main features

- simple
- realtime update of runtime configuration state (haproxy, upstream-confd, ...)
- modular handlers
  - haproxy handler to manage a load balancer service
  - upstream-confd handler to manage local upstream config files per application
- filter apps to be exposed by load balancer (or service files) via labels
  from marathon app definitions.
- first-class docker support.
- DNS based service discovery (supports `A` and `SRV` query types).
- TODO: provide simple rc scripts to run this agent (openrc/upstart/systemd)

### Start me Up

```!sh
mmsd  --marathon-host=localhost --marathon-port=8080
```

### Docker Support
```!sh
# build mmsd docker container image
docker build -t mmsd .

# run mmsd docker container in background
docker run --net=host -d --name mmsd mmsd \
           --marathon-host=$YOUR_MARATHON_IP --marathon-port=8080
```

### Usage

```
Usage: mmsd [flags ...]

  --api-port uint
    MMSD API TCP port (default 8082)
  --bind-ip value
    IP address for handlers to bind (default 0.0.0.0)
  --dns-basename string
    DNS service discovery's base name (default "mmsd.")
  --dns-port uint
    DNS service discovery port (default 53)
  --dns-push-srv
    DNS service discovery to also push SRV on A
  --dns-ttl duration
    DNS service discovery's reply message TTL (default 5s)
  --enable-dns
    Enables DNS-based service discovery
  --enable-files
    enables file based service discovery (default true)
  --enable-gateway
    Enables gateway support
  --enable-health-checks
    Enable local health checks (if available) instead of relying on Marathon health checks alone. (default true)
  --enable-tcp
    enables haproxy TCP load balancing (default true)
  --enable-udp
    enables UDP load balancing (default true)
  --filter-groups string
    Application group filter (default "*")
  --gateway-bind value
    gateway bind address (default 0.0.0.0)
  --gateway-port-http uint
    gateway port for HTTP (default 80)
  --gateway-port-https uint
    gateway port for HTTPS (default 443)
  --haproxy-after-cmd string
    Command to execute after Haproxy start/reload
  --haproxy-before-cmd string
    Command to execute before Haproxy start/reload
  --haproxy-bin string
    path to haproxy binary (default "/usr/local/bin/haproxy")
  --haproxy-cfgtail string
    path to haproxy tail config file (default "/etc/mmsd/haproxy-tail.cfg")
  --haproxy-enable-reuse-socket
    Enable haproxy feature to share a socket for listing ports
  --haproxy-port uint
    haproxy management port (default 8081)
  --haproxy-reload-interval duration
    Interval between reload haproxy for bulk changes; default 5s (default 5s)
  --marathon-ip value
    Marathon endpoint TCP IP address (default 127.0.0.1)
  --marathon-port uint
    Marathon endpoint TCP port number (default 8080)
  --reconnect-delay duration
    Marathon reconnect delay (default 4s)
  --run-state-dir string
    Path to directory to keep run-state (default "/var/run/mmsd")
  -v, --verbose
    Set verbosity level
  -V, --version
    Shows version and exits
```

### Upstream Config Files

An application, such as `/developer/trapni/php` will be written
into a upstream-confd file with the name `developer.trapni.php.instances`
with the following content

```
Service-Name: token
Service-Port: number
Service-Transport-Proto: tcp | udp
Service-Application-Proto: http | redis | redis-master | ...
Health-Check-Proto: tcp | http

host1:port1
host2:port2
host3:port3
```

Where hostN:portN is the actual host (Mesos Slave) your application
has been spawned on.

Your application may read them upon startup and whenever this file changes
(in realtime) to always have an up-to-date list of address:port pairs
your other application is running on.

### Marathon Label Definitions

##### Label `proto` = `APP_PROTO`
an app type name that identifies the given service, such as redis, smtp, ...

##### Label `lb-accept-proxy` = `1`
Enables proxy-protocol on service port.

##### Label `lb-proxy-protocol` =  `1` \| `2`
Enables proxy-protocol to the backend communication. `1` enables proxy-protocol
version 1 (clear text) whereas `2` enables version 2 (binary).
Any other value does not activate proxy-protocol.

##### Label `lb-group` = `GROUP_NAME`
Load-balancer group this app should be exposed to

##### Label `lb-vhost` = `VHOST,...`
list of virtual hosts to be served on gateway port 80

##### Label `lb-vhost-default` = `PORT_INDEX`
if set, this HTTP application (at port index) will serve as default application on port 80.

##### Label `lb-vhost-ssl` = `VHOST,...`
list of vhosts to be proxied via SSL, with SNI enabled, but no SSL termination performed.

##### Label `lb-vhost-ssl-default` = `PORT_INDEX`
if set, this HTTPS application (at port index) will serve as default application
on the application gateway's SSL port (usually 443)

##### Label `proto`

- `tcp` (default), TCP transport mode and simple TCP-connect health check
- `http` HTTP transport mode, with HTTP health check
- `smtp` SMTP protococol, enables SMTP-restrictive health check.
- `redis` mode tcp and health check is using Redis text protocol
- `redis-master` same as `redis` but only masters will be healthy
- `redis-slave` same as `redis` but only slaves will be healthy
- ... any other interesting text protocols we can map into haproxy?

### Service Discovery HTTP endpoint

- `/v1/apps` retrieves the list of all apps in Marathon, one app name by line
- `/v1/instances/NAME` retrieves list of instances by hostname:port tuple in
  each line for given application path, for example:
  `/v1/instances/production/sqltap1`


### Changelog

#### Version 0.12.0

New flags:

- `--haproxy-enable-reuse-socket`


#### Version 0.11.0

Following flags are removed:

- `--haproxy-bind`
- `--managed-ip`

New flags:

- `--api-port`
- `--bind-ip`
- `--haproxy-before-cmd`
- `--haproxy-after-cmd`
