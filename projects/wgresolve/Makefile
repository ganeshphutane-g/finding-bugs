# The version is this project's latest tag (wgresolve/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'wgresolve/v*' --always --dirty 2>/dev/null | sed 's|^wgresolve/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/wgresolve/internal/cli.Version=$(VERSION)

.PHONY: build test test-wireguard

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o wgresolve .

test:
	go vet ./...
	go test -race ./...

# Also end to end with real WireGuard in network namespaces (root; needs ip,
# wg, nft, dnsmasq, and kernel WireGuard or wireguard-go).
test-wireguard: build
	go test -c -o e2e.test ./internal/e2e
	cd internal/e2e && sudo WGRESOLVE_INTEGRATION=1 WGRESOLVE_BIN=$(CURDIR)/wgresolve $(CURDIR)/e2e.test -test.v
