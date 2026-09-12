//go:build cgo && darwin && arm64

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLateOnCoreMLCacheDirectoryTracksModel(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", cacheRoot)
	options := lateOnCoreMLProviderOptions()
	first, err := lateOnCoreMLCacheDirectory([]byte("first model"), options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := lateOnCoreMLCacheDirectory([]byte("second model"), options)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different models shared a Core ML cache directory")
	}
	if !filepath.IsAbs(first) || filepath.Dir(filepath.Dir(filepath.Dir(first))) != filepath.Join(cacheRoot, "reranker") {
		t.Fatalf("unexpected Core ML cache path %q", first)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("Core ML cache mode=%o, want 700", info.Mode().Perm())
	}
}

func TestLateOnCoreMLCacheKeyTracksRuntimeAndProvider(t *testing.T) {
	model := []byte("model")
	options := lateOnCoreMLProviderOptions()
	base := lateOnCoreMLCacheKey(model, "runtime one", options)
	if base == lateOnCoreMLCacheKey(model, "runtime two", options) {
		t.Fatal("different ONNX Runtime versions shared a Core ML cache key")
	}
	changedOptions := lateOnCoreMLProviderOptions()
	changedOptions["MLComputeUnits"] = "CPUOnly"
	if base == lateOnCoreMLCacheKey(model, "runtime one", changedOptions) {
		t.Fatal("different Core ML provider settings shared a cache key")
	}
	reorderedOptions := map[string]string{}
	for _, key := range []string{
		"RequireStaticInputShapes",
		"ModelFormat",
		"EnableOnSubgraphs",
		"MLComputeUnits",
	} {
		reorderedOptions[key] = options[key]
	}
	if base != lateOnCoreMLCacheKey(model, "runtime one", reorderedOptions) {
		t.Fatal("Core ML cache key depends on map iteration order")
	}
}
