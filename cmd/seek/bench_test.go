package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
)

// benchmarkStringSink and benchmarkPlansSink keep the result of a benchmark
// call, so the compiler cannot remove the call.
var (
	benchmarkStringSink string
	benchmarkPlansSink  []node
)

// benchmarkRefreshGit runs the node refresh that a search runs
// (gitNode.refresh): git status, the index checks, and the build when the
// state changed. A stale index is an error: the benchmark must time a build
// that publishes.
func benchmarkRefreshGit(ctx context.Context, paths gitPaths, plan node) error {
	_, indexState, err := ensureGitCorpusFresh(ctx, plan, paths)
	if err == nil && indexState == corpusStale {
		// A failed build serves the last index with no error.
		return errors.New("the refresh left the index stale")
	}
	return err
}

// --- Microbenchmarks of search-time functions ---
// These cover functions that run on each search.

func BenchmarkComputeStateHash_Small(b *testing.B) {
	// A clean repository: the two header records only, about 70 bytes.
	input := "# branch.oid abc123def456789012345678901234567890\x00# branch.head main\x00"
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		computeStateHash(input)
	}
}

func BenchmarkComputeStateHash_Dirty(b *testing.B) {
	// A repository with 50 dirty files and their stat fields: about 5 KB.
	var sb strings.Builder
	sb.WriteString("# branch.oid abc123def456789012345678901234567890\x00# branch.head main\x00")
	for i := range 50 {
		fmt.Fprintf(&sb, "1 .M N... 100644 100644 100644 abc123 def456 src/pkg%d/file%d.go\x00", i, i)
	}
	// The stat fields of each dirty file, as repoStateFingerprint appends
	// them, with fewer fields.
	for i := range 50 {
		fmt.Fprintf(&sb, "\x00src/pkg%d/file%d.go\x001709312345678901234\x0012345\x00", i, i)
	}
	input := sb.String()
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		computeStateHash(input)
	}
}

func BenchmarkParseGitStatusV2_Clean(b *testing.B) {
	raw := "# branch.oid abc123def456789012345678901234567890\x00# branch.head main\x00# branch.upstream origin/main\x00# branch.ab +0 -0\x00"
	b.ReportAllocs()
	for b.Loop() {
		parseGitStatusV2(raw)
	}
}

func BenchmarkParseGitStatusV2_50Files(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("# branch.oid abc123def456789012345678901234567890\x00# branch.head main\x00")
	for i := range 50 {
		fmt.Fprintf(&sb, "1 .M N... 100644 100644 100644 abc123 def456 src/deeply/nested/pkg%d/file%d.go\x00", i, i)
	}
	raw := sb.String()
	b.ReportAllocs()
	for b.Loop() {
		parseGitStatusV2(raw)
	}
}

func BenchmarkParseGitStatusV2_200Files(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("# branch.oid abc123def456789012345678901234567890\x00# branch.head main\x00")
	for i := range 200 {
		if i%3 == 0 {
			fmt.Fprintf(&sb, "? untracked/new_%d.go\x00", i)
		} else {
			fmt.Fprintf(&sb, "1 .M N... 100644 100644 100644 abc123 def456 src/pkg%d/file%d.go\x00", i, i)
		}
	}
	raw := sb.String()
	b.ReportAllocs()
	for b.Loop() {
		parseGitStatusV2(raw)
	}
}

func BenchmarkRepoStateFingerprint_NoFiles(b *testing.B) {
	state := repoState{
		HeadSHA:   "abc123",
		RawOutput: "# branch.oid abc123\x00# branch.head main\x00",
	}
	b.ReportAllocs()
	for b.Loop() {
		repoStateFingerprint("/tmp/fake", state)
	}
}

func BenchmarkRepoStateFingerprint_10Files(b *testing.B) {
	dir := b.TempDir()
	files := make([]string, 10)
	for i := range 10 {
		name := fmt.Sprintf("file_%d.go", i)
		files[i] = name
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("package f%d\n", i)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	state := repoState{
		HeadSHA:   "abc123",
		RawOutput: "# branch.oid abc123\x00",
		Files:     files,
	}
	b.ReportAllocs()
	for b.Loop() {
		repoStateFingerprint(dir, state)
	}
}

func BenchmarkRepoStateFingerprint_50Files(b *testing.B) {
	dir := b.TempDir()
	files := make([]string, 50)
	for i := range 50 {
		name := fmt.Sprintf("pkg/sub/file_%d.go", i)
		files[i] = name
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(fmt.Sprintf("package f%d\n", i)), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	state := repoState{
		HeadSHA:   "abc123",
		RawOutput: "# branch.oid abc123\x00",
		Files:     files,
	}
	b.ReportAllocs()
	for b.Loop() {
		repoStateFingerprint(dir, state)
	}
}

func BenchmarkRepoStateFingerprint_DeletedFiles(b *testing.B) {
	// No file is on disk, so each Lstat fails and the fingerprint records
	// each file as deleted.
	state := repoState{
		HeadSHA:   "abc123",
		RawOutput: "# branch.oid abc123\x00",
		Files:     []string{"gone1.go", "gone2.go", "gone3.go", "gone4.go", "gone5.go"},
	}
	b.ReportAllocs()
	for b.Loop() {
		repoStateFingerprint("/tmp/nonexistent", state)
	}
}

func BenchmarkReadStateFile(b *testing.B) {
	dir := b.TempDir()
	if err := writeStateFile(dir, "abc123def456789a"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		readStateFile(dir)
	}
}

func BenchmarkWriteStateFile(b *testing.B) {
	dir := b.TempDir()
	b.ReportAllocs()
	for b.Loop() {
		if err := writeStateFile(dir, "abc123def456789a"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExtractV2Path(b *testing.B) {
	entry := "1 .M N... 100644 100644 100644 abc123def456 def456abc123 src/deeply/nested/package/file.go"
	b.ReportAllocs()
	for b.Loop() {
		extractV2Path(entry, 8)
	}
}

func BenchmarkResolveGitPaths_Subprocess(b *testing.B) {
	dir := initGitRepo(b, "dummy.go", "package dummy\n")
	origDir, err := os.Getwd()
	if err != nil {
		b.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = os.Chdir(origDir) })
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		// resolveGitPaths has no detectGitBoundary fast path (classifyGitDir
		// has it), so the loop times the Git process alone.
		if _, err := resolveGitPaths(ctx, ""); err != nil {
			b.Fatal(err)
		}
	}
}

// --- Formatter benchmarks ---

const benchmarkFormatterCorpusID corpusID = "benchmark"

func benchmarkGitResults(files []zoekt.FileMatch) []corpusSearchResult {
	return wrapCorpusResults(node{
		id:   benchmarkFormatterCorpusID,
		kind: corpusKindGit,
	}, files)
}

func BenchmarkCorpusFormatting_1File_1Match(b *testing.B) {
	files := []zoekt.FileMatch{
		{
			FileName: "src/main.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("func main() {\n"), LineNumber: 5},
			},
		},
	}
	results := benchmarkGitResults(files)
	b.ReportAllocs()
	for b.Loop() {
		formatCorpusResultsWithContext(results, 0, 0, hideCorpusContext, plainPalette)
	}
}

// BenchmarkCorpusFormatting_LongLineWindowed times the window that the
// formatter cuts around a match. The line is about 4 times maxLineBytes, the
// match is deep in it, and both ends of the window get a cut marker.
func BenchmarkCorpusFormatting_LongLineWindowed(b *testing.B) {
	line := strings.Repeat("x", 2000) + "NEEDLE" + strings.Repeat("y", 2000) + "\n"
	files := []zoekt.FileMatch{{
		FileName: "min.js", Repository: "repo", Language: "JavaScript", Score: 10,
		LineMatches: []zoekt.LineMatch{
			{Line: []byte(line), LineNumber: 1, LineFragments: []zoekt.LineFragmentMatch{{LineOffset: 2000, MatchLength: 6}}},
		},
	}}
	results := benchmarkGitResults(files)
	b.ReportAllocs()
	for b.Loop() {
		formatCorpusResultsWithContext(results, 0, 0, hideCorpusContext, plainPalette)
	}
}

// BenchmarkCorpusFormatting_SanitizedControlChars times the slow path of
// writeSanitized, which drops control characters one rune at a time. The line
// mixes ESC (a C0 control) and U+009C (a C1 control) with text.
func BenchmarkCorpusFormatting_SanitizedControlChars(b *testing.B) {
	line := "func x() { " + strings.Repeat("a\x1bb\u009c", 50) + " }\n"
	files := []zoekt.FileMatch{{
		FileName: "a.go", Repository: "repo", Language: "Go", Score: 10,
		LineMatches: []zoekt.LineMatch{{Line: []byte(line), LineNumber: 1}},
	}}
	results := benchmarkGitResults(files)
	b.ReportAllocs()
	for b.Loop() {
		formatCorpusResultsWithContext(results, 0, 0, hideCorpusContext, plainPalette)
	}
}

func BenchmarkCorpusFormatting_10Files_3Matches(b *testing.B) {
	files := make([]zoekt.FileMatch, 10)
	for i := range 10 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("src/pkg%d/handler.go", i), Repository: "repo", Language: "Go",
			Score: float64(100 - i),
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("func Handle() {\n"), LineNumber: 10, Before: []byte("// comment\n"), After: []byte("    ctx := context.Background()\n")},
				{Line: []byte("    return nil\n"), LineNumber: 25},
				{Line: []byte("func Helper() {\n"), LineNumber: 50, Before: []byte("// helper doc\n")},
			},
		}
	}
	results := benchmarkGitResults(files)
	b.ReportAllocs()
	for b.Loop() {
		formatCorpusResultsWithContext(results, 0, 0, hideCorpusContext, plainPalette)
	}
}

func BenchmarkCorpusFormatting_WithSymbols(b *testing.B) {
	files := make([]zoekt.FileMatch, 20)
	for i := range 20 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("pkg/service%d.go", i), Repository: "repo", Language: "Go",
			Score: float64(20 - i),
			LineMatches: []zoekt.LineMatch{
				{
					Line: []byte("func ProcessRequest(ctx context.Context, req *Request) (*Response, error) {\n"), LineNumber: 42,
					Before: []byte("// ProcessRequest handles the incoming request.\n// It validates input and delegates to the handler.\n"),
					After:  []byte("    if err := validate(req); err != nil {\n        return nil, err\n    }\n"),
					LineFragments: []zoekt.LineFragmentMatch{
						{SymbolInfo: &zoekt.Symbol{Kind: "function"}},
					},
				},
			},
		}
	}
	results := benchmarkGitResults(files)
	b.ReportAllocs()
	for b.Loop() {
		formatCorpusResultsWithContext(results, 0, 0, hideCorpusContext, plainPalette)
	}
}

