# The version is this project's latest tag (composehost/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'composehost/v*' --always --dirty 2>/dev/null | sed 's|^composehost/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/composehost/internal/cli.Version=$(VERSION)

.PHONY: build install test test-compose

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o composehost .

# Compose looks for providers on PATH. Run "make build" first, as yourself;
# then "sudo make install" only copies.
install:
	@test -x composehost || { echo 'run "make build" first'; exit 1; }
	install -m 0755 composehost /usr/local/bin/composehost

test:
	go vet ./...
	go test -race ./...

# Also end to end with dockerd and Docker Compose 2.36.1 or later (root: the
# tests mount a tmpfs and chown directories).
test-compose: build
	go test -c -o e2e.test ./internal/e2e
	sudo env PATH="$(PATH)" COMPOSEHOST_INTEGRATION=1 COMPOSEHOST_BIN=$(CURDIR)/composehost $(CURDIR)/e2e.test -test.v
