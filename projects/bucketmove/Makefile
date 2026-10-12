VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ganeshphutane-g/bucketmove/internal/cli.Version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build test integration servers servers-down dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bucketmove .

test:
	go vet ./...
	go test -race ./...

servers:
	scripts/test-servers.sh up test-servers.env

servers-down:
	scripts/test-servers.sh down

# Needs `make servers` first.
integration:
	set -a; . ./test-servers.env; set +a; BUCKETMOVE_INTEGRATION=1 go test -race -count=1 ./internal/integration/

dist:
	rm -rf dist && mkdir -p dist
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/bucketmove-$$os-$$arch$$ext . || exit 1; \
	done
	cd dist && sha256sum bucketmove-* > SHA256SUMS

clean:
	rm -rf bucketmove dist