// buildLargeFixture makes nFiles FileMatches, each with matchesPerFile
// LineMatches. Each line match has Before and After context and a symbol.
func buildLargeFixture(nFiles, matchesPerFile int) []zoekt.FileMatch {
	files := make([]zoekt.FileMatch, nFiles)
	for i := range nFiles {
		matches := make([]zoekt.LineMatch, matchesPerFile)
		for j := range matchesPerFile {
			matches[j] = zoekt.LineMatch{
				Line:       []byte("func ProcessRequest(ctx context.Context) error {\n"),
				LineNumber: 10 + j*20,
				Before:     []byte("// handler doc\n"),
				After:      []byte("    return nil\n"),
				LineFragments: []zoekt.LineFragmentMatch{
					{SymbolInfo: &zoekt.Symbol{Kind: "function"}},
				},
			}
		}
		files[i] = zoekt.FileMatch{
			FileName:    fmt.Sprintf("pkg/service%03d/handler.go", i),
			Repository:  "repo",
			Language:    "Go",
			Score:       float64(nFiles - i),
			LineMatches: matches,
		}
	}
	return files
}

func BenchmarkCorpusFormatting_FileLimit(b *testing.B) {
	files := buildLargeFixture(100, 5)
	results := benchmarkGitResults(files)
	for _, limit := range []int{0, 1, 5, 10, 50} {
		name := "unlimited"
		if limit > 0 {
			name = fmt.Sprintf("limit_%d", limit)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				formatCorpusResultsWithContext(results, limit, 0, hideCorpusContext, plainPalette)
			}
		})
	}
}

func BenchmarkCorpusFormatting_MatchLimit(b *testing.B) {
	files := buildLargeFixture(20, 20)
	results := benchmarkGitResults(files)
	for _, maxMatches := range []int{0, 1, 3, 5} {
		name := "unlimited"
		if maxMatches > 0 {
			name = fmt.Sprintf("max_%d", maxMatches)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				formatCorpusResultsWithContext(results, 0, maxMatches, hideCorpusContext, plainPalette)
			}
		})
	}
}

func BenchmarkCorpusFormatting_Combined(b *testing.B) {
	files := buildLargeFixture(100, 10)
	results := benchmarkGitResults(files)
	cases := []struct {
		name              string
		limit, maxMatches int
	}{
		{"n5_m3", 5, 3},
		{"n1_m1", 1, 1},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				formatCorpusResultsWithContext(results, tc.limit, tc.maxMatches, hideCorpusContext, plainPalette)
			}
		})
	}
}

func BenchmarkSplitContextBytes(b *testing.B) {
	data := []byte("line one\nline two\nline three\n")
	b.ReportAllocs()
	for b.Loop() {
		splitContextBytes(data)
	}
}

func BenchmarkCountContextLines(b *testing.B) {
	data := []byte("line one\nline two\nline three\n")
	b.ReportAllocs()
	for b.Loop() {
		countContextLines(data)
	}
}

// --- Streaming indexer benchmarks ---

func BenchmarkStreamFiles_50Files(b *testing.B) {
	benchmarkStreamFiles(b, 50)
}

func BenchmarkStreamFiles_200Files(b *testing.B) {
	benchmarkStreamFiles(b, 200)
}

// benchmarkStreamFiles times streamDirtyFiles over numFiles small files. The
// benchmark names keep streamFiles for the CodSpeed history.
func benchmarkStreamFiles(b *testing.B, numFiles int) {
	b.Helper()

	dir := b.TempDir()
	files := make([]string, numFiles)
	for i := range numFiles {
		name := fmt.Sprintf("file_%03d.go", i)
		files[i] = name
		_ = os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("package f%d\n// content_%d\n", i, i)), 0o644)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		// The failure rule of the text index build (buildUncommittedStage).
		readCtx, cancel := context.WithCancelCause(b.Context())
		failure := &dirtyFailure{keep: true, stop: cancel}
		for fc := range streamDirtyFiles(readCtx, dir, files, 4, failure) {
			readSemaphore.Release(fc.weight)
		}
		if readErr, ok := errors.AsType[*folderReadError](context.Cause(readCtx)); ok {
			b.Fatal(readErr)
		}
		cancel(nil)
		if err := failure.noted(); err != nil {
			b.Fatal(err)
		}
	}
}

// --- End-to-end benchmarks: they need Git and ctags ---

func BenchmarkPlannedGitCorpus_ColdIndex(b *testing.B) {
	requireTools(b)

	for b.Loop() {
		dir := initGitRepo(b, "app.go", "package main\n\nfunc main() {\n\t// benchmark_marker_cold\n}\n")
		ctx := context.Background()
		paths, plan := planGitTestCorpus(b, dir)
		results, err := runSeekInPlannedGitCorpus(ctx, "benchmark_marker_cold", paths, plan)
		if err != nil {
			b.Fatalf("cold search: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected cold benchmark result")
		}
	}
}

