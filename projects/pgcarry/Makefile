VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/pgcarry/internal/cli.Version=$(VERSION)

.PHONY: build test test-docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o pgcarry .

test:
	go vet ./...
	go test -race ./...

# Starts real Postgres containers; needs Docker with the Compose plugin.
test-docker:
	PGCARRY_INTEGRATION=1 go test -race -timeout 30m ./...
