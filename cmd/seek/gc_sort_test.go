package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedCorpusWithSize seeds a corpus like seedCorpus, then writes a fake
// shard file of exactly indexBytes under index/ so the corpus has a
// deterministic corpusDirSize. The file is not a valid zoekt shard —
// readCorpusDisplayInfo swallows the metadata error and renders [empty],
// which is fine for order assertions.
func seedCorpusWithSize(tb testing.TB, cacheRoot, hash string, usedAt time.Time, indexBytes int) string {
	tb.Helper()
	dir := seedCorpus(tb, cacheRoot, hash, usedAt)
	shard := filepath.Join(dir, "index", "fake.zoekt")
	if err := os.WriteFile(shard, make([]byte, indexBytes), 0o644); err != nil {
		tb.Fatalf("write fake shard: %v", err)
	}
	return dir
}

// requireRowOrder asserts the table rows for the given corpora appear in
// exactly the given order. Matches by truncated hash, substring-based like
// requireRow so column-width tweaks don't break it.
func requireRowOrder(tb testing.TB, out string, hashes ...string) {
	tb.Helper()
	shorts := make([]string, len(hashes))
	for i, h := range hashes {
		shorts[i] = truncateHash(corpusID(h))
	}
	var got []string
	for _, line := range strings.Split(out, "\n") {
		for _, s := range shorts {
			if strings.Contains(line, s) {
				got = append(got, s)
				break
			}
		}
	}
	if len(got) != len(shorts) {
		tb.Fatalf("expected %d rows, matched %d (%v)\noutput:\n%s", len(shorts), len(got), got, out)
	}
	for i := range shorts {
		if got[i] != shorts[i] {
			tb.Fatalf("row order mismatch at index %d:\ngot  %v\nwant %v\noutput:\n%s", i, got, shorts, out)
		}
	}
}