func BenchmarkPlannedGitCorpus_WarmSearch(b *testing.B) {
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n\nfunc main() {\n\t// benchmark_marker_warm\n}\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, dir)
	if results, err := runSeekInPlannedGitCorpus(ctx, "benchmark_marker_warm", paths, plan); err != nil {
		b.Fatalf("warm setup: %v", err)
	} else if len(results) == 0 {
		b.Fatal("expected warm setup result")
	}

	b.ResetTimer()
	for b.Loop() {
		results, err := runSeekInPlannedGitCorpus(ctx, "benchmark_marker_warm", paths, plan)
		if err != nil {
			b.Fatalf("warm search: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected warm benchmark result")
		}
	}
}

func BenchmarkGitPathScoped_WarmSearch(b *testing.B) {
	requireTools(b)

	dir := initGitRepo(b, "seed.go", "package seed\n")
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "b"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "app.go"), []byte("package a\n// path_scoped_bench_marker\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b", "app.go"), []byte("package b\n// path_scoped_bench_marker\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "add path scoped benchmark files"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			b.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	ctx := context.Background()
	paths, _ := planGitTestCorpus(b, dir)
	scopedPlan, err := planGitCorpus(paths, []string{filepath.Join(dir, "a")})
	if err != nil {
		b.Fatalf("plan scoped corpus: %v", err)
	}

	if results, err := runSeekInPlannedGitCorpus(ctx, "path_scoped_bench_marker", paths, scopedPlan); err != nil {
		b.Fatalf("scoped setup: %v", err)
	} else {
		assertBenchmarkResultsOnly(b, results, "a/app.go")
	}

	b.ResetTimer()
	for b.Loop() {
		if _, err := runSeekInPlannedGitCorpus(ctx, "path_scoped_bench_marker", paths, scopedPlan); err != nil {
			b.Fatalf("search scoped corpus: %v", err)
		}
	}
}

func BenchmarkGitPathScoped_PlanCorpora(b *testing.B) {
	requireGit(b)

	dir := initGitRepo(b, "seed.go", "package seed\n")
	scope := filepath.Join(dir, "a")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package a\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	gitRunIn(b, dir, "add", ".")
	gitRunIn(b, dir, "commit", "-m", "add scoped planning files")

	ctx := context.Background()
	operands := []string{scope}
	if plans, err := planCorpora(ctx, operands); err != nil {
		b.Fatalf("plan setup: %v", err)
	} else if len(plans) != 1 {
		b.Fatalf("plans=%d, want 1", len(plans))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		plans, err := planCorpora(ctx, operands)
		if err != nil {
			b.Fatalf("plan scoped corpus: %v", err)
		}
		benchmarkPlansSink = plans
	}
}

func BenchmarkGitPathScoped_ColdIndexWithTrackedSibling(b *testing.B) {
	requireTools(b)

	dir, scope := writeScopedGitBenchmarkRepo(b)
	ctx := context.Background()
	paths, _ := planGitTestCorpus(b, dir)
	scopedPlan, err := planGitCorpus(paths, []string{scope})
	if err != nil {
		b.Fatalf("plan scoped corpus: %v", err)
	}
	if err := os.RemoveAll(scopedPlan.cacheDir); err != nil {
		b.Fatal(err)
	}
	if results, err := runSeekInPlannedGitCorpus(ctx, "scoped_cold_marker", paths, scopedPlan); err != nil {
		b.Fatalf("cold scoped setup: %v", err)
	} else {
		assertBenchmarkResultsOnly(b, results, "a/app.go")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		if err := os.RemoveAll(scopedPlan.cacheDir); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()

		if _, err := runSeekInPlannedGitCorpus(ctx, "scoped_cold_marker", paths, scopedPlan); err != nil {
			b.Fatalf("cold scoped corpus: %v", err)
		}
	}
}

func BenchmarkGitPathScoped_DirtyReindexWithTrackedSibling(b *testing.B) {
	requireTools(b)

	dir, scope := writeScopedGitBenchmarkRepo(b)
	ctx := context.Background()
	paths, _ := planGitTestCorpus(b, dir)
	scopedPlan, err := planGitCorpus(paths, []string{scope})
	if err != nil {
		b.Fatalf("plan scoped corpus: %v", err)
	}
	if results, err := runSeekInPlannedGitCorpus(ctx, "scoped_cold_marker", paths, scopedPlan); err != nil {
		b.Fatalf("warm scoped setup: %v", err)
	} else {
		assertBenchmarkResultsOnly(b, results, "a/app.go")
	}

	target := filepath.Join(scope, "app.go")
	setupMarker := "scoped_dirty_marker_setup"
	if err := os.WriteFile(target, []byte("package a\n// scoped_cold_marker\n// "+setupMarker+"\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	if results, err := runSeekInPlannedGitCorpus(ctx, setupMarker, paths, scopedPlan); err != nil {
		b.Fatalf("dirty scoped setup: %v", err)
	} else {
		assertBenchmarkResultsOnly(b, results, "a/app.go")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		marker := fmt.Sprintf("scoped_dirty_marker_%d", i)
		body := fmt.Appendf(nil, "package a\n// scoped_cold_marker\n// %s\n", marker)
		if err := os.WriteFile(target, body, 0o644); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		// The timer covers the refresh and the search, not the file write.
		// Compare only with earlier results of this benchmark.
		if _, err := runSeekInPlannedGitCorpus(ctx, marker, paths, scopedPlan); err != nil {
			b.Fatalf("dirty scoped corpus: %v", err)
		}
	}
}

func BenchmarkGitPathScoped_OutOfScopeCommitReusesCommittedLayer(b *testing.B) {
	requireTools(b)

	dir, scope := writeScopedGitBenchmarkRepo(b)
	ctx := context.Background()
	paths, _ := planGitTestCorpus(b, dir)
	scopedPlan, err := planGitCorpus(paths, []string{scope})
	if err != nil {
		b.Fatalf("plan scoped corpus: %v", err)
	}
	if results, err := runSeekInPlannedGitCorpus(ctx, "scoped_cold_marker", paths, scopedPlan); err != nil {
		b.Fatalf("warm scoped setup: %v", err)
	} else {
		assertBenchmarkResultsOnly(b, results, "a/app.go")
	}

	sibling := filepath.Join(dir, "b")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		name := filepath.Join(sibling, fmt.Sprintf("outside_%03d.go", i))
		body := fmt.Appendf(nil, "package b\n// outside scoped commit %d\n", i)
		if err := os.WriteFile(name, body, 0o644); err != nil {
			b.Fatal(err)
		}
		gitRunIn(b, dir, "add", "b")
		gitRunIn(b, dir, "commit", "-m", "out of scope benchmark commit")
		b.StartTimer()

		if _, err := runSeekInPlannedGitCorpus(ctx, "scoped_cold_marker", paths, scopedPlan); err != nil {
			b.Fatalf("search after out-of-scope commit: %v", err)
		}
	}
}

// writeScopedGitBenchmarkRepo commits a repository whose folder a is the
// scope and whose folder b is a tracked sibling. b/app.go holds the marker of
// a/app.go, beside 200 more files. It returns the repository and the scope.
func writeScopedGitBenchmarkRepo(b *testing.B) (repoDir, scope string) {
	b.Helper()
	dir := initGitRepo(b, "seed.go", "package seed\n")
	scope = filepath.Join(dir, "a")
	sibling := filepath.Join(dir, "b")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "app.go"), []byte("package a\n// scoped_cold_marker\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "app.go"), []byte("package b\n// scoped_cold_marker\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	for i := range 200 {
		body := []byte("package b\n" + strings.Repeat("// tracked sibling payload\n", 32))
		if err := os.WriteFile(filepath.Join(sibling, fmt.Sprintf("sibling_%03d.go", i)), body, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	gitRunIn(b, dir, "add", ".")
	gitRunIn(b, dir, "commit", "-m", "add scoped benchmark files")
	return dir, scope
}

func assertBenchmarkResultsOnly(b *testing.B, results []string, want ...string) {
	b.Helper()
	if len(results) != len(want) {
		b.Fatalf("results=%v, want exactly %v", results, want)
	}
	for _, expected := range want {
		found := false
		for _, got := range results {
			if got == expected {
				found = true
				break
			}
		}
		if !found {
			b.Fatalf("results=%v, want exactly %v", results, want)
		}
	}
}

func BenchmarkPlannedGitCorpus_DirtyReindex(b *testing.B) {
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n\nfunc main() {\n\t// dirty_bench\n}\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, dir)
	if results, err := runSeekInPlannedGitCorpus(ctx, "dirty_bench", paths, plan); err != nil {
		b.Fatalf("dirty setup: %v", err)
	} else if len(results) == 0 {
		b.Fatal("expected dirty setup result")
	}

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		// Each round edits app.go with a new marker.
		marker := fmt.Sprintf("dirty_bench_iter_%d", i)
		content := fmt.Sprintf("package main\n\nfunc main() {\n\t// %s\n}\n", marker)
		if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()

		results, err := runSeekInPlannedGitCorpus(ctx, marker, paths, plan)
		if err != nil {
			b.Fatalf("dirty search: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected dirty benchmark result")
		}
	}
}

// BenchmarkSmallRepo_Phases times each phase of the dirty refresh and of the
// search on a small repository. It needs no SEEK_BENCH_REPO.
func BenchmarkSmallRepo_Phases(b *testing.B) {
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n\nfunc main() {\n\t// phase_bench\n}\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, dir)

	if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
		b.Fatalf("initial indexing: %v", err)
	}

	// original is the committed app.go. The uncommitted phases append to it
	// to make the file dirty.
	original := []byte("package main\n\nfunc main() {\n\t// phase_bench\n}\n")

	b.Run("gitRepoStateIn", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			mustGitRepoStateIn(b, ctx, dir)
		}
	})

	b.Run("gitHeadTreeish", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := gitHeadTreeish(ctx, dir); err != nil {
				b.Fatalf("git head treeish: %v", err)
			}
		}
	})

	b.Run("stateHash_clean", func(b *testing.B) {
		state := mustGitRepoStateIn(b, ctx, dir)
		b.ReportAllocs()
		for b.Loop() {
			gitCorpusStateHash(paths, state)
		}
	})

	b.Run("checkCtagsCached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = checkCtagsCached()
		}
	})

	b.Run("planGitCorpus", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = planGitCorpus(paths, nil)
		}
	})

	b.Run("refreshCommitted_cached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := benchmarkRefreshGit(ctx, paths, plan); err != nil {
				b.Fatalf("index committed: %v", err)
			}
		}
	})

	b.Run("indexUncommitted_1file", func(b *testing.B) {
		if err := os.WriteFile(filepath.Join(dir, "app.go"), append(original, []byte("\n// dirty\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		state := mustGitRepoStateIn(b, ctx, dir)
		if len(state.Files) == 0 {
			b.Fatal("expected dirty files")
		}
		cachedState := readStateFile(plan.cacheDir)
		preState := gitCorpusStateHash(paths, state)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			content := fmt.Appendf(original[:len(original):len(original)], "\n// p_%d\n", i)
			if err := os.WriteFile(filepath.Join(dir, "app.go"), content, 0o644); err != nil {
				b.Fatal(err)
			}
			// The rounds use the first capture for its paths only.
			// indexUncommittedForBench stats each file again in each round
			// (statUncommittedCandidates), so it sees the new size and mtime.
			if err := indexUncommittedForBench(ctx, dir, plan.corpusStore, state, cachedState, preState); err != nil {
				b.Fatalf("index uncommitted: %v", err)
			}
			// A build records the state it published: the next round reads
			// a delta of the one changed file, as a real refresh does.
			cachedState = preState
		}
	})

	b.Run("postVerify_restat", func(b *testing.B) {
		if err := os.WriteFile(filepath.Join(dir, "app.go"), append(original, []byte("\n// restat\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		dirtyState := mustGitRepoStateIn(b, ctx, dir)
		if len(dirtyState.Files) == 0 {
			b.Fatal("expected dirty files")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			gitCorpusStateHash(paths, dirtyState)
		}
	})

	// The names of the next two sub-benchmarks stay for the CodSpeed history:
	// both time the node read (searchPlannedCorpusParsed).
	b.Run("executeParsedSearchScoped", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := searchPlannedCorpusForTest(ctx, plan, "phase_bench"); err != nil {
				b.Fatalf("execute search: %v", err)
			}
		}
	})

	b.Run("executeParsedSearchScopedDirs_withEmptyOptionalDir", func(b *testing.B) {
		userQ, err := parseSearchQuery("phase_bench")
		if err != nil {
			b.Fatal(err)
		}

		b.Run("singleDir", func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := searchPlannedCorpusParsed(ctx, plan, nil, userQ, defaultSearchConfig()); err != nil {
					b.Fatalf("execute search with one dir: %v", err)
				}
			}
		})
	})
}

func BenchmarkFolderCorpus_ColdIndex(b *testing.B) {
	requireTools(b)
	ctx := context.Background()

	for b.Loop() {
		b.StopTimer()
		root := writeBenchmarkFolder(b, 200, "folder_marker_cold")
		plan := planFolderTestCorpus(b, root)
		b.StartTimer()

		if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			b.Fatalf("ensure folder corpus: %v", err)
		} else if indexState == corpusKnownEmpty {
			b.Fatal("folder corpus should not be empty")
		}
		results, err := searchPlannedCorpusForTest(ctx, plan, "folder_marker_cold")
		if err != nil {
			b.Fatalf("search folder corpus: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected folder search result")
		}
	}
}

func BenchmarkFolderCorpus_WarmSearch(b *testing.B) {
	requireTools(b)
	ctx := context.Background()
	root := writeBenchmarkFolder(b, 200, "folder_marker_warm")
	plan := planFolderTestCorpus(b, root)
	if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		b.Fatalf("ensure folder corpus: %v", err)
	} else if indexState == corpusKnownEmpty {
		b.Fatal("folder corpus should not be empty")
	}

	b.ResetTimer()
	for b.Loop() {
		if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			b.Fatalf("ensure warm folder corpus: %v", err)
		} else if indexState == corpusKnownEmpty {
			b.Fatal("warm folder corpus should not be empty")
		}
		results, err := searchPlannedCorpusForTest(ctx, plan, "folder_marker_warm")
		if err != nil {
			b.Fatalf("search folder corpus: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected folder search result")
		}
	}
}

func BenchmarkFolderCorpus_WarmSearchAfterDeltas(b *testing.B) {
	requireTools(b)
	ctx := context.Background()
	root := writeBenchmarkFolder(b, 200, "folder_marker_delta_warm")
	plan := planFolderTestCorpus(b, root)
	if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		b.Fatalf("ensure folder corpus: %v", err)
	} else if indexState == corpusKnownEmpty {
		b.Fatal("folder corpus should not be empty")
	}
	path := filepath.Join(root, "pkg00", "file_000.go")
	for i := range 16 {
		content := fmt.Sprintf("package pkg00\n\nfunc Fn0() string { return %q }\n// folder_marker_delta_warm_%03d\n", "dirty", i)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
		if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			b.Fatalf("ensure dirty folder corpus: %v", err)
		} else if indexState == corpusKnownEmpty {
			b.Fatal("dirty folder corpus should not be empty")
		}
	}

	b.ResetTimer()
	for b.Loop() {
		if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			b.Fatalf("ensure warm folder corpus: %v", err)
		} else if indexState == corpusKnownEmpty {
			b.Fatal("warm folder corpus should not be empty")
		}
		results, err := searchPlannedCorpusForTest(ctx, plan, "folder_marker_delta_warm")
		if err != nil {
			b.Fatalf("search folder corpus: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected folder search result")
		}
	}
}

func BenchmarkExternalExactFile_ColdIndex(b *testing.B) {
	requireTools(b)
	ctx := context.Background()

	for b.Loop() {
		b.StopTimer()
		root := b.TempDir()
		path := filepath.Join(root, "single.go")
		if err := os.WriteFile(path, []byte("package single\n// exact_file_marker_cold\n"), 0o644); err != nil {
			b.Fatal(err)
		}
		plan := planFolderTestCorpus(b, path)
		b.StartTimer()

		if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			b.Fatalf("ensure exact file corpus: %v", err)
		} else if indexState == corpusKnownEmpty {
			b.Fatal("exact file corpus should not be empty")
		}
		results, err := searchPlannedCorpusForTest(ctx, plan, "exact_file_marker_cold")
		if err != nil {
			b.Fatalf("search exact file corpus: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected exact file search result")
		}
	}
}

func BenchmarkExternalExactFile_WarmSearch(b *testing.B) {
	requireTools(b)
	ctx := context.Background()
	root := b.TempDir()
	path := filepath.Join(root, "single.go")
	if err := os.WriteFile(path, []byte("package single\n// exact_file_marker_warm\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	plan := planFolderTestCorpus(b, path)
	if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		b.Fatalf("ensure exact file corpus: %v", err)
	} else if indexState == corpusKnownEmpty {
		b.Fatal("exact file corpus should not be empty")
	}

	b.ResetTimer()
	for b.Loop() {
		if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			b.Fatalf("ensure warm exact file corpus: %v", err)
		} else if indexState == corpusKnownEmpty {
			b.Fatal("warm exact file corpus should not be empty")
		}
		results, err := searchPlannedCorpusForTest(ctx, plan, "exact_file_marker_warm")
		if err != nil {
			b.Fatalf("search exact file corpus: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected exact file search result")
		}
	}
}

