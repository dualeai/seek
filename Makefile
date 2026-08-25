version:
	@bash ./cicd/version.sh -g . -c

version-full:
	@bash ./cicd/version.sh -g . -c -m

install:
	go mod download
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install gotest.tools/gotestsum@latest

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

# Plugin tests use the normal build target and exercise only seek's public CLI.
test-plugin: build
	PATH="$(CURDIR):$$PATH" sh ./plugins/seek-router/test/run.sh

JUNIT_XML ?= junit.xml
COVERPROFILE ?= cover.out
BENCH_COUNT ?= 10
BENCH_REPO_COUNT ?= 3

test-unit: test-plugin
	gotestsum --junitfile $(JUNIT_XML) -- ./... -v -race -timeout 18m -covermode=atomic -coverprofile=$(COVERPROFILE)

test-bench:
	SEEK_BENCH_REPO= go test ./cmd/seek/ -run='^$$' -bench=. -benchmem -count=$(BENCH_COUNT)

test-bench-repo:
	@if [ -z "$(SEEK_BENCH_REPO)" ]; then echo "Usage: make test-bench-repo SEEK_BENCH_REPO=/path/to/repo"; exit 1; fi
	SEEK_BENCH_REPO="$(SEEK_BENCH_REPO)" go test ./cmd/seek/ -run='^$$' -bench=BenchmarkLargeRepo -benchmem -count=$(BENCH_REPO_COUNT) -timeout=600s

# Compare two benchmark runs using golang.org/x/perf/cmd/benchstat.
# Workflow:
#   # Use the same normalized command on both revisions.
#   git switch baseline-revision
#   SEEK_BENCH_REPO= go test ./cmd/seek/ -run='^$' -bench=. -benchmem -count=10 > baseline.txt
#   git switch feature-revision
#   SEEK_BENCH_REPO= go test ./cmd/seek/ -run='^$' -bench=. -benchmem -count=10 > after.txt
#   BASE=baseline.txt NEW=after.txt make test-bench-compare
test-bench-compare:
	@if [ -z "$(BASE)" ] || [ -z "$(NEW)" ]; then echo "Usage: BASE=baseline.txt NEW=after.txt make test-bench-compare"; exit 1; fi
	go run golang.org/x/perf/cmd/benchstat@latest $(BASE) $(NEW)

lint:
	golangci-lint run --fix ./...

release:
	VERSION=$$($(MAKE) -s version-full) goreleaser release --clean

.PHONY: install upgrade build test test-static test-plugin test-unit test-bench test-bench-repo test-bench-compare lint release