func TestGC_ParseGCSortKey(t *testing.T) {
	cases := []struct {
		in      string
		want    gcSortKey
		wantErr bool
	}{
		{in: "name", want: sortByName},
		{in: "age", want: sortByAge},
		{in: "size", want: sortBySize},
		{in: "", wantErr: true},
		{in: "biggest", wantErr: true},
		{in: "SIZE", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseGCSortKey(tc.in)
		if tc.wantErr {
			switch {
			case err == nil:
				t.Errorf("parseGCSortKey(%q): expected error, got %v", tc.in, got)
			case !strings.Contains(err.Error(), "must be one of"):
				t.Errorf("parseGCSortKey(%q): error %q missing valid-values hint", tc.in, err)
			// The accepted values must actually appear — guards against a
			// stale/empty gcSortValues slice leaving a hint with no values.
			case !strings.Contains(err.Error(), "name|age|size"):
				t.Errorf("parseGCSortKey(%q): error %q missing the value list", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseGCSortKey(%q): unexpected error %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("parseGCSortKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestGC_SortGCRows_Direction pins largest-first / oldest-first with the
// tiebreak: the two equal-key rows (aaa,bbb) keep incoming order, ccc sorts
// by its key.
func TestGC_SortGCRows_Direction(t *testing.T) {
	same := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		rows []gcRow
		key  gcSortKey
	}{
		{"size", []gcRow{
			{entry: corpusDirEntry{name: "aaa"}, size: 5},
			{entry: corpusDirEntry{name: "bbb"}, size: 5},
			{entry: corpusDirEntry{name: "ccc"}, size: 9},
		}, sortBySize},
		{"age", []gcRow{
			{entry: corpusDirEntry{name: "aaa", usedAt: same}},
			{entry: corpusDirEntry{name: "bbb", usedAt: same}},
			{entry: corpusDirEntry{name: "ccc", usedAt: same.Add(-time.Hour)}},
		}, sortByAge},
	}
	want := []corpusID{"ccc", "aaa", "bbb"}
	for _, c := range cases {
		sortGCRows(c.rows, c.key)
		got := []corpusID{c.rows[0].entry.name, c.rows[1].entry.name, c.rows[2].entry.name}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: sort order = %v, want %v", c.name, got, want)
				break
			}
		}
	}
}

// TestGC_SortGCRows_StableTiebreak proves the tiebreak is delivered by a
// STABLE sort, not by luck. A small or all-equal fixture cannot: Go's sort
// uses insertion sort (incidentally stable) for n<=12, and pdqsort leaves an
// all-equal run untouched. The revealing pattern is many rows with only TWO
// distinct key values interleaved — quicksort's partitioning reorders the
// equal keys within each group, so sort.Slice fails here while SliceStable
// preserves the ascending-hash order within each group. (Verified: swapping
// SliceStable for Slice makes this test fail.)
func TestGC_SortGCRows_StableTiebreak(t *testing.T) {
	const n = 40
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Two interleaved key groups by parity of the (ascending) hash index.
	mk := func(set func(r *gcRow, hi bool)) []gcRow {
		rows := make([]gcRow, n)
		for i := range rows {
			rows[i] = gcRow{entry: corpusDirEntry{name: corpusID(fakeCorpusHash(i))}}
			set(&rows[i], i%2 == 0)
		}
		return rows
	}
	// Expected stable order: the "first" group (in sort terms) in ascending
	// hash order, then the "second" group in ascending hash order.
	wantOrder := func(firstGroupEven bool) []corpusID {
		var first, second []corpusID
		for i := 0; i < n; i++ {
			h := corpusID(fakeCorpusHash(i))
			if (i%2 == 0) == firstGroupEven {
				first = append(first, h)
			} else {
				second = append(second, h)
			}
		}
		return append(first, second...)
	}
	cases := []struct {
		name      string
		key       gcSortKey
		set       func(r *gcRow, even bool)
		firstEven bool // which parity group sorts first
	}{
		// size DESC: larger sorts first → even rows (size 1) lead.
		{"size", sortBySize, func(r *gcRow, even bool) {
			if even {
				r.size = 1
			}
		}, true},
		// age ASC (oldest first): older .used sorts first → even rows lead.
		{"age", sortByAge, func(r *gcRow, even bool) {
			r.entry.usedAt = old.Add(time.Hour)
			if even {
				r.entry.usedAt = old
			}
		}, true},
	}
	for _, c := range cases {
		rows := mk(c.set)
		sortGCRows(rows, c.key)
		want := wantOrder(c.firstEven)
		for i := range rows {
			if rows[i].entry.name != want[i] {
				t.Fatalf("%s: row %d = %q, want %q (stable order lost)\ngot:  %v",
					c.name, i, rows[i].entry.name, want[i], rowNames(rows))
			}
		}
	}
}

func rowNames(rows []gcRow) []corpusID {
	out := make([]corpusID, len(rows))
	for i, r := range rows {
		out[i] = r.entry.name
	}
	return out
}

// TestGCCmd_DryRun_NoCorpora — an empty cache under --dry-run prints the
// banner + "no corpora" and returns before the table, with NO summary line
// (exercises printGCTableBanner's false return, which the live path ignores
// but the dry-run path consumes as an early return).
func TestGCCmd_DryRun_NoCorpora(t *testing.T) {
	cacheRootForTest(t)
	out := runGCCapture(t, "--dry-run")
	if !strings.Contains(out, "no corpora") {
		t.Fatalf("expected 'no corpora' line; got:\n%s", out)
	}
	if strings.Contains(out, "corpora,") { // the summary line "N corpora, ... total"
		t.Fatalf("empty dry-run must not print a summary line; got:\n%s", out)
	}
}

// TestGCCmd_SortedCtxCancel_EvictsNothing — the sorted branch measures every
// corpus before evicting any. A ctx canceled before the run must therefore
// evict nothing (the "cancel during measurement evicts nothing" contract
// documented in runGC). Every other ctx-cancel test uses the default name
// order, which takes the interleaved branch instead.
func TestGCCmd_SortedCtxCancel_EvictsNothing(t *testing.T) {
	root := cacheRootForTest(t)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	dirs := []string{
		seedCorpusWithSize(t, root, fakeCorpusHash(1), stale, 1024),
		seedCorpusWithSize(t, root, fakeCorpusHash(2), stale, 4096),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = captureStdoutGC(t, func() {
		if err := runGCCommand(ctx, []string{"--force", "--sort=size"}); err != nil {
			t.Fatalf("gc --force --sort=size: %v", err)
		}
	})
	for i, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("sorted-branch ctx-cancel should not evict corpus %d: %v", i, err)
		}
	}
}

// TestGC_EmitGCRow_TrashedCountsAsEvicted pins the record predicate for the
// actionTrashed outcome (rename succeeded, RemoveAll failed — drainTrash
// finishes later). No eviction fixture engineers a failed RemoveAll, so this
// is the only coverage that a trashed corpus still counts toward the freed
// total. A predicate regression to `== actionEvicted` only would undercount.
func TestGC_EmitGCRow_TrashedCountsAsEvicted(t *testing.T) {
	for _, action := range []gcAction{actionEvicted, actionTrashed} {
		stats := &gcTableStats{}
		row := gcRow{entry: corpusDirEntry{name: "x"}, size: 4096}
		emitGCRow(io.Discard, stats, row, time.Now(), gcRowResult{action: action})
		if stats.evictCount != 1 || stats.evictBytes != 4096 {
			t.Errorf("action %d: evictCount=%d evictBytes=%d, want 1/4096",
				action, stats.evictCount, stats.evictBytes)
		}
	}
	// A kept row must NOT count toward the freed total.
	stats := &gcTableStats{}
	emitGCRow(io.Discard, stats, gcRow{size: 4096}, time.Now(), gcRowResult{action: actionKept})
	if stats.evictCount != 0 || stats.evictBytes != 0 {
		t.Errorf("kept row counted as evicted: evictCount=%d evictBytes=%d", stats.evictCount, stats.evictBytes)
	}
}

// TestGC_PredictGCAction_Boundary pins the TTL predicate at exact equality
// (usedAt == cutoff), which no seeded fixture reaches — mtime and the
// runtime-computed cutoff never coincide to the nanosecond. Semantics:
// strictly-after the cutoff is kept, at-or-before is evicted. This is the
// dry-run half of the predict-vs-evict contract; a boundary flip to
// !Before would silently invert it here.
func TestGC_PredictGCAction_Boundary(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		usedAt time.Time
		want   gcAction
	}{
		{"exactly at cutoff", cutoff, actionEvicted},
		{"one ns after", cutoff.Add(time.Nanosecond), actionKept},
		{"one ns before", cutoff.Add(-time.Nanosecond), actionEvicted},
	}
	for _, c := range cases {
		got := predictGCAction(corpusDirEntry{usedAt: c.usedAt}, cutoff).action
		if got != c.want {
			t.Errorf("%s: predictGCAction action = %d, want %d", c.name, got, c.want)
		}
	}
}

// rowProbeWriter records whether a marker file still existed when the row
// for a given corpus was written. Streaming (interleaved) processing renders
// row 1 before later corpora are touched, so a sweep-marker planted in the
// last corpus must still exist; a materialize-everything shape sweeps every
// corpus before rendering anything, so the marker is already gone.
type rowProbeWriter struct {
	rowToken                []byte // truncated hash of the corpus expected in row 1
	markerPath              string
	sawRow                  bool
	markerPresentAtFirstRow bool
	buf                     bytes.Buffer
}

func (w *rowProbeWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if !w.sawRow && bytes.Contains(p, w.rowToken) {
		w.sawRow = true
		_, err := os.Stat(w.markerPath)
		w.markerPresentAtFirstRow = err == nil
	}
	return len(p), nil
}

// TestGC_StreamingDefault_RendersBeforeLaterCorporaProcessed — with the
// default name order the live table must stream: row i renders before
// corpora i+1..n are processed (pre-materialization behavior: incremental
// output, and a Ctrl-C keeps the already-evicted prefix). Ordering-based
// via a synchronous writer probe — no timing, fully deterministic.
func TestGC_StreamingDefault_RendersBeforeLaterCorporaProcessed(t *testing.T) {
	root := cacheRootForTest(t)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	first, last := fakeCorpusHash(1), fakeCorpusHash(2)
	seedCorpus(t, root, first, stale)
	lastDir := seedCorpus(t, root, last, stale)

	// Orphan tmp manifest in the LAST corpus (hash order): deleted by
	// sweepCorpusOrphans when that corpus is processed.
	marker := filepath.Join(lastDir, folderManifestFileName+".tmp")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatalf("chtimes marker: %v", err)
	}

	w := &rowProbeWriter{
		rowToken:   []byte(truncateHash(corpusID(first))),
		markerPath: marker,
	}
	runGC(context.Background(), gcOptions{maxAge: defaultGCMaxAge, skipThrottle: true, writer: w}, 0)

	if !w.sawRow {
		t.Fatalf("no row rendered for first corpus; output:\n%s", w.buf.String())
	}
	if !w.markerPresentAtFirstRow {
		t.Fatalf("first row rendered only after later corpora were already processed (materialized, not streamed)\noutput:\n%s", w.buf.String())
	}
}

func TestGCCmd_DryRun_SortBySize_LargestFirst(t *testing.T) {
	root := cacheRootForTest(t)
	now := time.Now()
	small, large, medium := fakeCorpusHash(1), fakeCorpusHash(2), fakeCorpusHash(3)
	seedCorpusWithSize(t, root, small, now, 1024)
	seedCorpusWithSize(t, root, large, now, 64*1024)
	seedCorpusWithSize(t, root, medium, now, 8*1024)

	out := runGCCapture(t, "--dry-run", "--sort=size")
	requireRowOrder(t, out, large, medium, small)
}

func TestGCCmd_DryRun_SortByAge_OldestFirst(t *testing.T) {
	root := cacheRootForTest(t)
	now := time.Now()
	newest, oldest, middle := fakeCorpusHash(1), fakeCorpusHash(2), fakeCorpusHash(3)
	seedCorpus(t, root, newest, now.Add(-1*time.Hour))
	seedCorpus(t, root, oldest, now.Add(-72*time.Hour))
	seedCorpus(t, root, middle, now.Add(-24*time.Hour))

	out := runGCCapture(t, "--dry-run", "--sort=age")
	requireRowOrder(t, out, oldest, middle, newest)
}

// TestGCCmd_DryRun_DefaultOrderIsHash — without --sort the table must keep
// the historical hash order even when sizes would order differently.
func TestGCCmd_DryRun_DefaultOrderIsHash(t *testing.T) {
	root := cacheRootForTest(t)
	now := time.Now()
	h1, h2, h3 := fakeCorpusHash(1), fakeCorpusHash(2), fakeCorpusHash(3)
	// Sizes deliberately inverse to hash order.
	seedCorpusWithSize(t, root, h1, now, 1024)
	seedCorpusWithSize(t, root, h2, now, 8*1024)
	seedCorpusWithSize(t, root, h3, now, 64*1024)

	out := runGCCapture(t, "--dry-run")
	requireRowOrder(t, out, h1, h2, h3)
}

// TestGCCmd_SortInvalid_Errors — an invalid --sort must error loudly even
// when the run would otherwise be silently throttled (parse happens before
// the throttle gate).
func TestGCCmd_SortInvalid_Errors(t *testing.T) {
	root := cacheRootForTest(t)
	touchStamp(root) // fresh .last-gc: without --force the run would no-op

	err := runGCCommand(context.Background(), []string{"--sort=biggest"})
	if err == nil {
		t.Fatal("expected error for --sort=biggest, got nil")
	}
	if !strings.Contains(err.Error(), "must be one of") {
		t.Fatalf("error %q missing valid-values hint", err)
	}
}

// TestGCCmd_Force_SortBySize_LiveOrderAndEviction — the live table follows
// --sort and stale corpora are actually removed from disk.
func TestGCCmd_Force_SortBySize_LiveOrderAndEviction(t *testing.T) {
	root := cacheRootForTest(t)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	small, large := fakeCorpusHash(1), fakeCorpusHash(2)
	smallDir := seedCorpusWithSize(t, root, small, stale, 1024)
	largeDir := seedCorpusWithSize(t, root, large, stale, 64*1024)

	out := runGCCapture(t, "--force", "--sort=size")
	requireRowOrder(t, out, large, small)
	requireRow(t, out, large, "evicted")
	requireRow(t, out, small, "evicted")
	if _, err := os.Stat(largeDir); err == nil {
		t.Fatalf("large corpus still on disk: %s", largeDir)
	}
	if _, err := os.Stat(smallDir); err == nil {
		t.Fatalf("small corpus still on disk: %s", smallDir)
	}
}

func TestGCCmd_All_DryRun_SortByAge(t *testing.T) {
	root := cacheRootForTest(t)
	now := time.Now()
	newer, older := fakeCorpusHash(1), fakeCorpusHash(2)
	seedCorpus(t, root, newer, now.Add(-1*time.Hour))
	seedCorpus(t, root, older, now.Add(-48*time.Hour))

	out := runGCCapture(t, "--all", "--dry-run", "--sort=age")
	requireRowOrder(t, out, older, newer)
	// --all collapses the TTL to 0: everything is evictable.
	requireRow(t, out, older, "evicted")
	requireRow(t, out, newer, "evicted")
}

// gcTableRows extracts the corpus rows (between the header and the summary)
// from captured gc table output. The header sentinel is the production
// gcTableHeader constant itself, so a column rename can't silently make this
// return zero rows.
func gcTableRows(out string) []string {
	var rows []string
	inTable := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == gcTableHeader:
			inTable = true
		case inTable && strings.HasPrefix(line, "  "):
			rows = append(rows, line)
		default:
			inTable = false
		}
	}
	return rows
}

