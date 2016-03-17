#!/usr/bin/env ruby
# The MIT License (MIT)
# Copyright (c) 2016 Christian Parpart (DaWanda GmbH) <christian@dawanda.com>
#
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
#
# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.

require 'rubygems'
require 'em-eventsource'
require 'net/http'
require 'fileutils'
require 'logger'
require 'json'

# The label name that is identifying group filters.
# The value will be a comma seperated list of groups.
FILTER_GROUP_NAME = 'lb-group'
LB_ACCEPT_PROXY = 'lb-accept-proxy'
LB_PROXY_PROTOCOL = 'lb-proxy-protocol'
LB_VHOST = 'lb-vhost'
LB_VHOST_DEFAULT = 'lb-vhost-default'
LB_VHOST_SSL = 'lb-vhost-ssl'
LB_VHOST_SSL_DEFAULT = 'lb-vhost-ssl-default'
LB_SERVICE_PORT = 'lb-service-port'

class Helper # {{{
 public
  @@ipv4_cache = {}

  def self.resolve_ipv4(fqdn)
    @@ipv4_cache[fqdn] ||= `host "#{fqdn}" | awk '{print $NF}'`.strip
    @@ipv4_cache[fqdn]
  end

  def self.prettify_dns_name(name)
    # name.gsub(`domainname`.strip, '').gsub(/\.$/, '')
    name.split('.')[0]
  end

  def self.prettify_app_id(app_id, service_port)
    app_id = app_id.slice(1, app_id.length - 1)
    app_id = app_id.gsub('/', '.')
    app_id = "#{app_id}-#{service_port}"
  end

  def self.is_root_user?
    `id -u`.to_i == 0
  end
end # }}}
# {{{ app instance health check
class HealthCheck
  attr_accessor :protocol, :interval, :http_path

  def initialize(protocol, interval, http_path)
    @protocol = protocol # http, tcp, redis, redis-master, redis-slave, ...
    @interval = interval
    @http_path = http_path
  end
end
# }}}
class AppInstance # {{{
  attr_accessor :name, :host, :port

  def initialize(name, host, port)
    @name = name
    @host = host
    @port = port
  end
end
# }}}
class AppCluster # {{{
  attr_accessor :app_id, :service_port, :protocol, :health_check, :labels,
                :vhosts_ssl, :vhost_ssl_default, :vhosts, :vhost_default,
                :accept_proxy, :proxy_protocol, :instances

  def initialize(app_id, service_port, protocol, health_check, labels,
                 vhosts_ssl, vhost_ssl_default, vhosts, vhost_default,
                 accept_proxy, proxy_protocol, instances)
    @app_id = app_id
    @service_port = service_port
    @protocol = protocol
    @health_check = health_check
    @labels = labels
    @vhosts_ssl = vhosts_ssl
    @vhost_ssl_default = vhost_ssl_default
    @vhosts = vhosts
    @vhost_default = vhost_default
    @accept_proxy = accept_proxy
    @proxy_protocol = proxy_protocol
    @instances = instances
  end

  def app_protocol
    if labels['proto']
      labels['proto']
    elsif health_check
      health_check.protocol
    else
      protocol
    end
  end
