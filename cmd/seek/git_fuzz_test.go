package main

import (
	"testing"
)

func FuzzParseGitStatusV2(f *testing.F) {
	// Seed with realistic v2 outputs
	f.Add("# branch.oid abc123\n# branch.head main\n")
	f.Add("# branch.oid abc123\n1 .M N... 100644 100644 100644 abc def src/main.go\x00")
	f.Add("# branch.oid abc123\n? new_file.txt\x00")
	f.Add("# branch.oid abc123\n1 A. N... 100644 100644 100644 abc def added.go\x00")
	f.Add("# branch.oid abc123\nu UU N... 100644 100644 100644 100644 a b c conflict.go\x00")
	f.Add("")
	f.Add("\x00\x00\x00")
	f.Add("# branch.oid abc\n\x00\x00? \x00")
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
