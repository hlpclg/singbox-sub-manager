BINARY := bin/proxyctl

.PHONY: build test fmt check package

build:
	mkdir -p bin
	go build -trimpath -o $(BINARY) ./cmd/proxyctl

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

check:
	bash -n install-proxy.sh
	bash -n merge-nodes.sh
	go test ./...
	go vet ./...

package: check build
	mkdir -p dist
	tar -czf dist/proxy-installer.tar.gz --exclude=.git --exclude=bin --exclude=dist .
