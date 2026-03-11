version:
	@bash ./cicd/version.sh -g . -c

version-full:
	@bash ./cicd/version.sh -g . -c -m

install:
	go mod download
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

upgrade:
	go get -u ./...
	go mod tidy

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$$($(MAKE) -s version-full)" -o seek ./cmd/seek

test:
	$(MAKE) test-static
	$(MAKE) test-unit

test-static:
	go vet ./...
	golangci-lint run ./...

test-unit:
	go test ./... -v -race

lint:
	golangci-lint run --fix ./...

release:
	VERSION=$$($(MAKE) -s version-full) goreleaser release --clean

.PHONY: install upgrade build test test-static test-unit lint release
