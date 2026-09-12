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

# Maintainer-only: download fixed search sources, derive the model, and update
# tracked model, tokenizer, ONNX Runtime, and USearch resources. No build or
# release target invokes this.
search-assets-upgrade:
	bash ./cicd/rerank-assets-upgrade.sh

# Keep the old target as a compatibility alias for maintainer scripts.
rerank-assets-upgrade: search-assets-upgrade

VERSION ?= $(shell bash ./cicd/version.sh -g . -c -m)
BUILD_VERSION = $(patsubst v%,%,$(VERSION))
OUTPUT ?= seek
DIST_DIR ?= dist
TARGET ?= $(shell go env GOOS)_$(shell go env GOARCH)
USE_PREBUILT ?= 0
TOKENIZER_TARGET := $(shell go env GOOS)_$(shell go env GOARCH)
TOKENIZER_SUPPORTED_TARGETS := darwin_amd64 darwin_arm64 linux_amd64 linux_arm64
BUILD_CGO_ENABLED := $(shell go env CGO_ENABLED)

ifeq ($(BUILD_CGO_ENABLED),1)
ifneq ($(filter $(TOKENIZER_TARGET),$(TOKENIZER_SUPPORTED_TARGETS)),)
TOKENIZER_ARCHIVE := cmd/seek/rerank_tokenizer_assets_$(TOKENIZER_TARGET)/libtokenizers.tar.gz
TOKENIZER_NATIVE_DIR := cmd/seek/rerank_tokenizer_native_$(TOKENIZER_TARGET)
TOKENIZER_LIBRARY := $(TOKENIZER_NATIVE_DIR)/libtokenizers.a

$(TOKENIZER_LIBRARY): $(TOKENIZER_ARCHIVE)
	mkdir -p "$(TOKENIZER_NATIVE_DIR)"
	tar -xzf "$(TOKENIZER_ARCHIVE)" -C "$(TOKENIZER_NATIVE_DIR)" libtokenizers.a
	touch "$(TOKENIZER_LIBRARY)"

tokenizer-native: $(TOKENIZER_LIBRARY)
else
tokenizer-native:
	@echo "Seek supports builds only on: $(TOKENIZER_SUPPORTED_TARGETS)" >&2
	@exit 1
endif
else
tokenizer-native:
	@echo "Seek requires CGO_ENABLED=1; reduced lexical-only binaries are not supported" >&2
	@exit 1
endif

build: tokenizer-native
	go build -trimpath \
		-ldflags="-s -w -X main.version=$(BUILD_VERSION)" \
		-o "$(OUTPUT)" ./cmd/seek

package:
	$(MAKE) build OUTPUT=seek
	mkdir -p "$(DIST_DIR)"
	tar -czf "$(DIST_DIR)/seek_$(TARGET).tar.gz" seek LICENSE \
		THIRD_PARTY_NOTICES.md ONNXRUNTIME_THIRD_PARTY_NOTICES.txt

test:
	$(MAKE) test-static
	$(MAKE) test-unit

test-static: tokenizer-native
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
SEMANTIC_BENCH_SAMPLES ?= 10

test-unit: tokenizer-native test-plugin
	gotestsum --junitfile $(JUNIT_XML) -- ./... -v -race -timeout 18m -covermode=atomic -coverprofile=$(COVERPROFILE)

# Local benchmark output is diagnostic. CodSpeed is the source for comparisons.
test-bench: build
	SEEK_BENCH_BINARY="$(abspath $(OUTPUT))" SEEK_BENCH_REPO= go test ./cmd/seek/ -run='^$$' -bench=. -benchmem -count=$(BENCH_COUNT)

test-bench-repo: tokenizer-native
	@if [ -z "$(SEEK_BENCH_REPO)" ]; then echo "Usage: make test-bench-repo SEEK_BENCH_REPO=/path/to/repo"; exit 1; fi
	SEEK_BENCH_REPO="$(SEEK_BENCH_REPO)" go test ./cmd/seek/ -run='^$$' -bench=BenchmarkLargeRepo -benchmem -count=$(BENCH_REPO_COUNT) -timeout=600s

# Run the retained production-binary cold-build gates on one pinned checkout.
test-bench-semantic: build
	@if [ -z "$(SEEK_BENCH_REPO)" ]; then echo "Usage: make test-bench-semantic SEEK_BENCH_REPO=/path/to/pinned-kubernetes"; exit 1; fi
	SEEK_BIN="$(abspath $(OUTPUT))" SEEK_BENCH_REPO="$(SEEK_BENCH_REPO)" SEEK_BENCH_HEAD="$(SEEK_BENCH_HEAD)" \
		uv run --script ./cicd/bench-semantic.py --samples $(SEMANTIC_BENCH_SAMPLES)

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

lint: tokenizer-native
	golangci-lint run --fix ./...

release:
	bash ./cicd/release.sh

.PHONY: install upgrade search-assets-upgrade rerank-assets-upgrade tokenizer-native build package test test-static test-plugin test-unit test-bench test-bench-repo test-bench-semantic test-bench-compare lint release
