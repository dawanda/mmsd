FROM ruby:2.2.4
MAINTAINER Christian Parpart <christian@dawanda.com>

RUN gem install em-eventsource
RUN apt-get update && apt-get install -y haproxy

ADD mmsd /mmsd

ENTRYPOINT ["/mmsd"]
