package main

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseGitStatusV2 checks that parseGitStatusV2 does not panic on any
// input, always gives a HEAD value, and lists no empty path.
func FuzzParseGitStatusV2(f *testing.F) {
	// The seeds are git status --porcelain=v2 -z outputs: NUL-terminated
	// records.
	f.Add("# branch.oid abc123\x00# branch.head main\x00")
	f.Add("# branch.oid abc123\x001 .M N... 100644 100644 100644 abc def src/main.go\x00")
	f.Add("# branch.oid abc123\x00? new_file.txt\x00")
	f.Add("# branch.oid abc123\x001 A. N... 100644 100644 100644 abc def added.go\x00")
	f.Add("# branch.oid abc123\x00u UU N... 100644 100644 100644 100644 a b c conflict.go\x00")
	f.Add("")
	f.Add("\x00\x00\x00")
	f.Add("# branch.oid abc\x00\x00? \x00")
	f.Add("# branch.oid \x00")
	f.Fuzz(func(t *testing.T, raw string) {
		state := parseGitStatusV2(raw)
		if state.HeadSHA == "" {
			t.Error("HeadSHA must never be empty")
		}
		for _, f := range state.Files {
			if f == "" {
				t.Error("file path must not be empty")
			}
		}
	})
}

// FuzzExtractV2Path checks that extractV2Path does not panic on any entry and
// field count.
func FuzzExtractV2Path(f *testing.F) {
	f.Add("1 .M N... 100644 100644 100644 abc def src/main.go", 8)
	f.Add("u UU N... 100644 100644 100644 100644 abc def ghi conflict.go", 10)
	f.Add("short", 5)
	f.Add("", 0)
	f.Add("a b c", 100)
	f.Fuzz(func(t *testing.T, entry string, skipFields int) {
		if skipFields < 0 {
			return // A porcelain entry has no negative field count.
		}
		_ = extractV2Path(entry, skipFields)
	})
}

// FuzzDetectGitBoundary writes random bytes as the content of a `.git`
// worktree pointer file: detectGitBoundary must not panic on any content.
// Each input gets its own fixture directory.
func FuzzDetectGitBoundary(f *testing.F) {
	f.Add([]byte("gitdir: ../real-git\n"))
	f.Add([]byte("gitdir: ./real-git\n"))
	f.Add([]byte("gitdir: /absolute/path\n"))
	f.Add([]byte("gitdir: ./real-git\r\n"))
	f.Add([]byte(""))
	f.Add([]byte("not-a-pointer"))
	f.Add([]byte("gitdir:\n"))
	f.Add([]byte("\x00\x00\x00"))
	f.Add([]byte("gitdir:    ../with-spaces   \n"))

	f.Fuzz(func(t *testing.T, payload []byte) {
		root := t.TempDir()
		realGitDir := filepath.Join(root, "real-git")
		// A Git directory with the full triad beside the worktree, so the
		// pointer "gitdir: ../real-git" resolves to it.
		if err := os.MkdirAll(filepath.Join(realGitDir, "objects"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(realGitDir, "refs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realGitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		worktree := filepath.Join(root, "wt")
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, ".git"), payload, 0o644); err != nil {
			t.Fatal(err)
		}
		_, _ = detectGitBoundary(worktree)
	})
}