// BenchmarkMultiCorpus_WarmSearchAndFormat searches two warm folder nodes one
// after the other and formats their results. It skips the command route
// (searchNodes, the BM25 rescore, the headers); BenchmarkMultiNode_WarmSearch
// times that route.
func BenchmarkMultiCorpus_WarmSearchAndFormat(b *testing.B) {
	requireTools(b)
	ctx := context.Background()
	rootA := writeBenchmarkFolder(b, 100, "multi_corpus_marker")
	rootB := writeBenchmarkFolder(b, 100, "multi_corpus_marker")
	planA := planFolderTestCorpus(b, rootA)
	planB := planFolderTestCorpus(b, rootB)
	for _, plan := range []node{planA, planB} {
		if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			b.Fatalf("ensure corpus: %v", err)
		} else if indexState == corpusKnownEmpty {
			b.Fatal("multi corpus fixture should not be empty")
		}
	}
	userQ, err := parseSearchQuery("multi_corpus_marker")
	if err != nil {
		b.Fatalf("parse query: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		resultsA, _, err := prepareAndSearchCorpus(ctx, planA, nil, userQ, defaultSearchConfig())
		if err != nil {
			b.Fatalf("search corpus A: %v", err)
		}
		resultsB, _, err := prepareAndSearchCorpus(ctx, planB, nil, userQ, defaultSearchConfig())
		if err != nil {
			b.Fatalf("search corpus B: %v", err)
		}
		results := append(resultsA, resultsB...)
		benchmarkStringSink = formatCorpusResultsWithContext(results, 0, 0, showCorpusContext, plainPalette)
	}
}

func BenchmarkFormatCorpusResults_TwoCorpora(b *testing.B) {
	results := benchmarkCorpusResultsForFormatting()

	b.ReportAllocs()
	for b.Loop() {
		benchmarkStringSink = formatCorpusResultsWithContext(results, 0, 0, showCorpusContext, plainPalette)
	}
}

func BenchmarkWriteCorpusResults_TwoCorpora(b *testing.B) {
	results := benchmarkCorpusResultsForFormatting()

	b.ReportAllocs()
	for b.Loop() {
		output := bufio.NewWriter(io.Discard)
		wrote, err := writeCorpusResultsWithContext(
			output,
			results,
			0,
			0,
			showCorpusContext,
			plainPalette,
		)
		if err == nil {
			err = output.Flush()
		}
		if err != nil || !wrote {
			b.Fatalf("write results: wrote=%t error=%v", wrote, err)
		}
	}
}

func benchmarkCorpusResultsForFormatting() []corpusSearchResult {
	results := make([]corpusSearchResult, 0, 200)
	for i := range 100 {
		name := fmt.Sprintf("src/file_%03d.go", i)
		results = append(results,
			corpusSearchResult{
				corpusID:    corpusID("a"),
				kind:        corpusKindFolder,
				displayRoot: "/tmp/a",
				file:        benchmarkFileMatch(name, float64(100-i)),
			},
			corpusSearchResult{
				corpusID:    corpusID("b"),
				kind:        corpusKindFolder,
				displayRoot: "/tmp/b",
				file:        benchmarkFileMatch(name, float64(50-i)),
			},
		)
	}
	return results
}

func BenchmarkFolderCorpusState_200Files(b *testing.B) {
	benchmarkFolderCorpusState(b, 200)
}

func BenchmarkFolderCorpusState_1000Files(b *testing.B) {
	benchmarkFolderCorpusState(b, 1000)
}

func BenchmarkFolderCorpusFingerprint_200Files(b *testing.B) {
	benchmarkFolderCorpusFingerprint(b, 200)
}

func BenchmarkFolderCorpusFingerprint_1000Files(b *testing.B) {
	benchmarkFolderCorpusFingerprint(b, 1000)
}

func BenchmarkChangedFolderDocumentsFromManifest_1000Files_1Changed(b *testing.B) {
	root := writeBenchmarkFolder(b, 1000, "folder_manifest_marker")
	plan := planFolderTestCorpus(b, root)
	ctx := context.Background()

	_, oldSelected, err := folderCorpusState(ctx, plan)
	if err != nil {
		b.Fatalf("old folder state: %v", err)
	}
	if err := writeFolderManifest(plan.cacheDir, "old-state", oldSelected, 0, nil); err != nil {
		b.Fatalf("write folder manifest: %v", err)
	}
	manifest, ok := readFolderManifest(plan.cacheDir, "old-state")
	if !ok {
		b.Fatal("expected folder manifest")
	}

	path := filepath.Join(root, "pkg00", "file_000.go")
	content := "package pkg00\n\nfunc Fn0() string { return \"manifest_dirty\" }\n// changed\n// extra\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		b.Fatal(err)
	}
	_, selected, err := folderCorpusState(ctx, plan)
	if err != nil {
		b.Fatalf("new folder state: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		changedDocs, changedPaths, _ := changedFolderDocumentsFromManifest(selected, manifest, indexWindowBytes)
		if len(changedDocs) != 1 || len(changedPaths) != 1 {
			b.Fatalf("changed docs=%d paths=%d, want 1 each", len(changedDocs), len(changedPaths))
		}
		if changedDocs[0].name != "pkg00/file_000.go" {
			b.Fatalf("changed doc=%q, want pkg00/file_000.go", changedDocs[0].name)
		}
	}
}

func benchmarkFolderCorpusState(b *testing.B, files int) {
	root := writeBenchmarkFolder(b, files, "folder_state_marker")
	plan := planFolderTestCorpus(b, root)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		state, selected, err := folderCorpusState(ctx, plan)
		if err != nil {
			b.Fatalf("folder state: %v", err)
		}
		if state == "" || len(selected) == 0 {
			b.Fatal("expected non-empty folder state")
		}
	}
}

func benchmarkFolderCorpusFingerprint(b *testing.B, files int) {
	root := writeBenchmarkFolder(b, files, "folder_fingerprint_marker")
	plan := planFolderTestCorpus(b, root)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		state, selectedCount, err := folderCorpusFingerprint(ctx, plan)
		if err != nil {
			b.Fatalf("folder fingerprint: %v", err)
		}
		if state == "" || selectedCount == 0 {
			b.Fatal("expected non-empty folder fingerprint")
		}
	}
}

func BenchmarkFolderCorpus_DirtyReindex_1File(b *testing.B) {
	benchmarkFolderCorpusDirtyReindex(b, false)
}

func BenchmarkFolderCorpus_DirtyReindexAndSearch_1File(b *testing.B) {
	benchmarkFolderCorpusDirtyReindex(b, true)
}

func benchmarkFolderCorpusDirtyReindex(b *testing.B, searchAfterRefresh bool) {
	b.Helper()
	requireTools(b)
	ctx := context.Background()
	root := writeBenchmarkFolder(b, 200, "folder_dirty_marker")
	plan := planFolderTestCorpus(b, root)
	if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		b.Fatalf("ensure folder corpus: %v", err)
	} else if indexState == corpusKnownEmpty {
		b.Fatal("folder corpus should not be empty")
	}
	path := filepath.Join(root, "pkg00", "file_000.go")

	b.ReportAllocs()
	var lastMarker string
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		marker := fmt.Sprintf("folder_dirty_marker_%03d", i)
		lastMarker = marker
		content := fmt.Sprintf("package pkg00\n\nfunc Fn0() string { return %q }\n// %s\n", "dirty", marker)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()

		if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			b.Fatalf("ensure dirty folder corpus: %v", err)
		} else if indexState == corpusKnownEmpty {
			b.Fatal("dirty folder corpus should not be empty")
		}
		if searchAfterRefresh {
			results, err := searchPlannedCorpusForTest(ctx, plan, marker)
			if err != nil {
				b.Fatalf("search dirty folder corpus: %v", err)
			}
			if len(results) == 0 {
				b.Fatal("expected dirty folder search result")
			}
		}
	}
	if !searchAfterRefresh && lastMarker != "" {
		b.StopTimer()
		results, err := searchPlannedCorpusForTest(ctx, plan, lastMarker)
		if err != nil {
			b.Fatalf("search last dirty folder marker: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected last dirty folder marker to be searchable")
		}
	}
}

// BenchmarkFolderCorpus_DirtyReindexPhases_1File times the main production
// steps of a dirty folder refresh, one at a time.
// BenchmarkFolderCorpus_DirtyReindex_1File times the whole refresh.
func BenchmarkFolderCorpus_DirtyReindexPhases_1File(b *testing.B) {
	requireTools(b)
	ctx := context.Background()
	root := writeBenchmarkFolder(b, 200, "folder_dirty_phase_marker")
	plan := planFolderTestCorpus(b, root)
	if indexState, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		b.Fatalf("ensure folder corpus: %v", err)
	} else if indexState == corpusKnownEmpty {
		b.Fatal("folder corpus should not be empty")
	}
	cachedState := readStateFile(plan.cacheDir)
	manifest, ok := readFolderManifest(plan.cacheDir, cachedState)
	if !ok {
		b.Fatal("expected folder manifest")
	}
	path := filepath.Join(root, "pkg00", "file_000.go")
	content := []byte("package pkg00\n\nfunc Fn0() string { return \"folder_dirty_phase_marker\" }\n// changed\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		b.Fatal(err)
	}
	stateHash, selected, err := folderCorpusState(ctx, plan)
	if err != nil {
		b.Fatalf("folder state: %v", err)
	}
	changedDocs, changedPaths, _ := changedFolderDocumentsFromManifest(selected, manifest, indexWindowBytes)
	if len(changedDocs) != 1 || len(changedPaths) != 1 {
		b.Fatalf("changed docs=%d paths=%d, want 1 each", len(changedDocs), len(changedPaths))
	}
	repoName := folderRepoName(plan)

	b.Run("preLockFingerprint", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			gotState, selectedCount, err := folderCorpusFingerprint(ctx, plan)
			if err != nil {
				b.Fatalf("folder fingerprint: %v", err)
			}
			if gotState != stateHash || selectedCount != len(selected) {
				b.Fatal("unexpected folder fingerprint")
			}
		}
	})

	b.Run("postLockStateScan", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			gotState, gotSelected, err := folderCorpusState(ctx, plan)
			if err != nil {
				b.Fatalf("folder state: %v", err)
			}
			if gotState != stateHash || len(gotSelected) != len(selected) {
				b.Fatal("unexpected folder state")
			}
		}
	})

	b.Run("changedDocumentsFromManifest", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			docs, paths, _ := changedFolderDocumentsFromManifest(selected, manifest, indexWindowBytes)
			if len(docs) != 1 || len(paths) != 1 {
				b.Fatalf("changed docs=%d paths=%d, want 1 each", len(docs), len(paths))
			}
		}
	})

	b.Run("indexDeltaDocumentsWithBaseShard", func(b *testing.B) {
		baseDir := b.TempDir()
		templatePaths := benchmarkIndexFilePaths(b, plan.indexDir, repoName)
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			indexDir := filepath.Join(baseDir, strconv.Itoa(i))
			b.StopTimer()
			if err := os.Mkdir(indexDir, 0o755); err != nil {
				b.Fatalf("create index dir: %v", err)
			}
			copyBenchmarkIndexFiles(b, templatePaths, indexDir)
			b.StartTimer()
			if _, err := indexDeltaDocuments(indexDir, repoName, plan.root, changedDocs, indexParallelism(), changedPaths); err != nil {
				b.Fatalf("index delta documents with base shard: %v", err)
			}
			b.StopTimer()
			_ = os.RemoveAll(indexDir)
			b.StartTimer()
		}
	})
}

