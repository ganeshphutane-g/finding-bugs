# The version is this project's latest tag (tsroam/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'tsroam/v*' --always --dirty 2>/dev/null | sed 's|^tsroam/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/tsroam/internal/cli.Version=$(VERSION)

.PHONY: build test test-tailscale

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o tsroam .

test:
	go vet ./...
	go test -race ./...

# Also end to end with headscale and real tailscaled in network namespaces
# (root; needs ip, nft, bash, ping, and tailscale, tailscaled and headscale on
# PATH or in TAILSCALE, TAILSCALED, HEADSCALE).
test-tailscale: build
	go test -c -o e2e.test ./internal/e2e
	cd internal/e2e && sudo env PATH="$(PATH)" TSROAM_INTEGRATION=1 TSROAM_BIN=$(CURDIR)/tsroam $(CURDIR)/e2e.test -test.v
