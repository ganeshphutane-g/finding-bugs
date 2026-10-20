# The version is this project's latest tag (xffdns/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'xffdns/v*' --always --dirty 2>/dev/null | sed 's|^xffdns/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/xffdns/internal/cli.Version=$(VERSION)

.PHONY: build test test-server

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o xffdns .

test:
	go vet ./...
	go test -race ./...

# Also against real AdGuard Home, HAProxy, Traefik and dnsdist in Docker.
test-server:
	XFFDNS_INTEGRATION=1 go test -race -count=1 ./...
