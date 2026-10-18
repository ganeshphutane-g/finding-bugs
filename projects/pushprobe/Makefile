# The version is this project's latest tag (pushprobe/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'pushprobe/v*' --always --dirty 2>/dev/null | sed 's|^pushprobe/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/pushprobe/internal/cli.Version=$(VERSION)

.PHONY: build test test-server

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o pushprobe .

test:
	go vet ./...
	go test -race ./...

# Also against real Uptime Kuma and Gatus servers in Docker.
test-server:
	PUSHPROBE_INTEGRATION=1 go test -race -count=1 ./...
