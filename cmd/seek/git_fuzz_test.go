package main

import (
	"os"
	"path/filepath"
	"testing"
)

func FuzzParseGitStatusV2(f *testing.F) {
	// Seed with realistic v2 -z outputs (NUL-terminated records)
	f.Add("# branch.oid abc123\x00# branch.head main\x00")
	f.Add("# branch.oid abc123\x001 .M N... 100644 100644 100644 abc def src/main.go\x00")
	f.Add("# branch.oid abc123\x00? new_file.txt\x00")
	f.Add("# branch.oid abc123\x001 A. N... 100644 100644 100644 abc def added.go\x00")
	f.Add("# branch.oid abc123\x00u UU N... 100644 100644 100644 100644 a b c conflict.go\x00")
	f.Add("")
	f.Add("\x00\x00\x00")
	f.Add("# branch.oid abc\x00\x00? \x00")
	f.Fuzz(func(t *testing.T, raw string) {
		// Must never panic
		state := parseGitStatusV2(raw)
		// HeadSHA must always be non-empty
		if state.HeadSHA == "" {
			t.Error("HeadSHA must never be empty")
		}
		// Files must not contain empty strings
		for _, f := range state.Files {
			if f == "" {
				t.Error("file path must not be empty")
			}
		}
	})
}

func FuzzExtractV2Path(f *testing.F) {
	f.Add("1 .M N... 100644 100644 100644 abc def src/main.go", 8)
	f.Add("u UU N... 100644 100644 100644 100644 abc def ghi conflict.go", 10)
	f.Add("short", 5)
	f.Add("", 0)
	f.Add("a b c", 100)
	f.Fuzz(func(t *testing.T, entry string, skipFields int) {
		if skipFields < 0 {
			return // negative skip is not meaningful
		}
		// Must never panic
		_ = extractV2Path(entry, skipFields)
	})
}

// FuzzDetectGitBoundary feeds random bytes as the contents of a `.git`
// worktree pointer file. detectGitBoundary must never panic and must never
// OOM regardless of input. The fixture filesystem is reused across fuzz
// inputs to amortize setup cost.
func FuzzDetectGitBoundary(f *testing.F) {
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
		// Build a valid triad so a well-formed pointer can resolve.
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
		_, _ = detectGitBoundary(worktree, root)
	})
}
