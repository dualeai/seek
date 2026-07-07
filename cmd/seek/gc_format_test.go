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

// TestGC_TruncateHash pins the CORPUS-cell renderer against literal expected
// output. truncateHash is also the row-locator used by requireRow /
// requireRowOrder, so without an independent literal oracle a bounds bug
// would corrupt the column in production AND shift the test search token
// identically — every table test would pass on the buggy output.
func TestGC_TruncateHash(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0000000000000001", "00000...01"},                 // 16 hex → 5 + "..." + 2
		{"abcdef0123456789abcdef0123456789", "abcde...89"}, // full 32-hex production width
		{"1234567890", "1234567890"},                       // exactly 10 → passthrough
		{"abc", "abc"},                                     // shorter than 10 → passthrough
	}
	for _, c := range cases {
		if got := truncateHash(corpusID(c.in)); got != c.want {
			t.Errorf("truncateHash(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestGC_GCRowResultString pins every ACTION cell rendering, including the
// trashed/failed error tails that no eviction fixture reaches.
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
		{gcRowResult{action: actionTrashed}, "trashed: unknown"}, // nil err defensive path
	}
	for _, c := range cases {
		if got := c.res.String(); got != c.want {
			t.Errorf("gcRowResult{%d}.String() = %q, want %q", c.res.action, got, c.want)
		}
	}
}

// TestGC_CorpusDirSize pins the measured value (not just relative order that
// every sort test checks): the walk sums ALL files across subdirs (a real
// corpus has manifests + multiple shards + .meta sidecars, not one .zoekt),
// and skips .build-* dirs — neither exercised elsewhere.
func TestGC_CorpusDirSize(t *testing.T) {
	root := cacheRootForTest(t)
	dir := seedCorpusWithSize(t, root, fakeCorpusHash(1), time.Now(), 3072)

	// .used is empty (0 bytes), index/fake.zoekt is exactly 3072 → total 3072.
	if got := corpusDirSize(dir); got != 3072 {
		t.Fatalf("corpusDirSize = %d, want 3072", got)
	}

	// Add a non-.zoekt file at the corpus root and a .meta sidecar under
	// index/ — the walk must sum every file, not just shards. 3072+512+128.
	if err := os.WriteFile(filepath.Join(dir, folderManifestFileName), make([]byte, 512), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index", "fake.zoekt.meta"), make([]byte, 128), 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if got := corpusDirSize(dir); got != 3072+512+128 {
		t.Fatalf("corpusDirSize = %d, want %d (all files summed)", got, 3072+512+128)
	}

	// A transient .build-* dir must NOT be counted (avoids reporting the 2x
	// build footprint). Its bytes are excluded, so the total is unchanged.
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

// TestGC_HumanDuration — the AGE cell must never render a negative raw-second
// blob: a `.used` mtime in the future (clock skew, cp -p restore, NTP step)
// clamps to "0s".
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

// TestGC_HumanBytes — the SIZE cell is 6 chars wide (gcTableHeader): values
// must stay within it (TB is the top tier). ≥100 of a unit drops the decimal
// ("154GB", not "154.0GB"); ≥1024GB rolls into TB (not "1536.0GB").
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

// TestGC_FormatRoot_MultibyteTruncationIsValidUTF8 — left-truncation must cut
// on a rune boundary: a byte-sliced CJK path must not leave orphan
// continuation bytes (mojibake) after the ellipsis.
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

// TestGC_FormatRoot_ControlCharsSanitized — a Repository.Source containing
// control characters (interior newline is legal in POSIX paths) must not
// break the one-row-per-corpus table layout.
func TestGC_FormatRoot_ControlCharsSanitized(t *testing.T) {
	info := corpusDisplayInfo{source: "/tmp/evil\npath\twith\rctl"}
	got := formatRoot(info, 52)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("control characters leaked into table cell: %q", got)
	}
}
