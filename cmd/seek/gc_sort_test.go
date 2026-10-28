package main

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// seedCorpusWithSize seeds a corpus like seedCorpus, then writes a fake shard
// file of exactly indexBytes under index/, so the corpus has a known
// corpusDirSize. The file is not a valid Zoekt shard: readCorpusDisplayInfo
// gives no source, and the ROOT column shows [empty]. The order checks do not
// read that column.
func seedCorpusWithSize(tb testing.TB, cacheRoot, hash string, usedAt time.Time, indexBytes int) string {
	tb.Helper()
	dir := seedCorpus(tb, cacheRoot, hash, usedAt)
	shard := filepath.Join(dir, "index", "fake.zoekt")
	if err := os.WriteFile(shard, make([]byte, indexBytes), 0o644); err != nil {
		tb.Fatalf("write fake shard: %v", err)
	}
	return dir
}

// requireRowOrder checks that the table rows of the given corpora come in
// exactly the given order. As requireRow does, it finds a row by the
// truncated hash in the line, so a change of column width does not break it.
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
			// The hint must name the accepted values, so an empty
			// gcSortValues fails here.
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

// TestGC_SortGCRows_Direction checks largest first and oldest first, with the
// tiebreak. The two rows with equal keys (aaa, bbb) keep their input order,
// and ccc sorts by its key.
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

// TestGC_SortGCRows_StableTiebreak checks that a stable sort gives the
// tiebreak. A small or all-equal fixture cannot show it. Go's sort uses an
// insertion sort, which is stable, for 12 rows or fewer. pdqsort does not
// move an all-equal run. This fixture has 40 rows with two key values in
// turn. The partitions of pdqsort reorder the equal keys of each group, so
// sort.Slice in place of sort.SliceStable fails this test.
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

// TestGCCmd_DryRun_NoCorpora checks that --dry-run on an empty cache prints
// the banner with "no corpora" and no summary line. reportGCPlan returns
// after the banner when the cache has no corpus.
func TestGCCmd_DryRun_NoCorpora(t *testing.T) {
	cacheRootForTest(t)
	out := runGCCapture(t, "--dry-run")
	if !strings.Contains(out, "no corpora") {
		t.Fatalf("expected 'no corpora' line; got:\n%s", out)
	}
	// The summary line is "N corpora, ... total".
	if strings.Contains(out, "corpora,") {
		t.Fatalf("empty dry-run must not print a summary line; got:\n%s", out)
	}
}