// BenchmarkFolderCorpus_OneFileZoektShardLowerBound times the least work of a
// dirty refresh that writes one new shard. It builds a one-file Zoekt shard
// with Seek's index options (indexDocuments), then searches it as a node read
// does (searchPlannedCorpusParsed). BenchmarkFolderCorpus_DirtyReindex_1File
// times the real refresh.
func BenchmarkFolderCorpus_OneFileZoektShardLowerBound(b *testing.B) {
	requireTools(b)
	ctx := context.Background()
	baseDir := b.TempDir()
	repoName := "folder_bucket_lower_bound"
	source := baseDir
	userQ, err := parseSearchQuery("folder_bucket_lower_bound_marker")
	if err != nil {
		b.Fatalf("parse query: %v", err)
	}
	content := []byte("package pkg00\n\nfunc Fn0() string { return \"folder_bucket_lower_bound_marker\" }\n")

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		indexDir := filepath.Join(baseDir, strconv.Itoa(i))
		if err := os.Mkdir(indexDir, 0o755); err != nil {
			b.Fatalf("create index dir: %v", err)
		}
		fileCh := make(chan fileContent, 1)
		fileCh <- fileContent{name: "pkg00/file_000.go", content: content}
		close(fileCh)
		if indexedAny, err := indexDocuments(ctx, indexDir, repoName, source, fileCh, 1); err != nil {
			b.Fatalf("index one-file shard: %v", err)
		} else if !indexedAny {
			b.Fatal("expected one-file shard to be indexed")
		}
		plan := node{cacheDir: baseDir, indexDir: indexDir}
		results, err := searchPlannedCorpusParsed(ctx, plan, nil, userQ, defaultSearchConfig())
		if err != nil {
			b.Fatalf("search one-file shard: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected one-file shard search result")
		}
		b.StopTimer()
		_ = os.RemoveAll(indexDir)
		b.StartTimer()
	}
}

// benchmarkIndexFilePaths returns the shard files of repoName in indexDir,
// with the sidecar files of each (index.IndexFilePaths).
func benchmarkIndexFilePaths(b *testing.B, indexDir, repoName string) []string {
	b.Helper()
	var paths []string
	for _, shard := range repositoryShardFiles(indexDir, repoName) {
		shardPaths, err := index.IndexFilePaths(shard)
		if err != nil {
			b.Fatalf("index file paths for %s: %v", shard, err)
		}
		paths = append(paths, shardPaths...)
	}
	if len(paths) == 0 {
		b.Fatal("expected benchmark template index files")
	}
	return paths
}

func copyBenchmarkIndexFiles(b *testing.B, paths []string, dstDir string) {
	b.Helper()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			b.Fatalf("read benchmark index file %s: %v", path, err)
		}
		dst := filepath.Join(dstDir, filepath.Base(path))
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			b.Fatalf("write benchmark index file %s: %v", dst, err)
		}
	}
}

// writeBenchmarkFolder writes files Go files in ten folders, pkg00 to pkg09,
// below a new temporary root, and returns the root. File i holds marker and
// the word marker_i, with i in three digits.
func writeBenchmarkFolder(b *testing.B, files int, marker string) string {
	b.Helper()
	root := b.TempDir()
	for i := range files {
		dir := filepath.Join(root, fmt.Sprintf("pkg%02d", i%10))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		name := filepath.Join(dir, fmt.Sprintf("file_%03d.go", i))
		content := fmt.Sprintf("package pkg%02d\n\nfunc Fn%d() string { return %q }\n// %s_%03d\n", i%10, i, marker, marker, i)
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return root
}

func benchmarkFileMatch(name string, score float64) zoekt.FileMatch {
	return zoekt.FileMatch{
		FileName:   name,
		Repository: "bench",
		Language:   "Go",
		Score:      score,
		LineMatches: []zoekt.LineMatch{
			{Line: []byte("func Bench() {}\n"), LineNumber: 1},
		},
	}
}

// benchmarkRealisticFiles builds n files with more realistic content than the
// fixtures above. Each file has a tab-indented symbol match with a multibyte
// comment in its context. It also has a long minified line whose match is deep
// enough to start the window. A benchmark of these files times the sanitize,
// window, symbol and (with ansiPalette) color paths together.
func benchmarkRealisticFiles(n int) []zoekt.FileMatch {
	long := "const blob = \"" + strings.Repeat("x", 3000) + "needle" + strings.Repeat("y", 1500) + "\"\n"
	files := make([]zoekt.FileMatch, n)
	for i := range n {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("pkg/service%d/handler.go", i), Repository: "bench",
			Language: "Go", Score: float64(n - i),
			LineMatches: []zoekt.LineMatch{
				{
					Line:       []byte("\tfunc (s *Service) HandleRequest(ctx context.Context) error {\n"),
					LineNumber: 42,
					Before:     []byte("\t// HandleRequest validates the café ☕ then dispatches.\n"),
					After:      []byte("\t\treturn s.dispatch(ctx)\n"),
					LineFragments: []zoekt.LineFragmentMatch{
						// "HandleRequest" begins at byte 19 of
						// "\tfunc (s *Service) ".
						{LineOffset: 19, MatchLength: 13, SymbolInfo: &zoekt.Symbol{Kind: "method"}},
					},
				},
				{
					Line:          []byte(long),
					LineNumber:    200,
					LineFragments: []zoekt.LineFragmentMatch{{LineOffset: 3014, MatchLength: 6}},
				},
			},
		}
	}
	return files
}

// BenchmarkCorpusFormatting_Palettes times both the plain and the colored
// output paths. The other benchmarks use only plainPalette, so they do not time
// the color codes and the containsVisible scan. It reports out_B, the size of
// the output in bytes. The formatter keeps the output small for the agent that
// reads it, so out_B shows each change in the bytes that it writes.
func BenchmarkCorpusFormatting_Palettes(b *testing.B) {
	results := benchmarkGitResults(benchmarkRealisticFiles(20))
	for _, pc := range []struct {
		name string
		pal  palette
	}{
		{"plain", plainPalette},
		{"ansi", ansiPalette},
	} {
		b.Run(pc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchmarkStringSink = formatCorpusResultsWithContext(results, 0, 0, hideCorpusContext, pc.pal)
			}
			b.ReportMetric(float64(len(benchmarkStringSink)), "out_B")
		})
	}
}

// --- Large-repo benchmarks ---
// Set SEEK_BENCH_REPO to the path of a Git repository, for example a
// Kubernetes checkout, to run them. They time the index build and the search
// on a large code base, where each cost shows. They do not write to
// SEEK_BENCH_REPO: a benchmark that writes uses a scratch clone
// (cloneBenchRepoAt).
//
//   git clone --depth=20 https://github.com/kubernetes/kubernetes /tmp/k8s
//   SEEK_BENCH_REPO=/tmp/k8s go test ./cmd/seek/ -run='^$' \
//     -bench=BenchmarkLargeRepo -benchmem -count=3

// requireBenchRepo returns SEEK_BENCH_REPO. With no value, it skips the
// benchmark: only a run that names a checkout runs the large-repo benchmarks.
func requireBenchRepo(b *testing.B) string {
	b.Helper()
	dir := os.Getenv("SEEK_BENCH_REPO")
	if dir == "" {
		b.Skip("SEEK_BENCH_REPO not set — skipping large-repo benchmark")
	}
	requireTools(b)
	return dir
}

// setupLargeRepoBench builds the index of SEEK_BENCH_REPO and returns the
// repository, its Git paths and its node.
func setupLargeRepoBench(b *testing.B) (repoDir string, paths gitPaths, plan node) {
	b.Helper()
	repoDir = requireBenchRepo(b)
	paths, plan = planGitTestCorpus(b, repoDir)

	ctx := context.Background()
	if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
		b.Fatalf("initial indexing failed: %v", err)
	}
	return repoDir, paths, plan
}

// setupScratchLargeRepoBench is setupLargeRepoBench on a scratch clone of
// SEEK_BENCH_REPO, for a benchmark that edits files.
func setupScratchLargeRepoBench(b *testing.B) (repoDir string, paths gitPaths, plan node) {
	b.Helper()
	repoDir = cloneBenchRepoAt(b, requireBenchRepo(b), "HEAD")
	paths, plan = planGitTestCorpus(b, repoDir)

	ctx := context.Background()
	if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
		b.Fatalf("initial indexing failed: %v", err)
	}
	return repoDir, paths, plan
}

// cloneBenchRepoAt clones sourceRepo at ref into a temporary folder, so a
// benchmark can move HEAD or edit files with no change to SEEK_BENCH_REPO.
func cloneBenchRepoAt(b *testing.B, sourceRepo, ref string) string {
	b.Helper()
	sourceAbs, err := filepath.Abs(sourceRepo)
	if err != nil {
		b.Fatalf("resolve SEEK_BENCH_REPO: %v", err)
	}
	want := gitOutputIn(b, sourceAbs, "rev-parse", ref)
	repoDir := filepath.Join(b.TempDir(), "repo")
	gitRunIn(b, filepath.Dir(repoDir), "clone", "--no-checkout", sourceAbs, repoDir)
	// A local clone can copy core.fsmonitor=true while its daemon socket still
	// points at the source worktree. The clone turns it off, so the benchmark
	// times Seek and not a failed fsmonitor request.
	gitRunIn(b, repoDir, "config", "core.fsmonitor", "false")
	gitRunIn(b, repoDir, "checkout", "-B", "seek-bench", want)
	if got := gitOutputIn(b, repoDir, "rev-parse", "HEAD"); got != want {
		b.Fatalf("bench clone HEAD mismatch: source=%s clone=%s", want, got)
	}
	return repoDir
}

func BenchmarkLargeRepo_ColdIndex(b *testing.B) {
	repoDir := requireBenchRepo(b)
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		paths, plan := planGitTestCorpus(b, repoDir)
		if err := os.RemoveAll(plan.cacheDir); err != nil {
			b.Fatal(err)
		}
		if err := os.MkdirAll(plan.cacheDir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()

		if results, err := runSeekInPlannedGitCorpus(ctx, "type:file", paths, plan); err != nil {
			b.Fatalf("cold large-repo search: %v", err)
		} else if len(results) == 0 {
			b.Fatal("expected cold large-repo result")
		}
	}
}