end
# }}}
class HaproxyBuilder # {{{
  attr_accessor :binpath, :cfgfile, :cfgtail, :pidfile, :bindaddr, :logger,
                :gateway_http_port, :gateway_https_port

  def initialize(binpath, cfgfile, cfgtail, pidfile, bindaddr, port,
                 gateway_http_port, gateway_https_port, logger)
    @binpath = binpath
    @cfgfile = cfgfile
    @cfgtail = cfgtail
    @pidfile = pidfile
    @bindaddr = bindaddr
    @port = port
    @gateway_http_port = gateway_http_port
    @gateway_https_port = gateway_https_port
    @logger = logger
  end

  def apply(clusters, force)
    tmpfile = "#{@cfgfile}.tmp"

    write_config(tmpfile, clusters)
    check_haproxy(tmpfile)

    if !File.exist?(@cfgfile) then
      logger.debug "File new. #{@cfgfile} reloaded."
      FileUtils.mv(tmpfile, @cfgfile)
      ensure_haproxy
    elsif !FileUtils.identical?(tmpfile, @cfgfile) then
      logger.debug "File changed. #{@cfgfile} reloaded."
      FileUtils.rm(@cfgfile)
      FileUtils.mv(tmpfile, @cfgfile)
      ensure_haproxy
    else
      logger.debug "Nothing changed. #{@cfgfile} not reloaded."
      FileUtils.rm(tmpfile)
      ensure_haproxy if force
    end
  rescue => ex
    logger.error "#{ex.backtrace}: #{ex.message} (#{ex.class})"
  end

  def valid_pid?(pid)
    Process.kill(0, pid)
    # pid = Process.spawn(status_cmd)
    # Process.wait(pid)
    # pid, status = Process.waitpid2(pid)
    # status.exitstatus == 0
  rescue => bang
    logger.debug "Error while testing for valid PID #{pid}. #{bang}"
    false
  end

  def kill_process(pid)
    Process.kill(:SIGKILL, pid)
  rescue => bang
    logger.debug "Error while killing PID #{pid}. #{bang}"
  end

  def check_haproxy(cfgfile)
    args = [@binpath, '-f', cfgfile, '-p', @pidfile, '-c']
    logger.info "Checking haproxy configuration. #{args.join(' ')}"

    io = IO.popen(args.join(' '))
    output = io.read.strip
    io.close
    status = $?.exitstatus

    if !output.empty? then
      if status != 0 then
        logger.error "#{output}"
      else
        logger.info "#{output}"
      end
    end

    status
  end

  def start_haproxy
    args = [@binpath, '-f', @cfgfile, '-p', @pidfile, '-D', '-q']
    logger.info "Starting haproxy. #{args.join(' ')}"
    io = IO.popen(args.join(' '))
    output = io.read
    io.close
    logger.debug "start haproxy program output: #{output}" unless output.empty?
  end

  def reload_haproxy(pid)
    args = [@binpath, '-D', '-p', @pidfile, '-f', @cfgfile, '-sf', pid]
    logger.info "Reloading haproxy. #{args.join(' ')}"
    io = IO.popen(args.join(' '))
    output = io.read
    io.close
    logger.debug "reload haproxy program output: #{output}" unless output.empty?
  end

  def ensure_haproxy
    if File.exist?(@pidfile) then
      pid = File.read(@pidfile).strip.to_i
      if valid_pid?(pid) then
        reload_haproxy(pid)
      else
        logger.info "PID file #{@pidfile} contains an invalid PID #{pid}."
        kill_process(pid)
        start_haproxy
      end
    else
      start_haproxy
    end
  end

  def write_config_mgnt(file, bindaddr, port)
    file.write "listen haproxy\n"
    file.write "  bind #{bindaddr}:#{port}\n"
    file.write "  mode http\n"
    file.write "  stats enable\n"
    file.write "  stats uri /\n"
    file.write "  stats admin if TRUE\n"
    file.write "  monitor-uri /haproxy?monitor\n"
    file.write "\n"
  end

  def write_config_http(file, clusters, bindaddr, port)
    file.write "frontend __gateway_http\n"
    file.write "  bind #{bindaddr}:#{port}\n"
    file.write "  mode http\n"
    file.write "  option http-server-close\n"
    file.write "  reqadd X-Forwarded-Proto:\\ http\n"
    file.write "\n"

    suffix_routes = {}
    suffix_matches = []

    exact_routes = {}
    exact_matches = []

    vhost_default = ''

    clusters.each do |app_id, cluster|
      app_id = Helper.prettify_app_id(app_id, cluster.service_port)

      cluster.vhosts.each do |vhost|
        match_token = "vhost_#{vhost.gsub('.', '_').gsub('*', 'STAR')}"
        if vhost =~ /^\*\./ then
          suffix_matches << "  acl #{match_token} hdr_dom(host) -i #{vhost.split('.', 2)[1]}\n"
          suffix_routes[match_token] = app_id
        else
          exact_matches << "  acl #{match_token} hdr(host) -i #{vhost}:#{port}\n"
          exact_routes[match_token] = app_id
        end
      end

      vhost_default = app_id if cluster.vhost_default
    end

    exact_matches.each {|line| file.write line}
    suffix_matches.each {|line| file.write line}
    file.write "\n" unless suffix_matches.empty? && exact_matches.empty?

    exact_routes.each {|acl, app| file.write "  use_backend #{app} if #{acl}\n"}
    suffix_routes.each {|acl, app| file.write "  use_backend #{app} if #{acl}\n"}
    file.write "\n"

    if vhost_default != "" then
      file.write "  default_backend #{vhost_default}\n"
      file.write "\n"
    end
  end

  # helper: http://serverfault.com/questions/662662/haproxy-with-sni-and-different-ssl-settings
  def write_config_https(file, clusters, bindaddr, port)
    # SNI vhost selector
    suffix_routes = {}
    suffix_matches = []
    exact_routes = {}
    exact_matches = []
    vhost_default = ''
    clusters.each do |app_id, cluster|
      app_id = Helper.prettify_app_id(app_id, cluster.service_port)

      cluster.vhosts_ssl.each do |vhost|
        match_token = "vhost_ssl_#{vhost.gsub('.', '_').gsub('*', 'STAR')}"
        if vhost =~ /^\*\./ then
          suffix_matches << "  acl #{match_token} req_ssl_sni -m dom #{vhost.split('.', 2)[1]}\n"
          suffix_routes[match_token] = app_id
        else
          exact_matches << "  acl #{match_token} req_ssl_sni -i #{vhost}\n"
          exact_routes[match_token] = app_id
        end
      end

      vhost_default = app_id if cluster.vhost_ssl_default
    end

    return if exact_matches.empty? || suffix_matches.empty?

    # write header
    file.write "frontend __gateway_https\n"
    file.write "  bind #{bindaddr}:#{port}\n"
    file.write "  mode tcp\n"
    file.write "  tcp-request inspect-delay 5s\n"
    file.write "  tcp-request content accept if { req_ssl_hello_type 1 }\n"
    file.write "\n"

    # write ACL statements
    exact_matches.each {|line| file.write line}
    suffix_matches.each {|line| file.write line}
    file.write "\n" unless suffix_matches.empty? && exact_matches.empty?

    # write use_backend statements
    exact_routes.each {|acl, app| file.write "  use_backend #{app} if #{acl}\n"}
    suffix_routes.each {|acl, app| file.write "  use_backend #{app} if #{acl}\n"}
    file.write "\n"

    if vhost_default != "" then
      file.write "  default_backend #{vhost_default}\n"
      file.write "\n"
    end
  end

  def write_config(filename, clusters)
    logger.debug "HaproxyBuilder.write_config(\"#{filename}\")"
    file = File.open(filename, File::CREAT | File::TRUNC | File::WRONLY, 0644)

    file.write "global\n"
    file.write "  maxconn 32768\n"
    file.write "\n"

    file.write "defaults\n"
    file.write "  timeout client 90000\n"
    file.write "  timeout server 90000\n"
    file.write "  timeout connect 90000\n"
    file.write "  timeout queue 90000\n"
    file.write "  timeout http-request 90000\n"
    file.write "\n"

    write_config_mgnt(file, @bindaddr, @port) if @port != 0
    write_config_http(file, clusters, @bindaddr, @gateway_http_port) if @gateway_http_port > 0
    write_config_https(file, clusters, @bindaddr, @gateway_https_port) if @gateway_https_port > 0

    clusters.each do |app_id, cluster|
      # skip if the transport layer is anything else then TCP (e.g. UDP)
      next unless cluster.protocol == 'tcp'

      # kill the first / and replace any other occurence with a dot
      app_id = Helper.prettify_app_id(app_id, cluster.service_port)

      app_protocol = if !cluster.vhosts.empty? then
        # force app proto HTTP if virtual hosts have been provided.
        'http'
      else
        cluster.app_protocol
      end

      bind_opts = ''
      bind_opts = "#{bind_opts} defer-accept"

      if cluster.accept_proxy > 0 then
        bind_opts = "#{bind_opts} accept-proxy"
      end

      if app_protocol == 'http' then
        file.write "frontend __frontend_#{app_id}\n"
        file.write "  bind #{@bindaddr}:#{cluster.service_port}#{bind_opts}\n"
        file.write "  option dontlognull\n"
        file.write "  default_backend #{app_id}\n"
        file.write "\n"

        file.write "backend #{app_id}\n"
        file.write "  balance leastconn\n"
      else
        file.write "listen #{app_id}\n"
        file.write "  bind #{@bindaddr}:#{cluster.service_port}#{bind_opts}\n"
        file.write "  option dontlognull\n"
        file.write "  balance leastconn\n"
      end

      # health check interval defaults to 10s
      healthcheck_interval =
          if cluster.health_check && cluster.health_check.interval
            cluster.health_check.interval * 1000
          else
            10 * 1000
          end

      case app_protocol
      when 'http'
        uri = cluster.health_check.http_path || '/'

        # TODO: use label "#{LB_VHOST}-health-check" if available
        vhost = 'health-check'

        file.write "  mode http\n"
        file.write "  option forwardfor\n"
        file.write "  option abortonclose\n"
        file.write "  option redispatch\n"
        file.write "  option httpchk GET #{uri} HTTP/1.1\\r\\nHost:\\ #{vhost}\n"
        file.write "  retries 3\n"
        # TODO support custom haproxy option fields
      when 'smtp'
        file.write "  mode tcp\n"
        file.write "  option smtpchk EHLO #{ENV['HOST'] || ENV['HOSTNAME'] || 'localhost'}\n"
      when 'redis-disabled'
        file.write "  mode tcp\n"
        file.write "  option tcp-check\n"
        file.write "  tcp-check connect\n"
        file.write "  tcp-check send PING\\r\\n\n"
        file.write "  tcp-check expect string +PONG\n"
        file.write "  tcp-check send QUIT\\r\\n\n"
        file.write "  tcp-check expect string +OK\n"
      when 'redis-slave'
        file.write "  mode tcp\n"
        file.write "  option tcp-check\n"
        file.write "  tcp-check connect\n"
        file.write "  tcp-check send PING\\r\\n\n"
        file.write "  tcp-check expect string +PONG\n"
        file.write "  tcp-check send info\\ replication\\r\\n\n"
        file.write "  tcp-check expect string role:slave\n"
        file.write "  tcp-check send QUIT\\r\\n\n"
        file.write "  tcp-check expect string +OK\n"
      when 'redis-master', 'redis', 'redis-server'
        file.write "  mode tcp\n"
        file.write "  option tcp-check\n"
        file.write "  tcp-check connect\n"
        file.write "  tcp-check send PING\\r\\n\n"
        file.write "  tcp-check expect string +PONG\n"
        file.write "  tcp-check send info\\ replication\\r\\n\n"
        file.write "  tcp-check expect string role:master\n"
        file.write "  tcp-check send QUIT\\r\\n\n"
        file.write "  tcp-check expect string +OK\n"
      else
        # TODO support custom haproxy option fields
        file.write "  mode tcp\n"
      end

      server_opts = "check inter #{healthcheck_interval}"
      case cluster.proxy_protocol
      when 2
        server_opts = "#{server_opts} send-proxy-v2"
      when 1
        server_opts = "#{server_opts} send-proxy"
      when 0
        # do nothing
      else
        logger.warn "Invalid proxy_protocol given for #{app_id}: #{cluster.proxy_protocol} - ignoring."
      end

      cluster.instances.
          sort {|a, b| a.host.scan(/\d+/).map(&:to_i) <=>
                       b.host.scan(/\d+/).map(&:to_i)}.
          each do |i|
        file.write "  server #{Helper.prettify_dns_name(i.host)}:#{i.port}" +
                   " #{Helper.resolve_ipv4(i.host)}:#{i.port}" +
                   " #{server_opts}" +
                   "\n"
      end
      file.write "\n"
    end
    file.write @cfgtail
    file.close
  end