// TestGCCmd_SortedCtxCancel_EvictsNothing checks the sorted branch of runGC,
// which measures every corpus before it evicts one. A context canceled before
// the run evicts nothing, as runGC states for a cancel during the
// measurement. The other cancellation tests use the default name order, which
// takes the interleaved branch.
func TestGCCmd_SortedCtxCancel_EvictsNothing(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so an uncanceled run evicts both corpora.
	t.Setenv(envCacheBudget, "1")
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

// TestGC_EmitGCRow_TrashedCountsAsEvicted checks that emitGCRow counts the
// actionTrashed outcome (the rename worked, RemoveAll failed, and drainTrash
// finishes later) in the freed total. No eviction fixture makes RemoveAll
// fail, so only this test covers it. A check for actionEvicted alone would
// count too few.
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

// TestGC_PlanCorpusEviction checks the eviction plan: least recently used
// first, no corpus that the search holds, and a stop at the budget; --all
// evicts everything. The dry-run prediction follows the plan.
func TestGC_PlanCorpusEviction(t *testing.T) {
	root := cacheRootForTest(t)
	t.Setenv(envCacheBudget, strconv.Itoa(2*seededCorpusBytes))
	now := time.Now()
	// Age order is the reverse of name order (hash 4 is the oldest), so a
	// plan in name order fails.
	for i := 1; i <= 4; i++ {
		seedCorpus(t, root, fakeCorpusHash(i), now.Add(-time.Duration(i)*time.Hour))
	}
	entries, err := enumerateCorpusDirs(filepath.Join(root, corporaDir))
	if err != nil {
		t.Fatal(err)
	}
	keep := map[corpusID]bool{corpusID(fakeCorpusHash(4)): true}
	evict, err := planCorpusEviction(root, entries, false, keep, true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[corpusID]bool{corpusID(fakeCorpusHash(3)): true, corpusID(fakeCorpusHash(2)): true}
	if !maps.Equal(evict, want) {
		t.Fatalf("evict=%v, want %v (LRU, skip the held corpus, stop at the budget)", evict, want)
	}
	all, err := planCorpusEviction(root, entries, true, nil, true)
	if err != nil || len(all) != len(entries) {
		t.Fatalf("--all plan=%v err=%v, want every corpus", all, err)
	}
	for _, entry := range entries {
		wantAction := actionKept
		if want[entry.name] {
			wantAction = actionEvicted
		}
		if got := predictGCAction(entry, evict).action; got != wantAction {
			t.Errorf("%s: predicted %d, want %d", entry.name, got, wantAction)
		}
	}
}

// rowProbeWriter records whether a marker file still exists when the row of a
// given corpus is written. The interleaved pass prints row 1 before it sweeps
// the later corpora, so a marker in the last corpus is still there. A pass
// that sweeps every corpus before it prints a row has already removed it.
type rowProbeWriter struct {
	rowToken                []byte // short hash of the corpus in row 1
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

// TestGC_StreamingDefault_RendersBeforeLaterCorporaProcessed checks that the
// live table of the default name order streams. Row i prints before runGC
// processes corpora i+1..n. So the output grows row by row, and a Ctrl-C
// keeps the corpora already evicted. A writer probe checks the order, with no
// timing.
func TestGC_StreamingDefault_RendersBeforeLaterCorporaProcessed(t *testing.T) {
	root := cacheRootForTest(t)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	first, last := fakeCorpusHash(1), fakeCorpusHash(2)
	seedCorpus(t, root, first, stale)
	lastDir := seedCorpus(t, root, last, stale)

	// An orphan tmp manifest in the last corpus (hash order):
	// sweepCorpusOrphans deletes it when runGC processes that corpus.
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
	runGC(context.Background(), gcOptions{skipThrottle: true, writer: w})

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

// TestGCCmd_DryRun_DefaultOrderIsHash checks that with no --sort the table
// keeps the hash order, also when the sizes give another order.
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

// TestGCCmd_SortInvalid_Errors checks that an invalid --sort fails, also when
// the throttle would skip the run: the parse comes before the throttle gate.
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

// TestGCCmd_Force_SortBySize_LiveOrderAndEviction checks that the live table
// follows --sort and that runGC removes the corpora over the budget from disk.
func TestGCCmd_Force_SortBySize_LiveOrderAndEviction(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so GC evicts every corpus that no search holds.
	t.Setenv(envCacheBudget, "1")
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
	// --all evicts every corpus that no search holds.
	requireRow(t, out, older, "evicted")
	requireRow(t, out, newer, "evicted")
}

// gcTableRows returns the corpus rows, between the header and the summary, of
// gc table output. It finds the header by the production gcTableHeader, so a
// column rename cannot make it return no rows with no error.
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

// assertDryRunLiveParity runs --dry-run, then --force, both with --sort=size,
// on a cache of two corpora. It checks that the rows match byte for byte, as
// reportGCPlan states. The dry run goes first: it predicts and changes
// nothing, so the live run starts from the same state.
func assertDryRunLiveParity(t *testing.T) {
	t.Helper()
	dry := runGCCapture(t, "--dry-run", "--sort=size")
	live := runGCCapture(t, "--force", "--sort=size")
	dryRows, liveRows := gcTableRows(dry), gcTableRows(live)
	if len(dryRows) != 2 || len(liveRows) != 2 {
		t.Fatalf("expected 2 rows each, got dry=%d live=%d\ndry:\n%s\nlive:\n%s",
			len(dryRows), len(liveRows), dry, live)
	}
	for i := range dryRows {
		if dryRows[i] != liveRows[i] {
			t.Fatalf("row %d differs:\ndry:  %q\nlive: %q", i, dryRows[i], liveRows[i])
		}
	}
}

// TestGCCmd_SortBySize_DryRunLiveParity_Kept uses corpora within the budget.
// The live run keeps them all and changes nothing, so each "kept" row must
// match the dry-run row byte for byte. The ages are days in the past, so the
// AGE cell cannot change between the two runs.
func TestGCCmd_SortBySize_DryRunLiveParity_Kept(t *testing.T) {
	root := cacheRootForTest(t)
	usedAt := time.Now().Add(-48 * time.Hour)
	seedCorpusWithSize(t, root, fakeCorpusHash(1), usedAt, 1024)
	seedCorpusWithSize(t, root, fakeCorpusHash(2), usedAt, 64*1024)
	assertDryRunLiveParity(t)
}

// TestGCCmd_SortBySize_DryRunLiveParity_Evicted uses corpora over the
// budget: the dry run predicts "evicted" rows, and the live run evicts them.
// The rows, with the size measured before the eviction, must still match
// byte for byte. The kept variant cannot check the evicted rows.
func TestGCCmd_SortBySize_DryRunLiveParity_Evicted(t *testing.T) {
	root := cacheRootForTest(t)
	// Over the budget, so the plan evicts both corpora.
	t.Setenv(envCacheBudget, "1")
	stale := time.Now().Add(-30 * 24 * time.Hour)
	small, large := fakeCorpusHash(1), fakeCorpusHash(2)
	smallDir := seedCorpusWithSize(t, root, small, stale, 1024)
	largeDir := seedCorpusWithSize(t, root, large, stale, 64*1024)
	assertDryRunLiveParity(t)
	// The parity must hold on evicted rows, so the live run must evict both.
	for _, dir := range []string{smallDir, largeDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("live run did not evict %s: err=%v", dir, err)
		}
	}
}
