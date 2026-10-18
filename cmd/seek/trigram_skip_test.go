package main

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt/index"
)

// highTrigramContent returns text that Zoekt's default TrigramMax would skip.
// Before seek set TrigramMax, Zoekt stored "NOT-INDEXED: contains too many
// trigrams" in place of such a document, so an exact term in it had no match.
// The needle comes last, after the point where the default count is exceeded.
func highTrigramContent(tb testing.TB, needle string) []byte {
	tb.Helper()
	data := randomWordText(1, 96<<10, needle)
	var defaults index.Options
	defaults.SetDefaults()
	var checker index.DocChecker
	if got := checker.Check(data, defaults.TrigramMax, false); got != index.SkipReasonTooManyTrigrams {
		tb.Fatalf("fixture skip reason with Zoekt default TrigramMax=%d: got %v, want %v",
			defaults.TrigramMax, got, index.SkipReasonTooManyTrigrams)
	}
	return data
}

const highTrigramNeedle = "high_trigram_needle_marker"

// searchFrom runs one search from dir and returns its stdout.
func searchFrom(t *testing.T, dir, term string, operands []string) (string, error) {
	t.Helper()
	t.Chdir(dir)
	return captureStdout(t, func() error {
		return run(context.Background(), term, operands)
	})
}

// expectSearchMatch runs one search from dir and requires the file header and
// the matched term in the output.
func expectSearchMatch(t *testing.T, dir string, operands []string, term, header string) {
	t.Helper()
	out, err := searchFrom(t, dir, term, operands)
	if err != nil {
		t.Fatalf("run %q: %v\noutput:\n%s", term, err, out)
	}
	if !strings.Contains(out, header) || !strings.Contains(out, term) {
		t.Fatalf("expected %q with a %q line, got:\n%s", header, term, out)
	}
}

func TestRun_HighTrigramDocumentKeepsSearchableContent(t *testing.T) {
	requireTools(t)

	content := highTrigramContent(t, highTrigramNeedle)

	t.Run("folder", func(t *testing.T) {
		setTestUserCache(t)
		folder := t.TempDir()
		if err := os.WriteFile(filepath.Join(folder, "big.txt"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		expectSearchMatch(t, t.TempDir(), []string{folder}, highTrigramNeedle, "## big.txt")
	})

	t.Run("exact file", func(t *testing.T) {
		setTestUserCache(t)
		folder := t.TempDir()
		file := filepath.Join(folder, "big.txt")
		if err := os.WriteFile(file, content, 0o644); err != nil {
			t.Fatal(err)
		}
		expectSearchMatch(t, t.TempDir(), []string{file}, highTrigramNeedle, "## big.txt")
	})

	t.Run("git committed", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "big.txt", string(content))
		expectSearchMatch(t, repo, nil, highTrigramNeedle, "## big.txt")
	})

	t.Run("git dirty", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "big.txt", "committed content\n")
		if err := os.WriteFile(filepath.Join(repo, "big.txt"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		expectSearchMatch(t, repo, nil, highTrigramNeedle, "## big.txt")
	})
}

// TestRun_HighTrigramDocumentKeepsSearchableContentAfterDelta builds each index
// with a small file first, so the second search adds the large document
// through a delta build instead of a full build.
func TestRun_HighTrigramDocumentKeepsSearchableContentAfterDelta(t *testing.T) {
	requireTools(t)

	const seed = "small_seed_marker"
	content := highTrigramContent(t, highTrigramNeedle)
	small := []byte(seed + "\n")

	t.Run("folder delta", func(t *testing.T) {
		setTestUserCache(t)
		folder := t.TempDir()
		file := filepath.Join(folder, "big.txt")
		if err := os.WriteFile(file, small, 0o644); err != nil {
			t.Fatal(err)
		}
		cwd := t.TempDir()
		expectSearchMatch(t, cwd, []string{folder}, seed, "## big.txt")
		if err := os.WriteFile(file, content, 0o644); err != nil {
			t.Fatal(err)
		}
		expectSearchMatch(t, cwd, []string{folder}, highTrigramNeedle, "## big.txt")
	})

	t.Run("git uncommitted delta", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "big.txt", "committed content\n")
		file := filepath.Join(repo, "big.txt")
		if err := os.WriteFile(file, small, 0o644); err != nil {
			t.Fatal(err)
		}
		expectSearchMatch(t, repo, nil, seed, "## big.txt")
		if err := os.WriteFile(file, content, 0o644); err != nil {
			t.Fatal(err)
		}
		expectSearchMatch(t, repo, nil, highTrigramNeedle, "## big.txt")
	})

	t.Run("git committed delta", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "big.txt", seed+"\n")
		expectSearchMatch(t, repo, nil, seed, "## big.txt")
		if err := os.WriteFile(filepath.Join(repo, "big.txt"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		gitRunIn(t, repo, "commit", "-am", "large document")
		expectSearchMatch(t, repo, nil, highTrigramNeedle, "## big.txt")
	})
}

