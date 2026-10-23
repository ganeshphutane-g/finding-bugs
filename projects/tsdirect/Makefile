# The version is this project's latest tag (tsdirect/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'tsdirect/v*' --always --dirty 2>/dev/null | sed 's|^tsdirect/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/tsdirect/internal/cli.Version=$(VERSION)

.PHONY: build test test-tailscale

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o tsdirect .

test:
	go vet ./...
	go test -race ./...

# Also end to end with headscale and real tailscaled in network namespaces
# (root; needs ip, nft, bash, and tailscale, tailscaled and headscale on
# PATH or in TAILSCALE, TAILSCALED, HEADSCALE).
test-tailscale: build
	go test -c -o e2e.test ./internal/e2e
	cd internal/e2e && sudo env PATH="$(PATH)" TSDIRECT_INTEGRATION=1 TSDIRECT_BIN=$(CURDIR)/tsdirect $(CURDIR)/e2e.test -test.v
