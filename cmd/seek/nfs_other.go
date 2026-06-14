//go:build !linux && !darwin

package main

func isOnNFS(path string) bool { return false }
