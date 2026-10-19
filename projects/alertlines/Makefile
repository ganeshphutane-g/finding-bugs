# The version is this project's latest tag (alertlines/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'alertlines/v*' --always --dirty 2>/dev/null | sed 's|^alertlines/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/alertlines/internal/cli.Version=$(VERSION)

.PHONY: build test test-server

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o alertlines .

test:
	go vet ./...
	go test -race ./...

# Also against real Loki and Alertmanager servers in Docker.
test-server:
	ALERTLINES_INTEGRATION=1 go test -race -count=1 ./...
