# The version is this project's latest tag (docker-mop/vX.Y.Z), else the commit.
VERSION ?= $(or $(shell git describe --tags --match 'docker-mop/v*' --always --dirty 2>/dev/null | sed 's|^docker-mop/||'),dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/docker-mop/internal/cli.Version=$(VERSION)
PLUGIN_DIR ?= $(HOME)/.docker/cli-plugins

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build test install dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o docker-mop .

test:
	go vet ./...
	go test -race ./...

install: build
	mkdir -p $(PLUGIN_DIR)
	install -m 0755 docker-mop $(PLUGIN_DIR)/docker-mop

dist:
	rm -rf dist && mkdir -p dist
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/docker-mop-$$os-$$arch . || exit 1; \
	done
	cd dist && sha256sum docker-mop-* > SHA256SUMS

clean:
	rm -rf docker-mop dist
