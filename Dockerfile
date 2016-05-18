FROM golang:1.6.2-alpine
MAINTAINER Christian Parpart <christian@dawanda.com>

RUN apk --update add git haproxy && go get github.com/tools/godep

ADD . /go/src/github.com/dawanda/mmsd
RUN cd /go/src/github.com/dawanda/mmsd && godep restore && go install

ENTRYPOINT ["/go/bin/mmsd"]
CMD ["--help"]
