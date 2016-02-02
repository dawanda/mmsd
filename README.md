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

  --marathon-host=IP      Marathon IP
  --marathon-port=PORT    Marathon Port
  --groups=LIST           Comma seperated list of service groups to expose [*].
  --haproxy-bin=PATH      Path to haproxy binary [/usr/bin/haproxy]
  --haproxy-pidfile=PATH  Path to haproxy PID file [/var/run/haproxy.pid]
  --log-level=LEVEL       one of debug, info, warn, error, fatal [info]
  --upstream-confd=PATH   Path to runtime state dir containing
                          a file for each Marathon application with a
                          simple list of hostname:port pairs per line.

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

Label Name | Value  | Description
-----------|--------|-------------------------------------------------------
`proto`    | `APP_NAME` | an app type name that identifies the given service, such as redis, smtp, ...
`lb-proxy-protocol` | `1` \| `2` | Enables proxy-protocol to the backend communication. `1` enables proxy-protocol version 1 (clear text) whereas `2` enables version 2 (binary). Any other value does not activate proxy-protocol.
`lb-group` | `GROUP_NAME` | loadbalancer group this app should be exposed to
`lb-vhost` | `VHOST,...` | list of virtual hosts to be served on gateway port 80 and/or 443
`lb-vhost-default` | `1` | if set to 1, this HTTP application will serve as default application on port 80 and/or 443.

Possible `proto` values can be one of:

- `tcp` (default), TCP transport mode and simple TCP-connect health check
- `http` HTTP transport mode, with HTTP health check
- `redis` mode tcp and health check is using Redis text protocol
- `redis-master` same as `redis` but only masters will be healthy
- `redis-slave` same as `redis` but only slaves will be healthy
- ... any other interesting text protocols we can map into haproxy?
