FROM ruby:2.2.4

RUN gem install em-eventsource

ADD marathon-service-discovery.rb /mmsd.rb

CMD ["/mmsd.rb"]
