//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// installPrivateAsset installs fileName in directory, unless the file there
// already has wantBytes bytes and the SHA-256 wantSHA256 (validFileDigest).
// The install writes a temporary file and renames it, and gives the directory
// and the file mode 0700. It returns the path.
func installPrivateAsset(
	directory string,
	fileName string,
	temporaryPattern string,
	wantBytes int64,
	wantSHA256 string,
	write func(io.Writer) error,
) (string, error) {
	path := filepath.Join(directory, fileName)
	if validFileDigest(path, wantBytes, wantSHA256) {
		return path, nil
	}
	// A collection can remove an empty parent (reranker/<version>) between
	// the steps of MkdirAll, and a second call makes it again. macOS reports
	// that race as EINVAL, so any error gets the second call.
	if err := os.MkdirAll(directory, 0o700); err != nil {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(directory, temporaryPattern)
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := write(temporary); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if validFileDigest(path, wantBytes, wantSHA256) {
			return path, nil
		}
		return "", fmt.Errorf("install private asset: %w", err)
	}
	return path, nil
}

// installSemanticRuntime installs the bundled library of a vector engine in
// semantic/<engine>/<asset key> of the cache, unless a file with its digest is
// there. It returns the path of the file. engine names the library in
// errors (USearch, NumKong), and in lowercase it names the folder. It marks
// the folder as used for GC (touchRuntimeUse).
func installSemanticRuntime(engine string, compressed []byte, fileName string) (string, error) {
	runtimeBytes, err := decodeLateOnAsset(compressed)
	if err != nil {
		return "", fmt.Errorf("load %s runtime: %w", engine, err)
	}
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return "", err
	}
	folder := strings.ToLower(engine)
	dir := filepath.Join(cacheRoot, "semantic", folder, semanticRuntimeAssetKey(compressed))
	digest := sha256.Sum256(runtimeBytes)
	wantSHA256 := hex.EncodeToString(digest[:])
	defer touchRuntimeUse(dir)
	return installPrivateAsset(
		dir,
		fileName,
		"."+folder+"-*.tmp",
		int64(len(runtimeBytes)),
		wantSHA256,
		func(writer io.Writer) error {
			_, err := writer.Write(runtimeBytes)
			return err
		},
	)
}

// semanticRuntimeAssetKey names the cache folder of a bundled library by a
// hash of its compressed bytes, so a new library gets a new folder.
func semanticRuntimeAssetKey(compressed []byte) string {
	digest := sha256.Sum256(compressed)
	return hex.EncodeToString(digest[:16])
}