end
# }}}
class UpstreamConfDirHandler # {{{
  attr_accessor :confd_path, :logger

  def initialize(confd_path, logger)
    @confd_path = confd_path
    @logger = logger

    FileUtils.mkdir_p(@confd_path)
  end

  def apply(clusters, force)
    old_files = collect_files
    new_files = []

    clusters.each do |app_id, cluster|
      # kill the first / and replace any other occurence with a dot
      app_id = Helper.prettify_app_id(app_id, cluster.service_port)

      cfgfile = "#{confd_path}/#{app_id}.instances"
      tmpfile = "#{cfgfile}.tmp"

      write_file(tmpfile, app_id, cluster)
      new_files << cfgfile

      # only write this file if it changes content (to preserve mtime)
      if !File.exist?(cfgfile) then
        logger.debug "confd: new #{cfgfile}"
        FileUtils.mv(tmpfile, cfgfile)
      elsif !FileUtils.identical?(tmpfile, cfgfile) then
        logger.debug "confd: refresh #{cfgfile}"
        FileUtils.mv(tmpfile, cfgfile)
      else
        FileUtils.rm(tmpfile)
      end
    end

    superfluous_files = old_files - new_files
    if !superfluous_files.empty? then
      logger.debug "Removing superfluous files: #{superfluous_files.join(', ')}"
      FileUtils.rm_f(superfluous_files)
    end
  rescue => bang
    logger.error "[confd] Failed to apply configuration. #{bang}"
    logger.error bang.backtrace
  end

  def write_file(filename, app_id, cluster)
    flags = File::CREAT | File::TRUNC | File::WRONLY
    mode = 0644

    File.open(filename, flags, mode) do |file|
      file.write "Service-Name: #{app_id}\r\n"
      file.write "Service-Port: #{cluster.service_port}\r\n"
      file.write "Service-Transport-Proto: #{cluster.protocol}\r\n"
      file.write "Service-Application-Proto: #{cluster.app_protocol}\r\n"
      if cluster.health_check != nil then
        file.write "Health-Check-Proto: #{cluster.health_check.protocol}\r\n"
      end
      file.write "\r\n"
      cluster.instances.each do |instance|
        file.write "#{instance.host}:#{instance.port}\r\n"
      end
    end
  end

  def collect_files
    Dir.entries(@confd_path).reject {|f| f == '.' || f == '..' }.
                             map{|f| "#{@confd_path}/#{f}"}
  end