func BenchmarkLargeRepo_WarmSearch(b *testing.B) {
	_, paths, plan := setupLargeRepoBench(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if results, err := runSeekInPlannedGitCorpus(ctx, "type:file", paths, plan); err != nil {
			b.Fatalf("warm large-repo search: %v", err)
		} else if len(results) == 0 {
			b.Fatal("expected warm large-repo result")
		}
	}
}

func BenchmarkLargeRepo_DirtyReindex_1File(b *testing.B) {
	benchmarkLargeRepoDirtyN(b, 1)
}

func BenchmarkLargeRepo_DirtyReindex_10Files(b *testing.B) {
	benchmarkLargeRepoDirtyN(b, 10)
}

func BenchmarkLargeRepo_DirtyReindex_50Files(b *testing.B) {
	benchmarkLargeRepoDirtyN(b, 50)
}

func benchmarkLargeRepoDirtyN(b *testing.B, n int) {
	b.Helper()
	repoDir, paths, plan := setupScratchLargeRepoBench(b)
	ctx := context.Background()

	targets := findSourceFiles(b, repoDir, n)
	originals := make([][]byte, len(targets))
	for i, t := range targets {
		data, err := os.ReadFile(t)
		if err != nil {
			b.Fatal(err)
		}
		originals[i] = data
	}

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		marker := fmt.Sprintf("large_repo_dirty_bench_%d", i)
		for j, t := range targets {
			content := fmt.Appendf(originals[j][:len(originals[j]):len(originals[j])], "\n// %s\n", marker)
			if err := os.WriteFile(t, content, 0o644); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()
		if results, err := runSeekInPlannedGitCorpus(ctx, marker, paths, plan); err != nil {
			b.Fatalf("dirty large-repo search: %v", err)
		} else {
			assertBenchmarkResultsContainPaths(b, repoDir, targets, results)
		}
	}
}

// BenchmarkLargeRepo_Phases breaks down the dirty-reindex path and the search
// that follows it into individual phases, to show the time of each phase.
func BenchmarkLargeRepo_Phases(b *testing.B) {
	repoDir, paths, plan := setupScratchLargeRepoBench(b)
	ctx := context.Background()

	target := findSourceFile(b, repoDir)
	original, err := os.ReadFile(target)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("gitRepoStateIn", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			mustGitRepoStateIn(b, ctx, repoDir)
		}
	})

	b.Run("stateHash", func(b *testing.B) {
		state := mustGitRepoStateIn(b, ctx, repoDir)
		b.ReportAllocs()
		for b.Loop() {
			gitCorpusStateHash(paths, state)
		}
	})

	b.Run("checkCtagsCached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = checkCtagsCached()
		}
	})

	b.Run("planGitCorpus", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = planGitCorpus(paths, nil)
		}
	})

	b.Run("refreshCommitted_cached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := benchmarkRefreshGit(ctx, paths, plan); err != nil {
				b.Fatalf("index committed: %v", err)
			}
		}
	})

	b.Run("indexUncommitted_1file", func(b *testing.B) {
		// Dirty the file once and check that git status lists it. Each round
		// edits the file, runs git status, and builds the uncommitted family
		// (indexUncommittedForBench).
		if err := os.WriteFile(target, append(original, []byte("\n// dirty\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		state := mustGitRepoStateIn(b, ctx, repoDir)
		if len(state.Files) == 0 {
			b.Fatal("expected dirty files")
		}
		cachedState := readStateFile(plan.cacheDir)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			content := fmt.Appendf(original[:len(original):len(original)], "\n// p_%d\n", i)
			if err := os.WriteFile(target, content, 0o644); err != nil {
				b.Fatal(err)
			}
			loopState := mustGitRepoStateIn(b, ctx, repoDir)
			preState := gitCorpusStateHash(paths, loopState)
			if err := indexUncommittedForBench(ctx, repoDir, plan.corpusStore, loopState, cachedState, preState); err != nil {
				b.Fatalf("index uncommitted: %v", err)
			}
			// A build records the state it published: the next round reads
			// a delta of the one changed file, as a real refresh does.
			cachedState = preState
		}
	})

	b.Run("postVerify_gitStatus", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			postState := mustGitRepoStateIn(b, ctx, repoDir)
			gitCorpusStateHash(paths, postState)
		}
	})

	b.Run("postVerify_restat", func(b *testing.B) {
		// A check with no git status: gitCorpusStateHash reads the same
		// state, and repoStateFingerprint calls Lstat on each dirty file
		// again.
		if err := os.WriteFile(target, append(original, []byte("\n// dirty_for_restat\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		dirtyState := mustGitRepoStateIn(b, ctx, repoDir)
		if len(dirtyState.Files) == 0 {
			b.Fatal("expected dirty files")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			gitCorpusStateHash(paths, dirtyState)
		}
	})

	// The name stays for the CodSpeed history: it times the node read
	// (searchPlannedCorpusParsed).
	b.Run("executeParsedSearchScoped", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := searchPlannedCorpusForTest(ctx, plan, "func main"); err != nil {
				b.Fatalf("execute search: %v", err)
			}
		}
	})

	// --- The phases after the index build ---

	b.Run("loadShards", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			searchers, err := loadShards(plan.indexDir)
			if err != nil {
				b.Fatal(err)
			}
			for _, s := range searchers {
				s.Close()
			}
		}
	})

	b.Run("parseQuery", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := parseSearchQuery("func main"); err != nil {
				b.Fatal(err)
			}
		}
	})

	// It times the read lock that each node search takes and releases: a
	// node read that loads no shard.
	b.Run("acquireSearchLock_uncontended", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			read, _, err := openNodeRead(ctx, plan, nil)
			if err != nil {
				b.Fatal(err)
			}
			read.close()
		}
	})

	b.Run("dirtyFileSetFromState", func(b *testing.B) {
		if err := os.WriteFile(target, append(original, []byte("\n// dirty_set\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		dirtyState := mustGitRepoStateIn(b, ctx, repoDir)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_ = dirtyFileSetFromState(dirtyState)
		}
	})

	b.Run("formatCorpusResultsWithContext", func(b *testing.B) {
		results, err := searchPlannedCorpusForTest(ctx, plan, "func main")
		if err != nil {
			b.Fatal(err)
		}
		if len(results) == 0 {
			b.Fatal("the search found no results to format")
		}
		corpusResults := benchmarkGitResults(results)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			formatCorpusResultsWithContext(corpusResults, 0, 0, hideCorpusContext, plainPalette)
		}
	})

}

// findSourceFile returns the absolute path of a source file suitable for
// editing.
func findSourceFile(b *testing.B, repoDir string) string {
	b.Helper()
	targets := findSourceFiles(b, repoDir, 1)
	return targets[0]
}

// findSourceFiles returns absolute paths of n source files suitable for
// editing.
func findSourceFiles(b *testing.B, repoDir string, n int) []string {
	b.Helper()
	var result []string
	err := filepath.WalkDir(repoDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			return err
		}
		if len(result) >= n {
			return filepath.SkipAll
		}
		if !isBenchmarkSourceFile(d) {
			return nil
		}
		result = append(result, path)
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	if len(result) < n {
		b.Fatalf("SEEK_BENCH_REPO has fewer than %d source files", n)
	}
	return result
}

// isBenchmarkSourceFile reports a regular, non-empty file whose content fits
// one document (documentContentFits) and whose extension names a source
// language.
func isBenchmarkSourceFile(d os.DirEntry) bool {
	if d.Type()&os.ModeSymlink != 0 {
		return false
	}
	info, err := d.Info()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || !documentContentFits(info.Size()) {
		return false
	}
	switch strings.ToLower(filepath.Ext(d.Name())) {
	case ".c", ".cc", ".cpp", ".go", ".h", ".hpp", ".java", ".js", ".jsx", ".kt", ".py", ".rs", ".ts", ".tsx":
		return true
	default:
		return false
	}
}