// assertDryRunLiveParity runs `--dry-run` then `--force` with the same sort
// and asserts the rendered rows match byte-for-byte — the reportGCPlan
// byte-alignment promise. dry-run must run first: it predicts but never
// mutates, so live then sees the identical starting state.
func assertDryRunLiveParity(t *testing.T, wantRows int, sortArg string) {
	t.Helper()
	dry := runGCCapture(t, "--dry-run", sortArg)
	live := runGCCapture(t, "--force", sortArg)
	dryRows, liveRows := gcTableRows(dry), gcTableRows(live)
	if len(dryRows) != wantRows || len(liveRows) != wantRows {
		t.Fatalf("expected %d rows each, got dry=%d live=%d\ndry:\n%s\nlive:\n%s",
			wantRows, len(dryRows), len(liveRows), dry, live)
	}
	for i := range dryRows {
		if dryRows[i] != liveRows[i] {
			t.Fatalf("row %d differs:\ndry:  %q\nlive: %q", i, dryRows[i], liveRows[i])
		}
	}
}

// TestGCCmd_SortBySize_DryRunLiveParity_Kept — fresh (within-TTL) fixtures:
// the live run keeps everything and mutates nothing, so every "kept" row must
// match the dry-run prediction byte-for-byte. Ages are days in the past so
// the AGE cell cannot flip between the two runs.
func TestGCCmd_SortBySize_DryRunLiveParity_Kept(t *testing.T) {
	root := cacheRootForTest(t)
	usedAt := time.Now().Add(-48 * time.Hour)
	seedCorpusWithSize(t, root, fakeCorpusHash(1), usedAt, 1024)
	seedCorpusWithSize(t, root, fakeCorpusHash(2), usedAt, 64*1024)
	assertDryRunLiveParity(t, 2, "--sort=size")
}

// TestGCCmd_SortBySize_DryRunLiveParity_Evicted — stale fixtures: dry-run
// predicts "evicted" rows, live actually evicts them. The rendered rows
// (size captured pre-eviction) must still match byte-for-byte, pinning the
// evicted-row half of the byte-alignment promise that the kept variant
// cannot reach.
func TestGCCmd_SortBySize_DryRunLiveParity_Evicted(t *testing.T) {
	root := cacheRootForTest(t)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	seedCorpusWithSize(t, root, fakeCorpusHash(1), stale, 1024)
	seedCorpusWithSize(t, root, fakeCorpusHash(2), stale, 64*1024)
	assertDryRunLiveParity(t, 2, "--sort=size")
}
