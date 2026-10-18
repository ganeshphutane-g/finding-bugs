# The version is this project's latest tag (pushdir/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'pushdir/v*' --always --dirty 2>/dev/null | sed 's|^pushdir/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/pushdir/internal/cli.Version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 linux/arm

.PHONY: build test rclone dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o pushdir .

# Builds rclone for the tests if it is not installed.
rclone:
	@command -v rclone >/dev/null || GOBIN=$(CURDIR)/.bin go install github.com/rclone/rclone@v1.75.2

test:
	go vet ./...
	RCLONE_BIN=$${RCLONE_BIN:-$$(command -v rclone || echo $(CURDIR)/.bin/rclone)} go test -race ./...

dist:
	rm -rf dist && mkdir -p dist
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/pushdir-$$os-$$arch . || exit 1; \
	done
	cd dist && sha256sum pushdir-* > SHA256SUMS

clean:
	rm -rf pushdir dist .bin
