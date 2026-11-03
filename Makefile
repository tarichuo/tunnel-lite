GO ?= go

.PHONY: build test check clean
build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -o bin/tunnel-server ./cmd/tunnel-server
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -o bin/tunnel-agent ./cmd/tunnel-agent

test:
	$(GO) test -race ./...

check:
	$(GO) vet ./...
	node --check internal/server/web/app.js

clean:
	rm -rf bin
