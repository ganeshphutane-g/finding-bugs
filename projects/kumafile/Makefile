# The version is this project's latest tag (kumafile/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'kumafile/v*' --always --dirty 2>/dev/null | sed 's|^kumafile/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/kumafile/internal/cli.Version=$(VERSION)

.PHONY: build test test-server

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o kumafile .

test:
	go vet ./...
	go test -race ./...

# Starts real Uptime Kuma servers in Docker (KUMAFILE_IMAGE picks the version).
test-server:
	KUMAFILE_INTEGRATION=1 go test -race -timeout 30m ./...