end # }}}
class MarathonServiceDiscovery # {{{
  def initialize(marathon_host, marathon_port, group_filter, handlers, logger)
    @marathon_host = marathon_host
    @marathon_port = marathon_port
    @group_filter = group_filter
    @logger = logger
    @handlers = handlers

    logger.debug "mmsd groups: #{group_filter.inspect}"

    events_url = "http://#{marathon_host}:#{marathon_port}/v2/events"
    logger.info "Listening on URL #{events_url}"
    @source = EventMachine::EventSource.new(events_url)
  end

  def logger
    @logger
  end

  def run
    reset_from_tasks(true)

    EM.run do
      @source.error {|message| sse_error(message)}
      @source.on 'status_update_event' do |json| status_update_event(json) end
      @source.on 'health_status_changed_event' do |json| health_status_changed_event(json) end
      @source.on 'deployment_success' do |json| deployment_success(json) end

      @source.start
    end
  end

  def sse_error(message)
    logger.error "SSE stream error. #{message}"
  end

  def status_update_event(json)
    logger.debug "on[status_update_event]:"
    obj = JSON.parse(json)

    slave_id = obj['slaveId']
    task_status = obj['taskStatus']
    message = obj['message']
    app_id = obj['appId']
    host = obj['host']
    port = obj['ports'][0]
    timestamp = obj['timestamp']

    if task_status == 'TASK_RUNNING' then
      logger.debug "[#{app_id}] new instance on #{host}:#{port}"
    else
      logger.debug "[#{app_id}] instance on #{host}:#{port} is #{task_status}"
    end

    reset_from_tasks
  end

  def health_status_changed_event(json)
    logger.debug "on[health_status_changed_event]:"
    reset_from_tasks
  end

  def deployment_success(json)
    logger.debug "on[deployment_success]:"
    reset_from_tasks
  end

