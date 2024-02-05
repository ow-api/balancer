FROM alpine AS assets

RUN mkdir /src \
    && cd /src \
    && git clone https://github.com/ow-api/website.git

FROM golang:alpine AS builder

RUN mkdir -p /build/src

ADD . /build/src

COPY --from=assets /src/website/* /build/src/data/

RUN cd /build/src \
	&& go build -o /build/balancer

FROM alpine:3

COPY --from=builder /build/balancer /usr/bin/balancer

CMD "/usr/bin/balancer"