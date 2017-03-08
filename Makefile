.PHONY: default
default: build

REPO_LOCAL=localhost/haproxy-ingress
REPO_PUBLIC=docker.jamf.build/haproxy-ingress
DOCKER_HUB=docker.jamf.build
include container.mk

GOOS=linux
GOARCH=amd64
GIT_REPO=$(shell git config --get remote.origin.url)
ROOT_PKG=haproxy-ingress/pkg


.PHONY: build
build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
	  -installsuffix cgo \
	  -ldflags "-s -w" \
	  -o rootfs/haproxy-ingress-controller \
	  $(ROOT_PKG)/controller
