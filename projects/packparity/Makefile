# The version is this project's latest tag (packparity/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'packparity/v*' --always --dirty 2>/dev/null | sed 's|^packparity/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/packparity/internal/cli.Version=$(VERSION)

.PHONY: build test test-restic

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o packparity .

test:
	go vet ./...
	go test -race ./...

# Also end to end with real restic and rest-server (on PATH, or RESTIC_BIN
# and REST_SERVER_BIN).
test-restic:
	PACKPARITY_INTEGRATION=1 go test -race -count=1 ./...
