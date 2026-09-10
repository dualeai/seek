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

# Maintainer-only: download fixed re-ranker sources, print their byte counts and
# hashes, and update tracked compressed resources. No build or release target
# invokes this.
rerank-assets-upgrade:
	bash ./cicd/rerank-assets-upgrade.sh

VERSION ?= $(shell bash ./cicd/version.sh -g . -c -m)
BUILD_VERSION = $(patsubst v%,%,$(VERSION))
OUTPUT ?= seek
DIST_DIR ?= dist
TARGET ?= $(shell go env GOOS)_$(shell go env GOARCH)
USE_PREBUILT ?= 0

build:
	go build -trimpath \
		-ldflags="-s -w -X main.version=$(BUILD_VERSION)" \
		-o "$(OUTPUT)" ./cmd/seek

package:
	$(MAKE) build OUTPUT=seek
	mkdir -p "$(DIST_DIR)"
	tar -czf "$(DIST_DIR)/seek_$(TARGET).tar.gz" seek

test:
	$(MAKE) test-static
	$(MAKE) test-unit

test-static:
	go vet ./...
	golangci-lint run ./...

# Plugin tests use the normal build target and exercise only seek's public CLI.
# CI sets USE_PREBUILT=1 after it downloads the build job's executable.
ifneq ($(USE_PREBUILT),1)
test-plugin: build
endif
test-plugin:
	@if [ "$(USE_PREBUILT)" = "1" ] && [ ! -x "$(OUTPUT)" ]; then echo "Prebuilt seek is missing or not executable: $(OUTPUT)"; exit 1; fi
	PATH="$(dir $(abspath $(OUTPUT))):$$PATH" sh ./plugins/seek-router/test/run.sh

JUNIT_XML ?= junit.xml
COVERPROFILE ?= cover.out
BENCH_COUNT ?= 10
BENCH_REPO_COUNT ?= 3

test-unit: test-plugin
	gotestsum --junitfile $(JUNIT_XML) -- ./... -v -race -timeout 18m -covermode=atomic -coverprofile=$(COVERPROFILE)

# Local benchmark output is diagnostic. CodSpeed is the source for comparisons.
test-bench: build
	SEEK_BENCH_BINARY="$(abspath $(OUTPUT))" SEEK_BENCH_REPO= go test ./cmd/seek/ -run='^$$' -bench=. -benchmem -count=$(BENCH_COUNT)

test-bench-repo:
	@if [ -z "$(SEEK_BENCH_REPO)" ]; then echo "Usage: make test-bench-repo SEEK_BENCH_REPO=/path/to/repo"; exit 1; fi
	SEEK_BENCH_REPO="$(SEEK_BENCH_REPO)" go test ./cmd/seek/ -run='^$$' -bench=BenchmarkLargeRepo -benchmem -count=$(BENCH_REPO_COUNT) -timeout=600s

# Make a local diagnostic comparison with golang.org/x/perf/cmd/benchstat.
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
	bash ./cicd/release.sh

.PHONY: install upgrade rerank-assets-upgrade build package test test-static test-plugin test-unit test-bench test-bench-repo test-bench-compare lint release
