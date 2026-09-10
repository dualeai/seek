//go:build !cgo || (!darwin && !linux) || (!amd64 && !arm64)

package main

import "testing"

func TestCLIProcessRerankUnavailableKeepsBM25(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")

	baseline := runCLIProcess(t, t.TempDir(), []string{"alpha beta", folder}, nil)
	disabled := runCLIProcess(
		t,
		t.TempDir(),
		[]string{"--rerank=false", "alpha beta", folder},
		nil,
	)
	if disabled != baseline {
		t.Fatalf("explicit false changed BM25: baseline=%+v got=%+v", baseline, disabled)
	}

	got := runCLIProcess(t, t.TempDir(), []string{"--rerank", "alpha beta", folder}, nil)
	const warning = "seek: re-ranking is unavailable in this build; using BM25\n"
	if got.code != baseline.code || got.stdout != baseline.stdout || got.stderr != warning {
		t.Fatalf("unavailable backend: baseline=%+v got=%+v", baseline, got)
	}
}
