package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testDocumentContentLimit is a small content limit that limit-edge tests swap
// in, so they do not need files near the 4 GiB production limit.
const testDocumentContentLimit = 1 << 20 // 1 MiB

// swapForTest sets a package variable for one test and restores it at
// cleanup. Only sequential tests may call it.
func swapForTest[T any](tb testing.TB, variable *T, value T) {
	tb.Helper()
	old := *variable
	*variable = value
	tb.Cleanup(func() { *variable = old })
}

// swapShardFormatLimitForTest sets shardFormatLimit for one test.
func swapShardFormatLimitForTest(tb testing.TB, limit int64) {
	tb.Helper()
	swapForTest(tb, &shardFormatLimit, limit)
}

// swapShardMaxForTest sets shardMax, the Zoekt shard size target, for one test.
func swapShardMaxForTest(tb testing.TB, limit int) {
	tb.Helper()
	swapForTest(tb, &shardMax, limit)
}

// swapDocumentContentLimitForTest sets documentContentLimit for one test.
func swapDocumentContentLimitForTest(tb testing.TB, limit int64) {
	tb.Helper()
	swapForTest(tb, &documentContentLimit, limit)
}

// TestRun_ContentLimitChangeRebuildsIndex checks that a cached index built
// under one content limit is not reused under another: the second search must
// see the file's content, which the first index held by name only.
func TestRun_ContentLimitChangeRebuildsIndex(t *testing.T) {
	requireTools(t)
	const needle = "content_limit_rebuild_marker"
	content := needle + "\n" + strings.Repeat("filler text line\n", 256)

	search := func(t *testing.T, dir string, operands []string) error {
		t.Helper()
		_, err := searchFrom(t, dir, needle, operands)
		return err
	}
	expectRebuild := func(t *testing.T, dir string, operands []string) {
		t.Helper()
		swapDocumentContentLimitForTest(t, 1024)
		if err := search(t, dir, operands); !errors.Is(err, errNoMatch) {
			t.Fatalf("search under a 1 KiB limit: err=%v, want no content match", err)
		}
		swapDocumentContentLimitForTest(t, testDocumentContentLimit)
		if err := search(t, dir, operands); err != nil {
			t.Fatalf("search after the limit change: err=%v, want the content match", err)
		}
	}

	t.Run("folder", func(t *testing.T) {
		setTestUserCache(t)
		folder := t.TempDir()
		if err := os.WriteFile(filepath.Join(folder, "doc.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		expectRebuild(t, t.TempDir(), []string{folder})
	})
	t.Run("git", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "doc.txt", content)
		expectRebuild(t, repo, nil)
	})
}
