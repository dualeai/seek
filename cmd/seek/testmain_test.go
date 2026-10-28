package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"testing"
)

// lateOnExtractHelperEnv marks a child process of
// TestLateOnRuntimeCacheAcrossProcesses: with the value "1",
// TestLateOnRuntimeExtractHelper extracts the runtime.
const lateOnExtractHelperEnv = "SEEK_TEST_RERANK_EXTRACT"

// TestMain runs the tests with no log output and with a new SEEK_CACHE_DIR,
// which it removes at the end. A test cannot then read or change the cache of
// the user.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	// A helper process ends with os.Exit, so a cache directory of its own
	// would stay on disk. It uses the cache of its parent or none.
	if os.Getenv(cliProcessHelperEnv) == cliProcessTestMarker ||
		os.Getenv(lateOnExtractHelperEnv) == "1" || os.Getenv(shardScanEnv) != "" {
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
