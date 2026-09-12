//go:build cgo && darwin && arm64

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	ort "github.com/yalue/onnxruntime_go"
)

const lateOnProviderHasAccelerator = true

func configureLateOnProvider(options *ort.SessionOptions, _ int, model []byte) error {
	if err := configureLateOnCPUProvider(options, 1); err != nil {
		return err
	}
	providerOptions := lateOnCoreMLProviderOptions()
	cacheDirectory, err := lateOnCoreMLCacheDirectory(model, providerOptions)
	if err != nil {
		return err
	}
	providerOptions["ModelCacheDirectory"] = cacheDirectory
	if err := options.AppendExecutionProviderCoreMLV2(providerOptions); err != nil {
		return fmt.Errorf("enable Core ML: %w", err)
	}
	return nil
}

func lateOnCoreMLProviderOptions() map[string]string {
	return map[string]string{
		"ModelFormat":              "MLProgram",
		"MLComputeUnits":           "ALL",
		"RequireStaticInputShapes": "1",
		"EnableOnSubgraphs":        "0",
	}
}

func lateOnCoreMLCacheDirectory(model []byte, providerOptions map[string]string) (string, error) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(
		cacheRoot,
		"reranker",
		"coreml",
		"mlprogram-static",
		lateOnCoreMLCacheKey(model, ort.GetVersion(), providerOptions),
	)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create Core ML model cache: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", fmt.Errorf("set Core ML model cache permissions: %w", err)
	}
	return directory, nil
}

func lateOnCoreMLCacheKey(model []byte, runtimeVersion string, providerOptions map[string]string) string {
	hash := sha256.New()
	hash.Write([]byte("seek-coreml-cache-v1\x00"))
	writeSemanticHashField(hash, []byte(runtimeVersion))
	keys := make([]string, 0, len(providerOptions))
	for key := range providerOptions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		writeSemanticHashField(hash, []byte(key))
		writeSemanticHashField(hash, []byte(providerOptions[key]))
	}
	writeSemanticHashField(hash, model)
	return hex.EncodeToString(hash.Sum(nil)[:16])
}
