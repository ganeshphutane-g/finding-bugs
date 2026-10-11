VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/docker-mop/internal/cli.Version=$(VERSION)
PLUGIN_DIR ?= $(HOME)/.docker/cli-plugins

.PHONY: build test install clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o docker-mop .

test:
	go vet ./...
	go test -race ./...

install: build
	mkdir -p $(PLUGIN_DIR)
	install -m 0755 docker-mop $(PLUGIN_DIR)/docker-mop

clean:
	rm -rf docker-mop dist
