# The version is this project's latest tag (pullwhy/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'pullwhy/v*' --always --dirty 2>/dev/null | sed 's|^pullwhy/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/pullwhy/internal/cli.Version=$(VERSION)

.PHONY: build test test-docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o pullwhy .

test:
	go vet ./...
	go test -race ./...

# Also side by side with real docker pulls, in scratch daemons (docker:dind).
test-docker:
	PULLWHY_INTEGRATION=1 go test -race -count=1 ./...