// randomWordText returns size bytes of high-entropy ASCII words followed by
// needle on its own line.
func randomWordText(seed uint64, size int, needle string) []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	random := rand.New(rand.NewPCG(seed, seed+1))
	var content strings.Builder
	content.Grow(size + len(needle) + 2)
	for word := 0; content.Len() < size; word++ {
		for range 8 {
			content.WriteByte(alphabet[random.IntN(len(alphabet))])
		}
		if word%10 == 9 {
			content.WriteByte('\n')
		} else {
			content.WriteByte(' ')
		}
	}
	content.WriteString("\n" + needle + "\n")
	return []byte(content.String())
}

// TestRun_ShardGuardKeepsOversizedShardsNameOnly lowers the shard format limit
// so a 20 MiB document "wraps" while a 12 MiB one fits. The wrapped document
// must keep only its name, and every other document must stay searchable.
func TestRun_ShardGuardKeepsOversizedShardsNameOnly(t *testing.T) {
	requireTools(t)
	if testing.Short() {
		t.Skip("indexes 32 MiB of text")
	}
	// The same fitting and wrapping documents as the builder-level guard tests.
	const fitsNeedle, wrapsNeedle = "guard_fits_needle", "guard_wraps_needle"
	docs := guardDocs("fits.txt", "wraps.txt")
	fits, wraps := docs[0].content, docs[1].content
	small := []byte("guard_small_needle_marker\n")
	swapShardFormatLimitForTest(t, 60<<20)

	check := func(t *testing.T, dir string, operands []string, wrapsPath string) {
		t.Helper()
		expectSearchMatch(t, dir, operands, fitsNeedle, "## fits.txt")
		expectSearchMatch(t, dir, operands, "guard_small_needle_marker", "## small.txt")
		if out, _ := searchFrom(t, dir, wrapsNeedle, []string{wrapsPath}); strings.Contains(out, "## wraps.txt") {
			t.Fatalf("wrapped content stays searchable:\n%s", out)
		}
		// The name stays searchable.
		out, err := searchFrom(t, dir, "file:wraps", operands)
		if err != nil || !strings.Contains(out, "## wraps.txt") {
			t.Fatalf("file-name search: err=%v, output:\n%s", err, out)
		}
	}

	t.Run("folder full build", func(t *testing.T) {
		setTestUserCache(t)
		folder := t.TempDir()
		writeFileAt(t, folder, "fits.txt", string(fits))
		writeFileAt(t, folder, "wraps.txt", string(wraps))
		writeFileAt(t, folder, "small.txt", string(small))
		check(t, t.TempDir(), []string{folder}, filepath.Join(folder, "wraps.txt"))
	})

	t.Run("folder delta", func(t *testing.T) {
		setTestUserCache(t)
		folder := t.TempDir()
		writeFileAt(t, folder, "fits.txt", string(fits))
		writeFileAt(t, folder, "small.txt", string(small))
		cwd := t.TempDir()
		expectSearchMatch(t, cwd, []string{folder}, fitsNeedle, "## fits.txt")
		writeFileAt(t, folder, "wraps.txt", string(wraps))
		check(t, cwd, []string{folder}, filepath.Join(folder, "wraps.txt"))
	})

	t.Run("git committed", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "small.txt", string(small))
		writeFileAt(t, repo, "fits.txt", string(fits))
		writeFileAt(t, repo, "wraps.txt", string(wraps))
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-m", "large documents")
		check(t, repo, nil, filepath.Join(repo, "wraps.txt"))
	})

	t.Run("git committed delta", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "small.txt", string(small))
		expectSearchMatch(t, repo, nil, "guard_small_needle_marker", "## small.txt")
		writeFileAt(t, repo, "fits.txt", string(fits))
		writeFileAt(t, repo, "wraps.txt", string(wraps))
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-m", "large documents")
		check(t, repo, nil, filepath.Join(repo, "wraps.txt"))
	})

	t.Run("git uncommitted", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "small.txt", string(small))
		writeFileAt(t, repo, "fits.txt", string(fits))
		writeFileAt(t, repo, "wraps.txt", string(wraps))
		check(t, repo, nil, filepath.Join(repo, "wraps.txt"))
	})
}
