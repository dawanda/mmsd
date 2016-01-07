#!/usr/bin/env ruby
# coding: utf-8

require 'rubygems'
require 'em-eventsource'
require 'net/http'
require 'logger'
require 'json'
require 'pp'

# {{{ app instance health check
class HealthCheck
  def initialize
    @mode = :http # :http, :tcp, :redis_master, :redis_slave
    @http_uri = "/"
    @http_ignore_1xx = true
    @tcp_send = ""
    @tcp_expect = ""
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
  attr_accessor :instances, :app_id, :service_port, :mode

  def initialize(app_id, service_port, mode)
    @instances = []
    @app_id = app_id
    @service_port = service_port
    @mode = mode
  end

  def add_backend(name, host, port)
    @instances << AppInstance.new(name, host, port)
  end
end
# }}}
class HaproxyBuilder # {{{
  def initialize(cfgfile)
    @cfgfile = cfgfile
  end

  def apply(clusters)
    tmpfile = "#{@cfgfile}.tmp"
    write_config(tmpfile, clusters)
    # if files_differ?(@cfgfile, tmpfile) then
    #   # 3. if changed or if no process running, then reload/start haproxy
    # end
  end

  def write_config(filename, clusters)
    puts "HaproxyBuilder.write_config(\"#{filename}\")"
    file = File.open(filename, File::CREAT | File::TRUNC | File::RDWR, 0644)

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

    file.write "listen haproxy *:8081\n"
    file.write "  mode http\n"
    file.write "  stats enable\n"
    file.write "  stats uri /\n"
    file.write "  stats admin if TRUE\n"
    file.write "  monitor-uri /haproxy?monitor\n"
    file.write "\n"

    clusters.each do |app_id, cluster|
      file.write "listen #{cluster.app_id} *:#{cluster.service_port}\n"
      file.write "  balance leastconn\n"

      if cluster.mode == 'http' then
        file.write "  mode http\n"
        file.write "  option forwardfor\n"
        file.write "  option abortonclose\n"
      else
        file.write "  mode tcp\n"
      end

      cluster.instances.each do |i|
        file.write "  server #{i.host}:#{i.port} #{i.host}:#{i.port} check inter 10000\n"
      end
      file.write "\n"
    end
  end
end
# }}}
class MarathonServiceDiscovery # {{{
  def initialize(marathon_host, marathon_port, groups, logger)
    @marathon_host = marathon_host
    @marathon_port = marathon_port
    @groups = groups || ['*']
    @logger = logger
    @haproxy_cfg = "./haproxy.cfg"

    events_url = "http://#{marathon_host}:#{marathon_port}/v2/events"
    # logger.info "Listening on URL #{events_url}"
    @source = EventMachine::EventSource.new(events_url)
  end

  def logger
    @logger
  end

  def run
    reset_from_tasks

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
    pp obj

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
    obj = JSON.parse(json)
    pp obj
    reset_from_tasks
  end

  def deployment_success(json)
    logger.debug "on[deployment_success]:"
    obj = JSON.parse(json)
  end

private

  def reset_from_tasks
    uri = "http://#{@marathon_host}:#{@marathon_port}/v2/tasks?embed=tasks.apps"
    response = Net::HTTP.get(URI(uri))
    obj = JSON.parse(response)

    clusters = {}

    tasks = obj['tasks']
    tasks.each do |task|
      name = task['id']
      host = task['host']
      port = task['ports'][0]
      app_id = task['appId'].gsub('/', '.')
      service_port = task['servicePorts'][0] # XXX only the first one as primary

      if should_include?(app_id) then
        cluster = clusters[app_id]
        if cluster  == nil then
          mode = 'tcp' # TODO: one of (tcp, http, ...)
          cluster = clusters[app_id] = AppCluster.new(app_id, service_port, mode)
          # TODO: add health check definitions
        end
        cluster.add_backend(name, host, port)
      end
    end

    lb = HaproxyBuilder.new(@haproxy_cfg)
    lb.apply(clusters)
  end

  def should_include?(app_id)
    true # TODO: filter by @groups
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

logger = Logger.new(STDOUT)
logger.level = Logger::DEBUG
$stdout.sync = true

marathon_host = ENV.fetch('MARATHON_HOST')
marathon_port = ENV.fetch('MARATHON_PORT')
groups = ENV.fetch('MARATHON_GROUPS')

sd = MarathonServiceDiscovery.new(marathon_host, marathon_port, groups, logger)
sd.run
