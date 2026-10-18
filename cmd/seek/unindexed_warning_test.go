package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

// TestZoektNotIndexedMarker pins what Zoekt stores for a NUL document: the
// full text that the warning fixtures write out, and the unexported prefix that
// warnUnindexedFileOperands reads. A Zoekt upgrade that changes either fails
// here.
func TestZoektNotIndexedMarker(t *testing.T) {
	dir := t.TempDir()
	opts := index.Options{IndexDir: dir, RepositoryDescription: zoekt.Repository{Name: "marker"}}
	opts.SetDefaults()
	builder, err := index.NewBuilder(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Add(index.Document{Name: "nul.bin", Content: []byte("text\x00more\n")}); err != nil {
		t.Fatal(err)
	}
	if err := builder.Finish(); err != nil {
		t.Fatal(err)
	}
	searchers, err := loadShards(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, searcher := range searchers {
			searcher.Close()
		}
	})
	var stored []byte
	for _, searcher := range searchers {
		result, err := searcher.Search(context.Background(), &query.Const{Value: true}, &zoekt.SearchOptions{Whole: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range result.Files {
			if file.FileName == "nul.bin" {
				stored = file.Content
			}
		}
	}
	// Compare Zoekt's real output with literal text, then with the constant
	// that the warning code reads.
	if string(stored) != "NOT-INDEXED: contains binary content" {
		t.Fatalf("stored content of a NUL document = %q, want Zoekt's binary marker", stored)
	}
	if !strings.HasPrefix(string(stored), zoektNotIndexedMarker) {
		t.Fatalf("zoektNotIndexedMarker %q is not the prefix of %q", zoektNotIndexedMarker, stored)
	}
}

// runCapturingWarnings runs one search and returns the "file" attribute of
// every unindexed-file warning.
func runCapturingWarnings(t *testing.T, dir, term string, operands []string) []string {
	t.Helper()
	_, files := searchCapturingWarnings(t, dir, term, operands)
	return files
}

// searchCapturingWarnings runs one search and returns its stdout and the
// "file" attribute of every unindexed-file warning.
func searchCapturingWarnings(t *testing.T, dir, term string, operands []string) (string, []string) {
	t.Helper()
	recorder := captureTestLogs(t, slog.LevelWarn)
	out, err := searchFrom(t, dir, term, operands)
	if err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("run: %v", err)
	}
	var files []string
	for _, record := range recorder.Records() {
		if record.Message == "File content is not searchable" {
			files = append(files, testLogAttrs(record)["file"].(string))
		}
	}
	return out, files
}

// expectWarnings compares warned files with the canonical form of want.
func expectWarnings(t *testing.T, got []string, want ...string) {
	t.Helper()
	canonical := make([]string, len(want))
	for i, path := range want {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		canonical[i] = resolved
	}
	if strings.Join(got, "\n") != strings.Join(canonical, "\n") {
		t.Fatalf("warnings = %q, want %q", got, canonical)
	}
}

func TestRun_WarnsOnlyForUnindexedExactFiles(t *testing.T) {
	requireTools(t)
	const absent = "zz_absent_term"
	const nul = "hello\x00tail\n"
	// Zoekt's stored text for a NUL document, written out so the fixtures do
	// not depend on the code under test.
	const binaryMarker = "NOT-INDEXED: contains binary content"
	// A NUL file with the same length as that stored text.
	nulMarkerLength := strings.Repeat("x", len(binaryMarker)-1) + "\x00"

	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"nul.txt", nul, true},
		{"tiny.txt", "ab", true},
		{"nul-marker-length.bin", nulMarkerLength, true},
		// The first NUL comes after the reader's binary probe; Zoekt still
		// finds it.
		{"late-nul.bin", strings.Repeat("x", 9000) + "\x00tail\n", true},
		{"plain.txt", "plain text without the term\n", false},
		{"marker-lines.txt", binaryMarker + "\nreal text\n", false},
		{"marker-only.txt", binaryMarker, false},
	}
	for _, tc := range cases {
		t.Run("exact file "+tc.name, func(t *testing.T) {
			setTestUserCache(t)
			file := filepath.Join(t.TempDir(), tc.name)
			writeFileAt(t, filepath.Dir(file), filepath.Base(file), tc.content)
			got := runCapturingWarnings(t, t.TempDir(), absent, []string{file})
			if tc.want {
				expectWarnings(t, got, file)
			} else {
				expectWarnings(t, got)
			}
		})
	}

	t.Run("file-name match still warns", func(t *testing.T) {
		setTestUserCache(t)
		file := filepath.Join(t.TempDir(), "needleword.bin")
		writeFileAt(t, filepath.Dir(file), filepath.Base(file), "hello needleword\x00tail\n")
		out, warned := searchCapturingWarnings(t, t.TempDir(), "needleword", []string{file})
		if strings.Count(out, "## ") != 1 {
			t.Fatalf("want one file-name result, got:\n%s", out)
		}
		expectWarnings(t, warned, file)
	})

	t.Run("match on the stored marker text still warns", func(t *testing.T) {
		setTestUserCache(t)
		folder := t.TempDir()
		// "binary" matches Zoekt's stored marker text for the NUL file, and a
		// real line of the text file.
		writeFileAt(t, folder, "nul.bin", nul)
		writeFileAt(t, folder, "marker-lines.txt", binaryMarker+"\nreal text\n")
		nulFile := filepath.Join(folder, "nul.bin")
		got := runCapturingWarnings(t, t.TempDir(), "binary", []string{nulFile, filepath.Join(folder, "marker-lines.txt")})
		expectWarnings(t, got, nulFile)
	})

	t.Run("wrong-case operands on a case-insensitive file system", func(t *testing.T) {
		probe := filepath.Join(t.TempDir(), "Probe.bin")
		writeFileAt(t, filepath.Dir(probe), filepath.Base(probe), "x")
		if _, err := os.Stat(filepath.Join(filepath.Dir(probe), "probe.bin")); err != nil {
			t.Skip("the file system is case-sensitive")
		}
		setTestUserCache(t)
		folder := t.TempDir()
		writeFileAt(t, folder, "Upper.bin", nul)
		typed := filepath.Join(folder, "upper.bin")
		expectWarnings(t, runCapturingWarnings(t, t.TempDir(), absent, []string{typed}), typed)

		repo := initGitRepoNoRemote(t, "Upper.bin", nul)
		expectWarnings(t, runCapturingWarnings(t, repo, absent, []string{"upper.bin"}), filepath.Join(repo, "Upper.bin"))
	})

	t.Run("folder operand has no warning", func(t *testing.T) {
		setTestUserCache(t)
		folder := t.TempDir()
		writeFileAt(t, folder, "nul.txt", nul)
		expectWarnings(t, runCapturingWarnings(t, t.TempDir(), absent, []string{folder}))
	})

	t.Run("folder file inside a folder operand", func(t *testing.T) {
		setTestUserCache(t)
		folder := t.TempDir()
		file := filepath.Join(folder, "loose.bin")
		writeFileAt(t, filepath.Dir(file), filepath.Base(file), nul)
		expectWarnings(t, runCapturingWarnings(t, t.TempDir(), absent, []string{folder, file}), file)
	})

	t.Run("git exact file", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "nul.txt", nul)
		expectWarnings(t, runCapturingWarnings(t, repo, absent, []string{"nul.txt"}), filepath.Join(repo, "nul.txt"))
	})

	t.Run("git unscoped search has no warning", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "nul.txt", nul)
		expectWarnings(t, runCapturingWarnings(t, repo, absent, nil))
	})

	t.Run("git subdirectory file from a subdirectory", func(t *testing.T) {
		setTestUserCache(t)
		repo := initEmptyGitRepoNoRemote(t)
		if err := os.MkdirAll(filepath.Join(repo, "a", "b"), 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(repo, "a", "b", "nul.txt")
		writeFileAt(t, filepath.Dir(file), filepath.Base(file), nul)
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-m", "binary")
		got := runCapturingWarnings(t, filepath.Join(repo, "a"), absent, []string{"b/nul.txt"})
		expectWarnings(t, got, file)
	})

	t.Run("git file inside a directory operand", func(t *testing.T) {
		setTestUserCache(t)
		repo := initEmptyGitRepoNoRemote(t)
		if err := os.MkdirAll(filepath.Join(repo, "a", "b"), 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(repo, "a", "b", "nul.txt")
		writeFileAt(t, filepath.Dir(file), filepath.Base(file), nul)
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-m", "binary")
		expectWarnings(t, runCapturingWarnings(t, repo, absent, []string{"a", "a/b/nul.txt"}), file)
	})

	t.Run("git match in one operand still checks the other", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "plain.txt", "plain words here\n")
		writeFileAt(t, repo, "nul.txt", nul)
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-m", "binary")
		got := runCapturingWarnings(t, repo, "plain", []string{"plain.txt", "nul.txt"})
		expectWarnings(t, got, filepath.Join(repo, "nul.txt"))
	})

	t.Run("git two exact files", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "one.bin", nul)
		writeFileAt(t, repo, "two.bin", "x")
		gitRunIn(t, repo, "add", ".")
		gitRunIn(t, repo, "commit", "-m", "second")
		got := runCapturingWarnings(t, repo, absent, []string{"one.bin", "two.bin"})
		expectWarnings(t, got, filepath.Join(repo, "one.bin"), filepath.Join(repo, "two.bin"))
	})

	t.Run("git dirty text over committed binary has no warning", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "data.txt", nul)
		writeFileAt(t, repo, "data.txt", "now plain text\n")
		expectWarnings(t, runCapturingWarnings(t, repo, absent, []string{"data.txt"}))
	})

	t.Run("git dirty binary over committed text", func(t *testing.T) {
		setTestUserCache(t)
		repo := initGitRepoNoRemote(t, "data.txt", "committed plain text\n")
		writeFileAt(t, repo, "data.txt", nul)
		expectWarnings(t, runCapturingWarnings(t, repo, absent, []string{"data.txt"}), filepath.Join(repo, "data.txt"))
	})
}