private

  def reset_from_tasks(force = false)
    clusters = collect_clusters

    @handlers.each {|handler| handler.apply(clusters, force)}
  end

  # retrieves the number of services in a Marathon App
  def get_service_count(app)
    app['ports'].count
  end

  # Retrieves the service port number of a Marathon App at given port index.
  def get_service_port(app, index)
    app['ports'][index]
  end

  def get_service_protocol(app, index)
    protocols = (app['container']['docker']['portMappings'] || {}).map do |mapping|
      mapping['protocol']
    end

    protocols[index] || 'tcp'
  end

  def get_health_check(app, index)
    checks = app['healthChecks']
    return nil unless checks != nil

    check = checks[index]
    return nil unless check != nil

    case check['protocol']
    when 'HTTP'
      HealthCheck.new('http', check['intervalSeconds'].to_i, check['path'])
    when 'TCP'
      HealthCheck.new('tcp', check['intervalSeconds'].to_i, nil)
    else
      HealthCheck.new('tcp', nil, nil)
    end
  end

  # TODO: return array instead of map (not needed)
  def collect_clusters
    clusters = {}

    uri = "http://#{@marathon_host}:#{@marathon_port}/v2/apps?embed=apps.tasks"
    response = Net::HTTP.get(URI(uri))
    obj = JSON.parse(response)

    apps = obj['apps']
    apps.each do |app|
      app_id = app['id'].gsub('/', '.')

      service_ports = ((app['labels'] || {})[LB_SERVICE_PORT] || '').split(',')
      groups = ((app['labels'] || {})[FILTER_GROUP_NAME] || '').split(',')
      accept_proxy = ((app['labels'] || {})[LB_ACCEPT_PROXY] || '').to_i
      proxy_protocol = ((app['labels'] || {})[LB_PROXY_PROTOCOL] || '').to_i
      vhost_default = (app['labels'] || {})[LB_VHOST_DEFAULT]
      vhost_ssl_default = (app['labels'] || {})[LB_VHOST_SSL_DEFAULT]

      vhost_defs = ((app['labels'] || {})[LB_VHOST] || '').split(',')
      indexed_vhosts = vhost_defs.inject([]) do |result, vhost_def|
        fields = vhost_def.split(':')
        fields[1] = fields.size == 1 ? 0 : fields[1].to_i
        result << fields
      end

      vhost_ssl_defs = ((app['labels'] || {})[LB_VHOST_SSL] || '').split(',')
      indexed_vhosts_ssl = vhost_ssl_defs.inject([]) do |result, vhost_def|
        fields = vhost_def.split(':')
        fields[1] = fields.size == 1 ? 0 : fields[1].to_i
        result << fields
      end

      if should_include?(app_id, groups) then
        get_service_count(app).times do |port_index|
          service_port = get_service_port(app, port_index)

          # override service_port in case lb-service-port label was provided
          if !service_ports.empty? && service_ports[port_index].to_i != 0 then
            new_port = service_ports[port_index].to_i
            logger.debug "[#{app_id}] Override service port via label definition from #{service_port} to #{new_port}."
            service_port = new_port
          end

          next if service_port == 0

          app_service_name = "#{app_id}-#{port_index}"

          selected_vhosts =
              indexed_vhosts.select {|vhost| vhost[1] == port_index}.
              map {|indexed_vhost| indexed_vhost[0]}

          selected_vhosts_ssl =
              indexed_vhosts_ssl.select {|vhost| vhost[1] == port_index}.
              map {|indexed_vhost| indexed_vhost[0]}

          instances = []
          app['tasks'].each do |task|
            instances << AppInstance.new(task['id'],
                                         task['host'],
                                         task['ports'][port_index])
          end

          clusters[app_service_name] = AppCluster.new(
              app_service_name,
              service_port,
              get_service_protocol(app, port_index),
              get_health_check(app, port_index),
              app['labels'],
              selected_vhosts_ssl,
              vhost_ssl_default,
              selected_vhosts,
              vhost_default != nil && vhost_default.to_i == port_index,
              accept_proxy,
              proxy_protocol,
              instances)
        end
      end
    end

    clusters
  end

  # filter applications by (globbed) group names
  # [production, staging1, staging2] & [staging*] == [staging1, staging2]
  def should_include?(app_id, groups)
    return true if @group_filter.include?('*')

    !groups.select do |group|
      @group_filter.any?{|g| File.fnmatch(g, group)}
    end.empty?
  end

  def dump_clusters(clusters)
    clusters.each do |app_id, cluster|
      puts "* #{cluster.app_id} #{cluster.service_port}"
      cluster.instances.each do |i|
        puts "  + #{i.host}:#{i.port}"
      end
    end
  end
