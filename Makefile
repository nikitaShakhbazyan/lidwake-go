VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/nikitaShakhbazyan/lidwake-go/internal/paths.Version=$(VERSION)

.PHONY: build test vet install uninstall clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/lidwake ./cmd/lidwake

test:
	go test -race ./...

vet:
	go vet ./...

# Builds, then installs the helper, the daemon and the CLI (asks for your password once).
install: build
	./bin/lidwake setup

uninstall:
	lidwake uninstall

clean:
	rm -rf bin dist
