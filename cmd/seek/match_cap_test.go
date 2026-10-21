package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const matchCapMarker = "zqmatchcap"

// matchCapFiles lands in one shard: two files with 10,000 matching lines each
// and one file with a single match. A per-shard cap of 10,000 matches hides at
// least one file in any document order.
var matchCapFiles = map[string]int{"a_big.txt": 10_000, "b_big.txt": 10_000, "z_small.txt": 1}

// writeMatchCapFixture writes matchCapFiles into a new folder and returns it.
func writeMatchCapFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, lines := range matchCapFiles {
		content := strings.Repeat(matchCapMarker+"\n", lines)
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestNoPerShardMatchCap proves that every file of matchCapFiles shows: no
// per-shard match cap hides one.
func TestNoPerShardMatchCap(t *testing.T) {
	requireTools(t)
	root := writeMatchCapFixture(t)
	setTestUserCache(t)

	out := searchMatchCapFixture(t, 0, root)
	for name := range matchCapFiles {
		if !strings.Contains(out, "## "+filepath.Join(root, name)) {
			t.Fatalf("missing %q in output:\n%s", name, lastBytes(out))
		}
	}
}

// TestMatchTotalIndependentOfOperandSplit proves that the file total is the
// same for one folder operand and for one operand per file.
func TestMatchTotalIndependentOfOperandSplit(t *testing.T) {
	requireTools(t)
	root := writeMatchCapFixture(t)
	setTestUserCache(t)

	const want = "(showing 1 of 3)"
	whole := searchMatchCapFixture(t, 1, root)
	var files []string
	for name := range matchCapFiles {
		files = append(files, filepath.Join(root, name))
	}
	split := searchMatchCapFixture(t, 1, files...)
	if !strings.Contains(whole, want) || !strings.Contains(split, want) {
		t.Fatalf("totals differ by operand split, want %q in both:\nwhole:\n%s\nsplit:\n%s", want, lastBytes(whole), lastBytes(split))
	}
}

// searchMatchCapFixture shows one match per file so the captured output stays
// small: captureStdout reads the pipe only after the search returns.
func searchMatchCapFixture(t *testing.T, limit int, operands ...string) string {
	t.Helper()
	out, err := captureStdout(t, func() error {
		return runSearchCommand(
			context.Background(),
			matchCapMarker,
			operands,
			limit,
			1,
			defaultSearchConfig(),
			searchRunConfig{},
		)
	})
	if err != nil {
		t.Fatalf("run %v: %v", operands, err)
	}
	return out
}

// lastBytes returns the last 400 bytes of s, for a short failure message.
func lastBytes(s string) string {
	if len(s) <= 400 {
		return s
	}
	return s[len(s)-400:]
}

// TestHugeContextKeepsLinesAfterTheMatch proves that a context of math.MaxInt
// lines shows the lines before and after the match.
func TestHugeContextKeepsLinesAfterTheMatch(t *testing.T) {
	requireTools(t)
	root := t.TempDir()
	content := "one\ntwo\nzqhugecontext\nfour\nfive\n"
	if err := os.WriteFile(filepath.Join(root, "lines.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	setTestUserCache(t)
	out, err := captureStdout(t, func() error {
		return runWithSearchConfig(context.Background(), "zqhugecontext", []string{root}, explicitSearchConfig(math.MaxInt, false))
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, line := range []string{"1 one", "5 five"} {
		if !strings.Contains(out, line) {
			t.Fatalf("missing context line %q:\n%s", line, out)
		}
	}
}
