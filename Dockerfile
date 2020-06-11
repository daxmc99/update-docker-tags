FROM golang:1.14-alpine@sha256:6042b9cfb4eb303f3bdcbfeaba79b45130d170939318de85ac5b9508cb6f0f7e AS builder
MAINTAINER <test>
WORKDIR /go/src/update-docker-tags

COPY go.* ./
RUN go mod download

COPY *.go ./
RUN go build -o /bin/update-docker-tags

FROM alpine:3.11@sha256:9a839e63dad54c3a6d1834e29692c8492d93f90c59c978c1ed79109ea4fb9a54

RUN apk add --no-cache ca-certificates

COPY --from=builder /bin/update-docker-tags /usr/local/bin

ENTRYPOINT ["update-docker-tags"]

