//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func installPrivateAsset(
	directory string,
	fileName string,
	temporaryPattern string,
	valid func(string) bool,
	write func(io.Writer) error,
) (string, bool, error) {
	path := filepath.Join(directory, fileName)
	if valid(path) {
		return path, false, nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", false, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", false, err
	}
	temporary, err := os.CreateTemp(directory, temporaryPattern)
	if err != nil {
		return "", false, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return "", false, err
	}
	if err := write(temporary); err != nil {
		_ = temporary.Close()
		return "", false, err
	}
	if err := temporary.Close(); err != nil {
		return "", false, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if valid(path) {
			return path, false, nil
		}
		return "", false, fmt.Errorf("install private asset: %w", err)
	}
	return path, true, nil
}
