VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/issuemove/internal/cli.Version=$(VERSION)

.PHONY: build test test-server

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o issuemove .

test:
	go vet ./...
	go test -race ./...

# Starts real servers in Docker (ISSUEMOVE_IMAGE picks Gitea or Forgejo).
test-server:
	ISSUEMOVE_INTEGRATION=1 go test -race -timeout 30m ./...