end
# }}}
class ServiceIP # {{{
  attr_accessor :logger, :virtual_ip, :host_ip

  def initialize(logger, virtual_ip, host_ip)
    @logger = logger
    @virtual_ip = virtual_ip

    @host_ip = if @host_ip.nil? || @host_ip.empty? then
      `ip route | grep -w src | grep -v -w linkdown | awk '{print $NF}' | tail -n1`.strip
    else
      host_ip
    end
  end

  def up
    logger.debug "ServiceIP UP: Initialize virtual-ip #{virtual_ip}, host-ip #{host_ip}"

    return unless @virtual_ip != '' && @host_ip != ''
    comment = 'mmsd-service-ip'

    ['PREROUTING', 'OUTPUT'].each do |chain|
      # ensure any left-off traces are gone first
      `for n in $(iptables -t nat -L #{chain}-n | awk '$5 == "#{virtual_ip}" {print NR - 2}'); do iptables -t nat -D #{chain} $n; done`

      `iptables -t nat -L #{chain} -n | grep -q #{virtual_ip}`
      if $?.exitstatus != 0 then
        logger.info "ServiceIP: Adding iptables rule to chain #{chain}"
        `iptables -t nat -A #{chain} -d #{virtual_ip} -j DNAT --to #{host_ip} -m comment --comment '#{comment}'`
      else
        logger.info "ServiceIP: iptables rule for chain #{chain} already present."
      end
    end
  end

  def down
    logger.debug "ServiceIP DOWN: keep virtual-ip #{virtual_ip}, host-ip #{host_ip}"
   # do we want that?
  end