func assertBenchmarkResultsContainPaths(b *testing.B, repoDir string, targets []string, results []string) {
	b.Helper()
	if len(results) == 0 {
		b.Fatal("expected dirty large-repo result")
	}
	got := make(map[string]struct{}, len(results))
	for _, result := range results {
		got[filepath.ToSlash(result)] = struct{}{}
	}
	for _, target := range targets {
		rel, err := filepath.Rel(repoDir, target)
		if err != nil {
			b.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		if _, ok := got[rel]; !ok {
			b.Fatalf("expected dirty search result for edited path %q, got %v", rel, got)
		}
	}
}

// --- Delta-indexing benchmarks ---

// BenchmarkGitCommitted_1CommitAhead times the index update for one new
// commit. Each round makes the commit outside the timer, then runs the node
// refresh that a search runs (benchmarkRefreshGit). The refresh uses a native
// delta when the new commit is eligible (prepareNativeGitDelta).
func BenchmarkGitCommitted_1CommitAhead(b *testing.B) {
	requireTools(b)

	dir := initGitRepo(b, "seed.go", "package main\n// commit_ahead_seed\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, dir)
	if err := benchmarkRefreshGit(ctx, paths, plan); err != nil {
		b.Fatalf("initial commit index: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		name := fmt.Sprintf("step%d.go", i)
		body := fmt.Appendf(nil, "package main\n// commit_ahead_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			b.Fatal(err)
		}
		gitRunIn(b, dir, "add", ".")
		gitRunIn(b, dir, "commit", "-m", "delta step")
		b.StartTimer()
		if err := benchmarkRefreshGit(ctx, paths, plan); err != nil {
			b.Fatalf("delta commit index: %v", err)
		}
	}
}

// BenchmarkGitUncommitted_RapidEdits_N16 saves one file 16 times in each
// round, as an editor does, and times git status and the build of each save.
// Each save after the first writes one small delta shard and tombstones the
// earlier copy of the file. When the family holds more than
// maxUncommittedDeltaShards shards, the next save rebuilds it in full, so the
// shard count stays bounded.
func BenchmarkGitUncommitted_RapidEdits_N16(b *testing.B) {
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n// rapid_edits_baseline\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, dir)
	// One build with no dirty file publishes the first state.
	state := mustGitRepoStateIn(b, ctx, dir)
	preState := gitCorpusStateHash(paths, state)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, preState); err != nil {
		b.Fatalf("baseline index: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		for j := range 16 {
			body := fmt.Appendf(nil, "package main\n// rapid_edits_%d_%d\n", i, j)
			if err := os.WriteFile(filepath.Join(dir, "app.go"), body, 0o644); err != nil {
				b.Fatal(err)
			}
			state := mustGitRepoStateIn(b, ctx, dir)
			preState := gitCorpusStateHash(paths, state)
			if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, preState); err != nil {
				b.Fatalf("delta cycle %d.%d: %v", i, j, err)
			}
		}
	}
}

// BenchmarkGitBranch_Switch_Unrelated measures the worst case: each round
// switches between two branches that differ in 16 of the 21 indexed files.
// Delta builds add tombstones until the delta shard count passes
// maxCommittedDeltaShards; the next update then does a full build. The time
// per round includes the cost of the delta builds and of the full builds.
func BenchmarkGitBranch_Switch_Unrelated(b *testing.B) {
	requireTools(b)

	dir := initGitRepo(b, "main.go", "package main\n// branch_switch_main\n")
	for i := range 20 {
		name := fmt.Sprintf("file%d.go", i)
		body := fmt.Appendf(nil, "package main\n// main_marker_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	gitRunIn(b, dir, "add", ".")
	gitRunIn(b, dir, "commit", "-m", "main fleet")
	mainBranch := gitOutputIn(b, dir, "symbolic-ref", "--short", "HEAD")

	// The feature branch changes 16 of the 20 numbered files.
	gitRunIn(b, dir, "checkout", "-b", "feature")
	for i := range 16 {
		name := fmt.Sprintf("file%d.go", i)
		body := fmt.Appendf(nil, "package main\n// feature_marker_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	gitRunIn(b, dir, "commit", "-am", "feature churn")

	paths, plan := planGitTestCorpus(b, dir)
	ctx := context.Background()
	if err := benchmarkRefreshGit(ctx, paths, plan); err != nil {
		b.Fatalf("initial index: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		target := mainBranch
		if i%2 == 1 {
			target = "feature"
		}
		gitRunIn(b, dir, "checkout", target)
		if err := benchmarkRefreshGit(ctx, paths, plan); err != nil {
			b.Fatalf("post-switch index: %v", err)
		}
	}
}

// BenchmarkSearchTombstoneCost times a search of an uncommitted family after
// 16 edits of one file. Only the newest copy is live, and each older shard
// holds a tombstoned copy. Each search must find the live marker. The time
// and the allocations show a change in Zoekt's tombstone filter
// (index/eval.go) on CodSpeed.
func BenchmarkSearchTombstoneCost(b *testing.B) {
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n// tombstone_cost_baseline\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, dir)
	state := mustGitRepoStateIn(b, ctx, dir)
	preState := gitCorpusStateHash(paths, state)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, preState); err != nil {
		b.Fatalf("baseline index: %v", err)
	}

	// 16 edits of one file: one full build, then 15 deltas. The newest shard
	// holds the live content; the older shards hold only tombstoned copies.
	for i := range 16 {
		body := fmt.Appendf(nil, "package main\n// tombstone_cost_iter_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, "app.go"), body, 0o644); err != nil {
			b.Fatal(err)
		}
		state := mustGitRepoStateIn(b, ctx, dir)
		preState := gitCorpusStateHash(paths, state)
		if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, preState); err != nil {
			b.Fatalf("delta cycle %d: %v", i, err)
		}
	}

	target := "tombstone_cost_iter_15"
	if results, err := searchPlannedCorpusForTest(ctx, plan, target); err != nil {
		b.Fatalf("warm-up search: %v", err)
	} else if len(results) == 0 {
		b.Fatal("expected live tombstone-cost marker to be findable")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		results, err := searchPlannedCorpusForTest(ctx, plan, target)
		if err != nil {
			b.Fatalf("search: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("search lost live marker mid-bench")
		}
	}
}

// --- GC benchmarks ---
//
// They time the GC work of a command:
//   - the warm path after each search: touchUsed, and runOpportunisticGC when
//     the throttle stops it;
//   - the eviction path: enumerateCorpusDirs, and full runGC passes;
//   - the dry-run table: reportGCPlan with one real corpus and seeded ones.
//
// On CodSpeed they show the loss of these fast paths:
//   - the throttle gate runs before MkdirAll and isOnNFS, so the warm path
//     makes one system call, not three;
//   - touchStamp and touchUsed try Chtimes first;
//   - pickDisplayShard matches a name prefix (isUncommittedShard), not a
//     substring.

func BenchmarkGC_TouchUsed_WarmPath(b *testing.B) {
	resetNFSCheck(b)
	root := cacheRootForTest(b)
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(0))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	// The first call makes .used and runs the NFS check once.
	touchUsed(dir)

	b.ReportAllocs()
	for b.Loop() {
		touchUsed(dir)
	}
}

func BenchmarkGC_TouchUsed_ColdCreate(b *testing.B) {
	resetNFSCheck(b)
	root := cacheRootForTest(b)
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(0))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	usedPath := filepath.Join(dir, usedFile)
	// The first call runs the NFS check once, so the loop makes no Statfs.
	touchUsed(dir)

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		_ = os.Remove(usedPath)
		b.StartTimer()
		touchUsed(dir)
	}
}

func BenchmarkGC_RunOpportunisticGC_Throttled(b *testing.B) {
	resetNFSCheck(b)
	root := cacheRootForTest(b)
	// A new .last-gc stamp: the throttle gate returns after one stat.
	if err := os.WriteFile(filepath.Join(root, gcStampFile), nil, 0o644); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		runOpportunisticGC(ctx)
	}
}

func BenchmarkGC_Enumerate_N100(b *testing.B) {
	root := cacheRootForTest(b)
	now := time.Now()
	for i := 0; i < 100; i++ {
		seedCorpus(b, root, fakeCorpusHash(i), now)
	}
	corporaPath := filepath.Join(root, corporaDir)

	b.ReportAllocs()
	for b.Loop() {
		entries, err := enumerateCorpusDirs(corporaPath)
		if err != nil {
			b.Fatal(err)
		}
		if len(entries) != 100 {
			b.Fatalf("want 100 entries, got %d", len(entries))
		}
	}
}

func BenchmarkGC_RunGC_NoEvictions_N100(b *testing.B) {
	resetNFSCheck(b)
	root := cacheRootForTest(b)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		// Seed the corpora again in each round. If runGC evicted fresh
		// corpora by mistake, one setup would leave an empty cache after the
		// first round. The fast later rounds would then hide the defect in
		// the average. So each timed round lists and keeps the same 100
		// corpora.
		now := time.Now()
		for i := 0; i < 100; i++ {
			seedCorpus(b, root, fakeCorpusHash(i), now)
		}
		b.StartTimer()

		runGC(ctx, gcOptions{skipThrottle: true})
	}
}

func BenchmarkGC_RunGC_FullEvict_N100(b *testing.B) {
	resetNFSCheck(b)
	root := cacheRootForTest(b)
	// A 1-byte budget puts every corpus over the budget, so each pass evicts
	// all 100.
	b.Setenv(envCacheBudget, "1")
	stale := time.Now().Add(-30 * 24 * time.Hour)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		// The last pass evicted the 100 stale corpora, so seed them again.
		for i := 0; i < 100; i++ {
			seedCorpus(b, root, fakeCorpusHash(i), stale)
		}
		b.StartTimer()

		runGC(ctx, gcOptions{skipThrottle: true})
	}
}

// BenchmarkGC_RunGC_StreamingSorted_N100 times the path of
// `seek gc --force --sort=size`. It measures each row (corpusDirSize and the
// display information), sorts the rows (sortGCRows), and evicts in table
// order. The silent benchmarks above do not run these steps.
func BenchmarkGC_RunGC_StreamingSorted_N100(b *testing.B) {
	resetNFSCheck(b)
	root := cacheRootForTest(b)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		// Seed the 100 corpora again each iteration. The 1P cache budget
		// that setTestUserCache sets keeps them all, so the pass evicts
		// nothing.
		for i := 0; i < 100; i++ {
			seedCorpus(b, root, fakeCorpusHash(i), stale)
		}
		b.StartTimer()

		runGC(ctx, gcOptions{
			skipThrottle: true,
			writer:       io.Discard,
			sortKey:      sortBySize,
		})
	}
}

func BenchmarkGC_PickDisplayShard(b *testing.B) {
	cases := [][]string{
		{"/c/index/github.com%2Ffoo%2Fbar_v16.00000.zoekt"},
		{"/c/index/uncommitted_v16.00000.zoekt", "/c/index/github.com%2Ffoo%2Fbar_v16.00000.zoekt"},
		{"/c/index/uncommitted_v16.00000.zoekt", "/c/index/github.com%2Ffoo%2Funcommitted-tool_v16.00000.zoekt"},
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, c := range cases {
			_ = pickDisplayShard(c)
		}
	}
}

func BenchmarkGC_DryRun_Render_N20(b *testing.B) {
	requireTools(b)
	resetNFSCheck(b)

	// One real corpus gives readCorpusDisplayInfo shard metadata to read
	// (Zoekt's ReadMetadataPath) on one row. The other rows take the
	// [empty] branch.
	repo := initGitRepo(b, "app.go", "package main\n// bench_dry_run\n")
	ctx := context.Background()
	paths, realPlan := planGitTestCorpus(b, repo)
	if _, err := runSeekInPlannedGitCorpus(ctx, "bench_dry_run", paths, realPlan); err != nil {
		b.Fatalf("real corpus build: %v", err)
	}

	root, err := seekUserCacheRoot()
	if err != nil {
		b.Fatalf("seekUserCacheRoot: %v", err)
	}
	// 19 corpora with no shard beside the real one. Their rows show
	// [empty]; the benchmark times the rendering of the table.
	now := time.Now()
	for i := 0; i < 19; i++ {
		seedCorpus(b, root, fakeCorpusHash(i+1), now)
	}

	corporaPath := filepath.Join(root, corporaDir)
	entries, err := enumerateCorpusDirs(corporaPath)
	if err != nil {
		b.Fatal(err)
	}
	// A dry run records no size (runGCCommandCmd).
	evict, err := planCorpusEviction(root, entries, false, nil, false)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := reportGCPlan(ctx, io.Discard, root, entries, evict, sortByName); err != nil {
			b.Fatal(err)
		}
	}
}

// Indexer benchmarks beyond the read budget, and the folder manifest write.

// BenchmarkFolderCorpus_ColdIndex_LargeRotation sets a readSemaphore of 4 MiB
// and builds 12 MiB of content. Its ns/op shows the cost of a build past the
// read budget. Each 512 KiB file is a normal document, so one Builder takes all
// of them and no window rotates. The name stays for the CodSpeed history.
func BenchmarkFolderCorpus_ColdIndex_LargeRotation(b *testing.B) {
	requireTools(b)
	ctx := context.Background()

	testReadSemMu.Lock()
	defer testReadSemMu.Unlock()
	const testBudget int64 = 4 * 1024 * 1024
	restore := swapReadSemaphoreForTest(testBudget)
	defer restore()

	for b.Loop() {
		b.StopTimer()
		root := b.TempDir()
		const fileCount = 24
		const fileSize = 512 * 1024 // 12 MiB in all
		payload := make([]byte, fileSize-len("package x\n"))
		for i := range payload {
			payload[i] = byte('a' + i%26)
		}
		for i := 0; i < fileCount; i++ {
			name := filepath.Join(root, fmt.Sprintf("f%03d.go", i))
			if err := os.WriteFile(name, append([]byte("package x\n"), payload...), 0o644); err != nil {
				b.Fatal(err)
			}
		}
		plan := planFolderTestCorpus(b, root)
		b.StartTimer()
		if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
			b.Fatalf("ensureFolderCorpusFresh: %v", err)
		}
	}
}

