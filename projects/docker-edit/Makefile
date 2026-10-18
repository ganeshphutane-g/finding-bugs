# The version is this project's latest tag (docker-edit/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'docker-edit/v*' --always --dirty 2>/dev/null | sed 's|^docker-edit/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/docker-edit/internal/cli.Version=$(VERSION)

.PHONY: build install test test-docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o docker-edit .

# Installs it as a Docker CLI plugin: `docker edit`.
install: build
	mkdir -p $(HOME)/.docker/cli-plugins
	cp docker-edit $(HOME)/.docker/cli-plugins/docker-edit

test:
	go vet ./...
	go test -race ./...

# Creates and recreates containers on the local Docker daemon (test-only
# names starting with "de-").
test-docker:
	DOCKEREDIT_INTEGRATION=1 go test -race -count=1 ./...
