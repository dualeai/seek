package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestGC_TruncateHash checks the CORPUS cell against literal values.
// requireRow and requireRowOrder find a row with truncateHash. So a defect in
// truncateHash changes the table and the search key the same way, and only
// literal values find it.
func TestGC_TruncateHash(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0000000000000001", "00000...01"},                 // 16 hex digits: 5, "...", 2
		{"abcdef0123456789abcdef0123456789", "abcde...89"}, // the 32 hex digits of a corpus ID
		{"1234567890", "1234567890"},                       // 10 characters stay whole
		{"abc", "abc"},                                     // fewer than 10 stay whole
	}
	for _, c := range cases {
		if got := truncateHash(corpusID(c.in)); got != c.want {
			t.Errorf("truncateHash(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestGC_GCRowResultString checks each ACTION cell, with the error text of
// trashed and failed that no eviction fixture reaches.
func TestGC_GCRowResultString(t *testing.T) {
	cases := []struct {
		res  gcRowResult
		want string
	}{
		{gcRowResult{action: actionKept}, "kept"},
		{gcRowResult{action: actionEvicted}, "evicted"},
		{gcRowResult{action: actionLocked}, "locked"},
		{gcRowResult{action: actionGone}, "gone"},
		{gcRowResult{action: actionTrashed, err: errors.New("boom")}, "trashed: boom"},
		{gcRowResult{action: actionFailed, err: errors.New("nope")}, "failed: nope"},
		{gcRowResult{action: actionTrashed}, "trashed: unknown"}, // no error
	}
	for _, c := range cases {
		if got := c.res.String(); got != c.want {
			t.Errorf("gcRowResult{%d}.String() = %q, want %q", c.res.action, got, c.want)
		}
	}
}

// TestGC_CorpusDirSize checks the size value, not only the order that the
// sort tests check. The walk adds the size of every file in each subfolder: a
// real corpus has manifests, shards and .meta records. It skips the ledger
// entry (corpusSizeFile) and the build folders (buildDirPrefix).
func TestGC_CorpusDirSize(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpusWithSize(t, root, fakeCorpusHash(1), time.Now(), 3072)

	// The .used marker is empty, and the walk skips the ledger entry, so the
	// total is the 3072 bytes of index/fake.zoekt.
	if got := corpusDirSize(dir); got != 3072 {
		t.Fatalf("corpusDirSize = %d, want 3072", got)
	}

	// A manifest at the corpus root adds 512 bytes, and a .meta record under
	// index/ adds 128. The walk adds every file, not only the shards.
	if err := os.WriteFile(filepath.Join(dir, folderManifestFileName), make([]byte, 512), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index", "fake.zoekt.meta"), make([]byte, 128), 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if got := corpusDirSize(dir); got != 3072+512+128 {
		t.Fatalf("corpusDirSize = %d, want %d (all files summed)", got, 3072+512+128)
	}

	// The walk skips a build folder, so the size does not show the extra disk
	// space of a build. The total stays the same.
	buildDir := filepath.Join(dir, "index", buildDirPrefix+"tmp")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("mkdir build dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "shard.zoekt"), make([]byte, 99999), 0o644); err != nil {
		t.Fatalf("write build shard: %v", err)
	}
	if got := corpusDirSize(dir); got != 3072+512+128 {
		t.Fatalf("corpusDirSize with .build-* present = %d, want %d (build bytes excluded)", got, 3072+512+128)
	}
}

// TestGC_HumanDuration checks the AGE cell. A `.used` time in the future, from
// clock skew, a cp -p restore or an NTP step, shows "0s", not a negative
// count of seconds.
func TestGC_HumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-3 * 24 * time.Hour, "0s"},
		{-time.Second, "0s"},
		{0, "0s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{36 * time.Hour, "1d"},
		{14 * 24 * time.Hour, "14d"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestGC_HumanBytes checks that each value fits the 6-character SIZE cell; TB
// is the largest unit. At 100 or more of a unit, the value has no decimal
// ("154GB", not "154.0GB"). At 1024GB or more, the unit is TB ("1.5TB", not
// "1536.0GB").
func TestGC_HumanBytes(t *testing.T) {
	const (
		kb = int64(1024)
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
	)
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{1023, "1023B"},
		{kb, "1KB"},
		{mb - 1, "1023KB"},
		{mb, "1MB"},
		{5*mb + 300*kb, "5MB"},
		{gb, "1.0GB"},
		{gb + gb/2, "1.5GB"},
		{99*gb + 900*mb, "99.9GB"},
		{100 * gb, "100GB"},
		{154 * gb, "154GB"},
		{1023 * gb, "1023GB"},
		{tb, "1.0TB"},
		{tb + tb/2, "1.5TB"},
		{150 * tb, "150TB"},
	}
	for _, c := range cases {
		got := humanBytes(c.in)
		if got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
		if len(got) > 6 {
			t.Errorf("humanBytes(%d) = %q overflows the 6-char SIZE column", c.in, got)
		}
	}
}

// TestGC_FormatRoot_MultibyteTruncationIsValidUTF8 checks that the cut at the
// left of a long path is on a rune boundary. A CJK path cut at a byte must not
// leave a partial rune after the ellipsis.
func TestGC_FormatRoot_MultibyteTruncationIsValidUTF8(t *testing.T) {
	info := corpusDisplayInfo{source: "/tmp/" + strings.Repeat("日本語", 30)}
	got := formatRoot(info, 20)
	if !utf8.ValidString(got) {
		t.Fatalf("formatRoot returned invalid UTF-8: %q", got)
	}
	if len(got) > 20 {
		t.Fatalf("formatRoot exceeded width budget: %d bytes (%q)", len(got), got)
	}
	if !strings.HasPrefix(got, "...") {
		t.Fatalf("expected ellipsis prefix, got %q", got)
	}
}

// TestGC_FormatRoot_ControlCharsSanitized gives a root with control
// characters; a POSIX path can hold a newline. The ROOT cell drops them, so
// each corpus keeps one row of the table.
func TestGC_FormatRoot_ControlCharsSanitized(t *testing.T) {
	info := corpusDisplayInfo{source: "/tmp/evil\npath\twith\rctl"}
	got := formatRoot(info, 52)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("control characters leaked into table cell: %q", got)
	}
}
