package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"testing"
)

const lateOnExtractHelperEnv = "SEEK_TEST_RERANK_EXTRACT"

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	if os.Getenv(cliProcessHelperEnv) == cliProcessTestMarker ||
		os.Getenv(lateOnExtractHelperEnv) == "1" {
		os.Exit(m.Run())
	}
	cacheDir, err := os.MkdirTemp("", "seek-test-cache-*")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create isolated test cache: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("SEEK_CACHE_DIR", cacheDir); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "set isolated test cache: %v\n", err)
		_ = os.RemoveAll(cacheDir)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(cacheDir); err != nil && code == 0 {
		_, _ = fmt.Fprintf(os.Stderr, "remove isolated test cache: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
