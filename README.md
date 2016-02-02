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
mmsd [options]

  --marathon-host=IP        Marathon IP
  --marathon-port=PORT      Marathon Port
  --filter-groups=LIST      Comma seperated list of service groups to expose [*].
  --haproxy-bin=PATH        Path to haproxy binary [/usr/bin/haproxy]
  --haproxy-pidfile=PATH    Path to haproxy PID file [/var/run/haproxy.pid]
  --haproxy-cfg=PATH        Path to haproxy.cfg [/var/run/haproxy.cfg]
  --haproxy-bind=IP         Default IP bind [0.0.0.0]
  --haproxy-port=PORT       haproxy TCP port to the management interface.
  --enable-gateway          Enables HTTP(S) gateway. Disabled by default.
  --gateway-http-port=PORT  HTTP gateway port, enables HTTP gateway on given
                            port to proxy incoming HTTP requests to the
                            application by its HTTP request host header.
  --gateway-https-port=PORT HTTPS gateway port, enables HTTPS gateway on given 
                            port to proxy incoming HTTP requests to the
                            application by its HTTP request host header.
  --upstream-confd=PATH     Path to runtime state dir containing
                            a file for each Marathon application with a
                            simple list of hostname:port pairs per line.
  --log-level=LEVEL         one of debug, info, warn, error, fatal [info]

Every commandline parameter can be also specified as environment variable,
however, the command line argument takes precedence.
Environment variables are upper case, without leading dashes, and mid-dashes
represented as underscores.
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