// BenchmarkWriteFolderManifest_100k times writeFolderManifest with 100k
// entries, the largest realistic folder manifest. The encoder builds the whole
// JSON text in memory and writes it in one write. ReportAllocs shows a change
// in allocation.
func BenchmarkWriteFolderManifest_100k(b *testing.B) {
	const count = 100_000
	selected := make([]folderCandidate, count)
	for i := range selected {
		selected[i] = folderCandidate{
			name:  fmt.Sprintf("path/to/file_%06d.go", i),
			size:  int64(i % 4096),
			mtime: int64(i),
			dev:   1,
			ino:   uint64(i),
		}
	}
	b.ReportAllocs()
	dir := b.TempDir()
	b.ResetTimer()
	for b.Loop() {
		if err := writeFolderManifest(dir, "state-x", selected, 0, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// --- Detector benchmarks ---

// BenchmarkDetectGitBoundary_NotBoundary measures the most common case of the
// walker: detection on a directory that contains no `.git` entry. Most
// subdirectories use this path, so the benchmark reports allocation changes.
func BenchmarkDetectGitBoundary_NotBoundary(b *testing.B) {
	root := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, status := detectGitBoundary(root)
		if status != notBoundary {
			b.Fatalf("status=%v", status)
		}
	}
}

// BenchmarkDetectGitBoundary_DirForm times the confirmed path of a `.git`
// directory with HEAD, objects and refs (steps L1 and L2a of
// detectGitBoundary).
func BenchmarkDetectGitBoundary_DirForm(b *testing.B) {
	root := b.TempDir()
	writeGitTriadAt(b, filepath.Join(root, ".git"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, status := detectGitBoundary(root)
		if status != boundaryConfirmed {
			b.Fatalf("status=%v", status)
		}
	}
}

// BenchmarkDetectGitBoundary_WorktreeFile times the parse of a `.git` file
// that names a Git directory outside the worktree (the layout of
// git init --separate-git-dir). With no core.worktree and no linked-worktree
// back-reference, the detector returns ambiguous and Git decides
// (TestDetectGitBoundary_SeparateGitDirOutsideTreeAsksGit).
func BenchmarkDetectGitBoundary_WorktreeFile(b *testing.B) {
	root := b.TempDir()
	realGitDir := filepath.Join(root, "real-git")
	writeGitTriadAt(b, realGitDir)
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+realGitDir+"\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, status := detectGitBoundary(worktree)
		if status != ambiguous {
			b.Fatalf("status=%v, want ambiguous", status)
		}
	}
}

// BenchmarkDetectGitBoundary_LinkedWorktreeBackref times the confirmed path
// of a linked worktree: its `.git` file names an admin directory whose gitdir
// file names the worktree back.
func BenchmarkDetectGitBoundary_LinkedWorktreeBackref(b *testing.B) {
	root := b.TempDir()
	parentGit := filepath.Join(root, "parent.git")
	writeGitTriadAt(b, parentGit)

	worktree := filepath.Join(root, "feature-wt")
	wtGit := writeLinkedWorktreeAdmin(b, parentGit, worktree)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, status := detectGitBoundary(worktree)
		if status != boundaryConfirmed {
			b.Fatalf("status=%v", status)
		}
	}
}

// BenchmarkManyShardGit_WarmSearch times warm searches of one Git node with
// at least 8 shards through runSearchCommand, the entry point of a command.
// It times Seek's stages with a model that runs no inference
// (benchmarkNodeModel). Each case query gives the model text, so each case
// runs the model stages, also the cases named Strict. Each search loads every
// shard of the node, so the time of a case grows with the number of shard
// loads per node read. The body calls only runSearchCommand, so a change
// inside the node read does not change the benchmark.
func BenchmarkManyShardGit_WarmSearch(b *testing.B) {
	requireTools(b)
	setTestUserCache(b)
	swapShardMaxForTest(b, 128<<10)
	repo := initEmptyGitRepoNoRemote(b)
	writeBenchmarkNodeFiles(b, repo, "pkg", 1024)
	writeFileAt(b, repo, "pkg/f0517_marker.go", "package pkg\n\n// zqselectivebenchmarker\n")
	// The benchmark model puts these files near the query by their names.
	// They hold no query word, so a case that finds them also runs the
	// relaxed query. Their names match the file:pkg/f00 filter of the last
	// case.
	for i := range 64 {
		writeFileAt(b, repo, fmt.Sprintf("pkg/f00s%02d.go", i), fmt.Sprintf("package pkg\n\n// vector-only file %d\n", i))
	}
	gitRunIn(b, repo, "add", ".")
	gitRunIn(b, repo, "commit", "-q", "-m", "fixture")
	operands := []string{repo}
	// The Description case must find files by vector, so it runs the
	// relaxed query.
	if out := captureStdoutGC(b, func() { mustRunBenchmarkSearch(b, "parse handler", operands) }); !strings.Contains(out, "f00s") {
		b.Fatal("the description case found no vector-only file, so the relaxed query does not run")
	}
	discardStdoutForBenchmark(b)
	cases := []struct{ name, query string }{
		{"SelectiveStrict", "zqselectivebenchmarker"},
		{"CommonStrict", "handler"},
		{"Description", "parse handler"},
	}
	for _, c := range cases {
		mustRunBenchmarkSearch(b, c.query, operands)
	}
	plans, err := planCorpora(b.Context(), operands)
	if err != nil || len(plans) != 1 {
		b.Fatalf("plans=%d err=%v", len(plans), err)
	}
	if shards, _ := filepath.Glob(filepath.Join(plans[0].indexDir, "*.zoekt")); len(shards) < 8 {
		b.Fatalf("the fixture has %d shards, want 8 or more", len(shards))
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				mustRunBenchmarkSearch(b, c.query, operands)
			}
		})
	}
	// The last case reads the dirty overlay and the file: mask too.
	writeFileAt(b, repo, "pkg/f0001.go", "package pkg\n\n// handler parses the dirty request\n")
	const dirtyQuery = "file:pkg/f00 parse handler"
	mustRunBenchmarkSearch(b, dirtyQuery, operands)
	b.Run("DescriptionFileFilterDirtyGit", func(b *testing.B) {
		for b.Loop() {
			mustRunBenchmarkSearch(b, dirtyQuery, operands)
		}
	})
}

// BenchmarkMultiNode_WarmSearch times warm searches with no path operand in a
// folder that holds two Git repositories. The search has three nodes, the
// folder and the two repositories, and searchNodes reads them at the same
// time. It times Seek's stages with a model that runs no inference
// (benchmarkNodeModel). Both case queries give the model text, so both cases
// run the model stages.
func BenchmarkMultiNode_WarmSearch(b *testing.B) {
	requireTools(b)
	setTestUserCache(b)
	root := b.TempDir()
	writeBenchmarkNodeFiles(b, root, "docs", 200)
	for _, name := range []string{"repo-a", "repo-b"} {
		repo := initEmptyGitRepoNoRemoteAt(b, filepath.Join(root, name))
		writeBenchmarkNodeFiles(b, repo, "pkg", 200)
		gitRunIn(b, repo, "add", ".")
		gitRunIn(b, repo, "commit", "-q", "-m", "fixture")
	}
	b.Chdir(root)
	discardStdoutForBenchmark(b)
	if plans, err := planCorpora(b.Context(), []string{"."}); err != nil || len(plans) != 3 {
		b.Fatalf("plans=%d err=%v, want 3 nodes", len(plans), err)
	}
	for _, c := range []struct{ name, query string }{
		{"Strict", "handler"},
		{"Description", "parse handler"},
	} {
		mustRunBenchmarkSearch(b, c.query, nil)
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				mustRunBenchmarkSearch(b, c.query, nil)
			}
		})
	}
}

// writeBenchmarkNodeFiles writes count Go files of about 2 KiB below
// dir/sub. Each file holds the words "handler" and "parses".
func writeBenchmarkNodeFiles(b *testing.B, dir, sub string, count int) {
	b.Helper()
	for i := range count {
		writeFileAt(b, dir, fmt.Sprintf("%s/f%04d.go", sub, i), fmt.Sprintf(
			"package pkg\n\n// handler%04d parses the request of case %d.\nfunc handler%04d() {}\n%s",
			i, i, i, strings.Repeat(fmt.Sprintf("// note %d of the benchmark fixture text\n", i), 48),
		))
	}
}

// mustRunBenchmarkSearch runs one search command as the CLI does, with the
// benchmark model (benchmarkNodeModel) for the model stages.
func mustRunBenchmarkSearch(b *testing.B, query string, operands []string) {
	b.Helper()
	runConfig := searchRunConfig{policy: defaultSearchPolicy(), newModel: benchmarkNodeModel}
	if err := runSearchCommand(b.Context(), query, operands, 0, 0, defaultSearchConfig(), runConfig); err != nil {
		b.Fatalf("search %q: %v", query, err)
	}
}

// benchmarkNodeModel is the test model with one vector per file, so USearch's
// graph has distinct points: equal vectors give a degenerate graph that finds
// nothing. It runs no inference, so a benchmark with it times Seek's stages;
// BenchmarkCLIProcess times a warm description search with the bundled model.
// The files whose path starts with pkg/f00s lie near the query, and the
// others lie far from it. The model accepts every document that it scores.
func benchmarkNodeModel(context.Context) (semanticModel, error) {
	return &hybridTestScorer{mode: payloadAccept, vectorForUnit: func(unit semanticUnit) semanticVector {
		var vector semanticVector
		if strings.HasPrefix(unit.path, "pkg/f00s") {
			vector[0] = 1
		} else {
			vector[1] = 1
		}
		hash := fnv.New32a()
		_, _ = hash.Write([]byte(unit.path))
		vector[2+int(hash.Sum32()%uint32(semanticEmbeddingDimensions-2))] = 0.05
		_ = normalizeSemanticVector(&vector)
		return vector
	}}, nil
}

// discardStdoutForBenchmark sends the results that runSearchCommand writes to
// os.Stdout to the null device until the benchmark ends. The testing package
// keeps its own copy of the standard output, so its result lines stay.
func discardStdoutForBenchmark(b *testing.B) {
	b.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devNull
	b.Cleanup(func() {
		os.Stdout = stdout
		_ = devNull.Close()
	})
}

// indexUncommittedForBench runs the uncommitted part of a Git build as
// gitNode.buildLocked does: a delta when the live family takes one, else a
// full stage. It takes no build lock or publish lock, so it times that part
// alone.
func indexUncommittedForBench(ctx context.Context, repoDir string, store corpusStore, state repoState, cachedState, preState string) error {
	// The bench does not write the family manifest (.family-v1) between
	// rounds, and no other process changes the family, so it passes
	// familyIntact as true.
	delta, stage, prebuildErr, err := prepareUncommitted(ctx, repoDir, store, cachedState, true, state, indexParallelism())
	if err := errors.Join(prebuildErr, err); err != nil {
		return err
	}
	if stage != nil {
		defer stage.discard()
	}
	return indexUncommitted(ctx, repoDir, store, state, preState, indexParallelism(), stage, delta)
}