end # }}}
class Mmsd # {{{
  attr_accessor :logger

  def self.run(argv)
    mmsd = Mmsd.new(argv)
    mmsd.main
  end

  OPT_DEFAULTS = {'marathon-host' => '',
                  'marathon-port' => '',
                  'filter-groups' => '*',
                  'haproxy-bin' => '/usr/sbin/haproxy',
                  'haproxy-pidfile' => '/var/run/haproxy.pid',
                  'haproxy-cfg' => '/var/run/haproxy.cfg',
                  'haproxy-cfgtail' => '',
                  'haproxy-bind' => '0.0.0.0',
                  'haproxy-port' => '8081',
                  'enable-gateway' => '',
                  'gateway-http-port' => '80',
                  'gateway-https-port' => '443',
                  'upstream-confd' => '/var/run/mmsd/confd',
                  'disable-upstream' => '',
                  'managed-ip' => '',
                  'log-level' => 'info'}

  def initialize(argv)
    @logger = Logger.new(STDOUT)

    @args = read_env(OPT_DEFAULTS)
    parse_args(argv, @args)
  end

  def read_env(defaults)
    opts = {}

    defaults.each do |name, default_value|
      env_name = name.gsub('-', '_').upcase
      env_value = ENV[env_name]

      opts[name] = env_value || default_value
    end

    opts
  end

  def parse_args(argv, opts)
    argv.each do |arg|
      fields = arg.split('=')
      if fields.count == 2 then
        name, value = arg.split('=')
        name.gsub!(/^--/, '')
        opts[name] = value
      else
        name = arg.gsub(/^--/, '')
        logger.debug "parse_args: #{name} = true"
        opts[name] = 'true'
      end
    end
  end

  def to_loglevel(value)
    case value
    when 'debug'
      Logger::DEBUG
    when 'info'
      Logger::INFO
    when 'warn'
      Logger::WARN
    when 'error'
      Logger::ERROR
    else
      Logger::ERROR
    end
  end

  def quick_shutdown(signo)
    # FIXME: why is logging from trap context prohibited but puts allowed?
    puts "Quick Shutdown (#{signo})."
    exit 0
  end

  def main
    $stdout.sync = true

    logger.level = to_loglevel(getopt('log-level'))

    Signal.trap(:SIGINT) {|num| quick_shutdown(num)}
    Signal.trap(:SIGQUIT) {|num| quick_shutdown(num)}
    Signal.trap(:SIGTERM) {|num| quick_shutdown(num)}

    gateway_http_port = if getopt('enable-gateway') == 'true' then
      getopt('gateway-http-port').to_i
    else
      0
    end

    gateway_https_port = if getopt('enable-gateway') == 'true' then
      getopt('gateway-https-port').to_i
    else
      0
    end

    if getopt('managed-ip') != '' then
      managed_ip = getopt('managed-ip').split(':')
      @service_ip = ServiceIP.new(logger, managed_ip[0], managed_ip[1])
    else
      @service_ip = ServiceIP.new(logger, '', '')
    end

    config_tail = if getopt('haproxy-cfgtail') != '' then
      File.read(getopt('haproxy-cfgtail'))
    else
      ''
    end

    handlers = []
    if getopt('disable-upstream') == '' then
      handlers << UpstreamConfDirHandler.new(getopt('upstream-confd'), logger)
    end

    handlers << HaproxyBuilder.new(getopt('haproxy-bin'),
                                   getopt('haproxy-cfg'),
                                   config_tail,
                                   getopt('haproxy-pidfile'),
                                   getopt('haproxy-bind'),
                                   getopt('haproxy-port').to_i,
                                   gateway_http_port,
                                   gateway_https_port,
                                   logger)

    sd = MarathonServiceDiscovery.new(getopt('marathon-host'),
                                      getopt('marathon-port').to_i,
                                      getopt('filter-groups').split(','),
                                      handlers,
                                      logger)
    @service_ip.up

    sd.run

    @service_ip.down
  end

  def getopt(name)
    @args[name] || ''
  end
end
# }}}

Mmsd.run(ARGV)
