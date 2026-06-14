package main

import (
	"context"
	"fmt"
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

var benchmarkStringSink string

// --- Hot-path microbenchmarks ---
// These cover every function called on each search invocation.

func BenchmarkComputeStateHash_Small(b *testing.B) {
	// Typical clean repo: headers only (~80 bytes)
	input := "# branch.oid abc123def456789012345678901234567890\x00# branch.head main\x00"
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		computeStateHash(input)
	}
}

func BenchmarkComputeStateHash_Dirty(b *testing.B) {
	// Repo with 50 dirty files — fingerprinted output (~5 KB)
	var sb strings.Builder
	sb.WriteString("# branch.oid abc123def456789012345678901234567890\x00# branch.head main\x00")
	for i := range 50 {
		fmt.Fprintf(&sb, "1 .M N... 100644 100644 100644 abc123 def456 src/pkg%d/file%d.go\x00", i, i)
	}
	// Simulate fingerprint appendix
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
	// All files are "deleted" (don't exist on disk) — exercises error path
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

func BenchmarkFastResolveGitPaths(b *testing.B) {
	dir := b.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755); err != nil {
		b.Fatal(err)
	}

	// Change to the temp dir so fastResolveGitPaths can find .git
	origDir, err := os.Getwd()
	if err != nil {
		b.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = os.Chdir(origDir) })

	b.ReportAllocs()
	for b.Loop() {
		fastResolveGitPaths()
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
		// Call resolveGitPaths directly to bypass the fast path
		// and measure pure subprocess cost.
		if _, err := resolveGitPaths(ctx, ""); err != nil {
			b.Fatal(err)
		}
	}
}

// --- Formatter benchmarks ---

const benchmarkFormatterCorpusID corpusID = "benchmark"

func benchmarkGitResults(files []zoekt.FileMatch) []corpusSearchResult {
	return wrapCorpusResults(corpusPlan{
		id:   benchmarkFormatterCorpusID,
		kind: corpusKindGit,
	}, files)
}

func benchmarkDirtyFilesByCorpus(dirtyFiles dirtyFileSet) dirtyFilesByCorpus {
	if dirtyFiles == nil {
		return nil
	}
	return dirtyFilesByCorpus{benchmarkFormatterCorpusID: dirtyFiles}
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
		formatCorpusResultsWithContext(results, nil, 0, 0, hideCorpusContext)
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
		formatCorpusResultsWithContext(results, nil, 0, 0, hideCorpusContext)
	}
}

func BenchmarkCorpusFormatting_100Files_WithDedup(b *testing.B) {
	files := dedupFixtureFiles(100, "file_%03d.go", "match\n", "updated match\n")
	results := benchmarkGitResults(files)
	b.ReportAllocs()
	for b.Loop() {
		formatCorpusResultsWithContext(results, nil, 0, 0, hideCorpusContext)
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
		formatCorpusResultsWithContext(results, nil, 0, 0, hideCorpusContext)
	}
}

// buildLargeFixture creates nFiles FileMatches, each with matchesPerFile
// LineMatches including Before/After context and symbol annotations.
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
				formatCorpusResultsWithContext(results, nil, limit, 0, hideCorpusContext)
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
				formatCorpusResultsWithContext(results, nil, 0, maxMatches, hideCorpusContext)
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
				formatCorpusResultsWithContext(results, nil, tc.limit, tc.maxMatches, hideCorpusContext)
			}
		})
	}
}

func BenchmarkDeduplicateCorpusResults_100(b *testing.B) {
	results := make([]corpusSearchResult, 200)
	for i := range 100 {
		results[i] = corpusSearchResult{
			corpusID: corpusID("repo"),
			kind:     corpusKindGit,
			file:     zoekt.FileMatch{FileName: fmt.Sprintf("f%d.go", i), Repository: "repo"},
		}
		results[100+i] = corpusSearchResult{
			corpusID: corpusID("repo"),
			kind:     corpusKindGit,
			file:     zoekt.FileMatch{FileName: fmt.Sprintf("f%d.go", i), Repository: repoUncommitted},
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		deduplicateCorpusResults(results, nil)
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
		for range streamFiles(dir, files, 4) {
		}
	}
}

// --- End-to-end benchmark (requires git + ctags) ---

func BenchmarkPlannedGitCorpus_ColdIndex(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
	scopedPlan, err := planCurrentGitCorpusWithOperands(paths, []string{filepath.Join(dir, "a")})
	if err != nil {
		b.Fatalf("plan scoped corpus: %v", err)
	}

	if results, err := runSeekInPlannedGitCorpus(ctx, "path_scoped_bench_marker", paths, scopedPlan); err != nil {
		b.Fatalf("scoped setup: %v", err)
	} else if len(results) == 0 {
		b.Fatal("expected scoped setup result")
	}

	b.ResetTimer()
	for b.Loop() {
		results, err := runSeekInPlannedGitCorpus(ctx, "path_scoped_bench_marker", paths, scopedPlan)
		if err != nil {
			b.Fatalf("search scoped corpus: %v", err)
		}
		if len(results) == 0 {
			b.Fatal("expected scoped benchmark result")
		}
	}
}

func BenchmarkPlannedGitCorpus_DirtyReindex(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
		// Simulate edit on each iteration
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

// BenchmarkSmallRepo_Phases breaks down the dirty-reindex path into
// individual phases on a tiny repo (no SEEK_BENCH_REPO needed).
func BenchmarkSmallRepo_Phases(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping in short mode")
	}
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n\nfunc main() {\n\t// phase_bench\n}\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, dir)

	if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
		b.Fatalf("initial indexing: %v", err)
	}

	// Dirty the file for uncommitted phases
	original := []byte("package main\n\nfunc main() {\n\t// phase_bench\n}\n")

	b.Run("gitRepoStateIn", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			gitRepoStateIn(ctx, dir)
		}
	})

	b.Run("stateHash_clean", func(b *testing.B) {
		state := gitRepoStateIn(ctx, dir)
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

	b.Run("planCurrentGitCorpus", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = planCurrentGitCorpus(paths)
		}
	})

	b.Run("ensureUntrackedCache", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ensureUntrackedCache(ctx, paths)
		}
	})

	b.Run("indexCommitted_incremental", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := indexCommitted(dir, plan.indexDir, indexParallelism()); err != nil {
				b.Fatalf("index committed: %v", err)
			}
		}
	})

	b.Run("indexUncommitted_1file", func(b *testing.B) {
		if err := os.WriteFile(filepath.Join(dir, "app.go"), append(original, []byte("\n// dirty\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		state := gitRepoStateIn(ctx, dir)
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
			// Reuse the captured state (paths only). statUncommittedCandidates
			// re-stats each file during indexUncommitted, so the delta path
			// still sees fresh mtime/size/ino for every iteration.
			if err := indexUncommitted(ctx, dir, plan.indexDir, plan.cacheDir, state, cachedState, preState, indexParallelism()); err != nil {
				b.Fatalf("index uncommitted: %v", err)
			}
		}
	})

	b.Run("postVerify_restat", func(b *testing.B) {
		if err := os.WriteFile(filepath.Join(dir, "app.go"), append(original, []byte("\n// restat\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		dirtyState := gitRepoStateIn(ctx, dir)
		if len(dirtyState.Files) == 0 {
			b.Fatal("expected dirty files")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			gitCorpusStateHash(paths, dirtyState)
		}
	})

	b.Run("executeParsedSearchScoped", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := executeUnscopedShardSearchForTest(ctx, plan.indexDir, "phase_bench"); err != nil {
				b.Fatalf("execute search: %v", err)
			}
		}
	})
}

func BenchmarkFolderCorpus_ColdIndex(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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

func BenchmarkMultiCorpus_WarmSearchAndFormat(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
	requireTools(b)
	ctx := context.Background()
	rootA := writeBenchmarkFolder(b, 100, "multi_corpus_marker")
	rootB := writeBenchmarkFolder(b, 100, "multi_corpus_marker")
	planA := planFolderTestCorpus(b, rootA)
	planB := planFolderTestCorpus(b, rootB)
	for _, plan := range []corpusPlan{planA, planB} {
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
		resultsA, _, err := prepareAndSearchCorpus(ctx, planA, nil, userQ)
		if err != nil {
			b.Fatalf("search corpus A: %v", err)
		}
		resultsB, _, err := prepareAndSearchCorpus(ctx, planB, nil, userQ)
		if err != nil {
			b.Fatalf("search corpus B: %v", err)
		}
		results := append(resultsA, resultsB...)
		benchmarkStringSink = formatCorpusResultsWithContext(results, nil, 0, 0, showCorpusContext)
	}
}

func BenchmarkFormatCorpusResults_Dedupe(b *testing.B) {
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

	b.ReportAllocs()
	for b.Loop() {
		benchmarkStringSink = formatCorpusResultsWithContext(results, nil, 0, 0, showCorpusContext)
	}
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
	if err := writeFolderManifest(plan.cacheDir, "old-state", oldSelected); err != nil {
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
		changedDocs, changedPaths := changedFolderDocumentsFromManifest(selected, manifest)
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
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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

// BenchmarkFolderCorpus_DirtyReindexPhases_1File measures coarse production
// functions used by dirty folder refresh. The full refresh contract is measured
// by BenchmarkFolderCorpus_DirtyReindex_1File.
func BenchmarkFolderCorpus_DirtyReindexPhases_1File(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
	changedDocs, changedPaths := changedFolderDocumentsFromManifest(selected, manifest)
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
			docs, paths := changedFolderDocumentsFromManifest(selected, manifest)
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
			if _, err := indexDeltaDocuments(indexDir, repoName, plan.root, changedDocs, folderDeltaShardMax, changedPaths); err != nil {
				b.Fatalf("index delta documents with base shard: %v", err)
			}
			b.StopTimer()
			_ = os.RemoveAll(indexDir)
			b.StartTimer()
		}
	})
}

// BenchmarkFolderCorpus_OneFileZoektShardLowerBound measures the raw lower
// bound for a bucket-style dirty refresh: build a fresh one-file Zoekt shard
// with seek's normal indexing options, then search it through Zoekt. It does
// not represent the product dirty-reindex contract; that remains
// BenchmarkFolderCorpus_DirtyReindex_1File.
func BenchmarkFolderCorpus_OneFileZoektShardLowerBound(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
		results, err := executeParsedSearchScoped(ctx, indexDir, userQ, nil)
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

// --- Large-repo benchmarks ---
// Set SEEK_BENCH_REPO to a git repo path (e.g. a kubernetes checkout) to
// enable these. They measure real-world indexing and search latency on a
// large codebase where the overhead is actually visible.
//
//   git clone --depth=1 https://github.com/kubernetes/kubernetes /tmp/k8s
//   SEEK_BENCH_REPO=/tmp/k8s go test ./cmd/seek/ -bench=BenchmarkLargeRepo -benchmem -count=3

func requireBenchRepo(b *testing.B) string {
	b.Helper()
	dir := os.Getenv("SEEK_BENCH_REPO")
	if dir == "" {
		b.Skip("SEEK_BENCH_REPO not set — skipping large-repo benchmark")
	}
	requireTools(b)
	return dir
}

// setupLargeRepoBench ensures the index is warm and returns the planned corpus.
func setupLargeRepoBench(b *testing.B) (repoDir string, paths gitPaths, plan corpusPlan) {
	b.Helper()
	repoDir = requireBenchRepo(b)
	paths, plan = planGitTestCorpus(b, repoDir)

	ctx := context.Background()
	if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
		b.Fatalf("initial indexing failed: %v", err)
	}
	return repoDir, paths, plan
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

		if results, err := runSeekInPlannedGitCorpus(ctx, "func main", paths, plan); err != nil {
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
		if results, err := runSeekInPlannedGitCorpus(ctx, "func main", paths, plan); err != nil {
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
	repoDir, paths, plan := setupLargeRepoBench(b)
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
	b.Cleanup(func() {
		for i, t := range targets {
			_ = os.WriteFile(t, originals[i], 0o644)
		}
	})

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

// BenchmarkLargeRepo_Phases breaks down the dirty-reindex path into
// individual phases so we can see where time is actually spent.
func BenchmarkLargeRepo_Phases(b *testing.B) {
	repoDir := requireBenchRepo(b)
	paths, plan := planGitTestCorpus(b, repoDir)
	ctx := context.Background()

	if _, _, err := ensureGitCorpusFresh(ctx, plan, paths); err != nil {
		b.Fatalf("initial indexing: %v", err)
	}

	target := findSourceFile(b, repoDir)
	original, err := os.ReadFile(target)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = os.WriteFile(target, original, 0o644) })

	b.Run("gitRepoStateIn", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			gitRepoStateIn(ctx, repoDir)
		}
	})

	b.Run("stateHash", func(b *testing.B) {
		state := gitRepoStateIn(ctx, repoDir)
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

	b.Run("planCurrentGitCorpus", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = planCurrentGitCorpus(paths)
		}
	})

	b.Run("ensureUntrackedCache", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ensureUntrackedCache(ctx, paths)
		}
	})

	b.Run("indexCommitted_incremental", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := indexCommitted(paths.RepoDir, plan.indexDir, indexParallelism()); err != nil {
				b.Fatalf("index committed: %v", err)
			}
		}
	})

	b.Run("indexUncommitted_1file", func(b *testing.B) {
		// Dirty the file once, capture the file list, then benchmark just
		// the indexing loop without re-running git status each iteration.
		if err := os.WriteFile(target, append(original, []byte("\n// dirty\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		state := gitRepoStateIn(ctx, repoDir)
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
			loopState := gitRepoStateIn(ctx, repoDir)
			preState := gitCorpusStateHash(paths, loopState)
			if err := indexUncommitted(ctx, repoDir, plan.indexDir, plan.cacheDir, loopState, cachedState, preState, indexParallelism()); err != nil {
				b.Fatalf("index uncommitted: %v", err)
			}
		}
	})

	b.Run("postVerify_gitStatus", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			postState := gitRepoStateIn(ctx, repoDir)
			gitCorpusStateHash(paths, postState)
		}
	})

	b.Run("postVerify_restat", func(b *testing.B) {
		// Lightweight alternative: re-stat only the known dirty files
		// instead of running a full git status. Uses the same state struct
		// (same RawOutput), but repoStateFingerprint re-Lstats each file.
		if err := os.WriteFile(target, append(original, []byte("\n// dirty_for_restat\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		dirtyState := gitRepoStateIn(ctx, repoDir)
		if len(dirtyState.Files) == 0 {
			b.Fatal("expected dirty files")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			gitCorpusStateHash(paths, dirtyState)
		}
	})

	b.Run("executeParsedSearchScoped", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := executeUnscopedShardSearchForTest(ctx, plan.indexDir, "func main"); err != nil {
				b.Fatalf("execute search: %v", err)
			}
		}
	})

	// --- Post-indexation phase breakdown ---

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

	b.Run("acquireSearchLock_uncontended", func(b *testing.B) {
		lockPath := filepath.Join(plan.cacheDir, lockFile)
		b.ReportAllocs()
		for b.Loop() {
			f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				b.Fatal(err)
			}
			_ = acquireSearchLock(ctx, f)
			unlockFile(f)
			_ = f.Close()
		}
	})

	b.Run("dirtyFileSetFromState", func(b *testing.B) {
		if err := os.WriteFile(target, append(original, []byte("\n// dirty_set\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		dirtyState := gitRepoStateIn(ctx, repoDir)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_ = dirtyFileSetFromState(dirtyState)
		}
	})

	b.Run("formatCorpusResultsWithContext", func(b *testing.B) {
		results, err := executeUnscopedShardSearchForTest(ctx, plan.indexDir, "func main")
		if err != nil {
			b.Fatal(err)
		}
		if len(results) == 0 {
			b.Skip("no results to format")
		}
		corpusResults := benchmarkGitResults(results)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			formatCorpusResultsWithContext(corpusResults, nil, 0, 0, hideCorpusContext)
		}
	})

	b.Run("formatCorpusResultsWithContext_dirtyFiles", func(b *testing.B) {
		if err := os.WriteFile(target, append(original, []byte("\n// dirty_fmt\n")...), 0o644); err != nil {
			b.Fatal(err)
		}
		dirtyState := gitRepoStateIn(ctx, repoDir)
		results, err := executeUnscopedShardSearchForTest(ctx, plan.indexDir, "func main")
		if err != nil {
			b.Fatal(err)
		}
		if len(results) == 0 {
			b.Skip("no results to format")
		}
		dirtyFiles := make(dirtyFileSet, len(dirtyState.Files))
		for _, f := range dirtyState.Files {
			dirtyFiles[f] = struct{}{}
		}
		corpusResults := benchmarkGitResults(results)
		dirtyByCorpus := benchmarkDirtyFilesByCorpus(dirtyFiles)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			formatCorpusResultsWithContext(corpusResults, dirtyByCorpus, 0, 0, hideCorpusContext)
		}
	})
}

// findSourceFile returns the absolute path of a source file suitable for editing.
func findSourceFile(b *testing.B, repoDir string) string {
	b.Helper()
	targets := findSourceFiles(b, repoDir, 1)
	return targets[0]
}

// findSourceFiles returns absolute paths of n source files suitable for editing.
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
		b.Skipf("repo has fewer than %d source files", n)
	}
	return result
}

func isBenchmarkSourceFile(d os.DirEntry) bool {
	if d.Type()&os.ModeSymlink != 0 {
		return false
	}
	info, err := d.Info()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxGitDirtyFileSize {
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

// --- Delta-indexing benches (added with the IsDelta migration) ---

// BenchmarkGitCommitted_1CommitAhead measures the steady-state cost of
// indexing a single committed change. Each iteration writes a new file,
// commits it, and runs indexCommitted with IsDelta=true. The expected gain
// vs a hypothetical IsDelta=false build comes from Zoekt's tree-to-tree
// diff (only the new blob is hashed + ctags-parsed).
func BenchmarkGitCommitted_1CommitAhead(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
	requireTools(b)

	dir := initGitRepo(b, "seed.go", "package main\n// commit_ahead_seed\n")
	paths, plan := planGitTestCorpus(b, dir)
	if err := indexCommitted(paths.RepoDir, plan.indexDir, indexParallelism()); err != nil {
		b.Fatalf("initial commit index: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		name := fmt.Sprintf("step%d.go", i)
		body := fmt.Appendf(nil, "package main\n// commit_ahead_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			b.Fatal(err)
		}
		gitRunIn(b, dir, "add", ".")
		gitRunIn(b, dir, "commit", "-m", "delta step")
		if err := indexCommitted(paths.RepoDir, plan.indexDir, indexParallelism()); err != nil {
			b.Fatalf("delta commit index: %v", err)
		}
	}
}

// BenchmarkGitUncommitted_RapidEdits_N16 simulates editor-driven saves:
// rewrite the same file 16 times and measure the cumulative per-iteration
// indexing cost. With delta on, each cycle should write one tiny shard and
// tombstone the prior one. Empty-shard cleanup keeps the count bounded.
func BenchmarkGitUncommitted_RapidEdits_N16(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n// rapid_edits_baseline\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, dir)
	// Establish the cachedState baseline by indexing once with no dirty files.
	state := gitRepoStateIn(ctx, dir)
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
			state := gitRepoStateIn(ctx, dir)
			preState := gitCorpusStateHash(paths, state)
			if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, preState); err != nil {
				b.Fatalf("delta cycle %d.%d: %v", i, j, err)
			}
		}
	}
}

// BenchmarkGitBranch_Switch_Unrelated measures the pathological case: a
// branch switch that rewrites most of the indexed tree. Delta builds keep
// emitting tombstones until the shard threshold trips and Zoekt falls back
// to a full rebuild — the bench reports the combined cost so we can confirm
// the worst case does not regress vs a hypothetical IsDelta=false build.
func BenchmarkGitBranch_Switch_Unrelated(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
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
	mainBranch := gitCurrentBranch(&testing.T{}, dir)

	// Create a feature branch that overwrites 80% of the files.
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
	if err := indexCommitted(paths.RepoDir, plan.indexDir, indexParallelism()); err != nil {
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
		if err := indexCommitted(paths.RepoDir, plan.indexDir, indexParallelism()); err != nil {
			b.Fatalf("post-switch index: %v", err)
		}
	}
}

// BenchmarkSearchTombstoneCost measures the search-time penalty of an
// accumulated delta shard set. After N edit cycles, only the latest content
// is live; older shards have all docs tombstoned. The bench asserts that
// search over the multi-shard index returns the expected hit and reports
// wall time + allocs so regressions in eval.go's tombstone filter show up
// in CodSpeed walltime mode.
func BenchmarkSearchTombstoneCost(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n// tombstone_cost_baseline\n")
	ctx := context.Background()
	paths, plan := planGitTestCorpus(b, dir)
	state := gitRepoStateIn(ctx, dir)
	preState := gitCorpusStateHash(paths, state)
	if err := runIndexingWithCache(ctx, paths, plan.cacheDir, plan.indexDir, state, preState); err != nil {
		b.Fatalf("baseline index: %v", err)
	}

	// Generate 16 delta cycles on the same file. After cleanup, the live
	// content lives in one shard and the rest hold tombstones.
	for i := range 16 {
		body := fmt.Appendf(nil, "package main\n// tombstone_cost_iter_%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, "app.go"), body, 0o644); err != nil {
			b.Fatal(err)
		}
		state := gitRepoStateIn(ctx, dir)
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

// =============================================================================
// GC benchmarks
//
// Cover the GC integration end-to-end. Pinned scenarios:
//   - hot path (after every search): touchUsed + opportunistic GC throttled
//   - cold path (eviction): enumerate + per-corpus evict + full cycle
//   - dry-run rendering: reportGCPlan with real + seeded corpora
//
// Defends audit findings against silent regressions on CodSpeed:
//   - throttle gate before MkdirAll+isOnNFS (warm path 3→1 syscall)
//   - touchStamp / touchUsed Chtimes fast path
//   - shard picker HasPrefix (not substring)
// =============================================================================

func BenchmarkGC_TouchUsed_WarmPath(b *testing.B) {
	resetNFSCheck(b)
	root := cacheRootForTest(b)
	dir := filepath.Join(root, corporaDir, fakeCorpusHash(0))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	touchUsed(dir) // prime .used + nfsCheckOnce

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
	// Prime nfsCheckOnce so subsequent loop iters don't pay Statfs.
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
	// Fresh stamp → throttle gate must short-circuit after 1 stat.
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
		// Re-seed every iter to lock in the "no evictions" invariant. If
		// runGC ever regresses to wrongly evict fresh corpora, a one-shot
		// setup would let the first iter evict everything and subsequent
		// iters fast-path through an empty cache — masking the regression
		// in the average. Per-iter reseed forces every measured cycle to
		// face the same 100-corpus enumeration + skip path.
		now := time.Now()
		for i := 0; i < 100; i++ {
			seedCorpus(b, root, fakeCorpusHash(i), now)
		}
		b.StartTimer()

		runGC(ctx, gcOptions{maxAge: defaultGCMaxAge, skipThrottle: true}, defaultGCInterval)
	}
}

func BenchmarkGC_RunGC_FullEvict_N100(b *testing.B) {
	resetNFSCheck(b)
	root := cacheRootForTest(b)
	stale := time.Now().Add(-30 * 24 * time.Hour)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		// Recreate the 100 stale corpora each iter — runGC's prior pass
		// just evicted them.
		for i := 0; i < 100; i++ {
			seedCorpus(b, root, fakeCorpusHash(i), stale)
		}
		// Bust throttle so the next call evicts even though stamp is fresh.
		_ = os.Remove(filepath.Join(root, gcStampFile))
		b.StartTimer()

		runGC(ctx, gcOptions{maxAge: defaultGCMaxAge, skipThrottle: true}, defaultGCInterval)
	}
}

func BenchmarkGC_PickDisplayShard(b *testing.B) {
	cases := [][]string{
		{"/c/index/github.com%2Ffoo%2Fbar_v17.00000.zoekt"},
		{"/c/index/uncommitted_v17.00000.zoekt", "/c/index/github.com%2Ffoo%2Fbar_v17.00000.zoekt"},
		{"/c/index/uncommitted_v17.00000.zoekt", "/c/index/github.com%2Ffoo%2Funcommitted-tool_v17.00000.zoekt"},
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, c := range cases {
			_ = pickDisplayShard(c)
		}
	}
}

func BenchmarkGC_DryRun_Render_N20(b *testing.B) {
	if testing.Short() {
		b.Skip("builds a real git repo")
	}
	requireTools(b)
	resetNFSCheck(b)

	// One real corpus so corpusDisplayName has actual shard metadata to
	// read on at least one row — exercises ReadMetadataPath + [gone]/
	// [empty] branches together.
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
	// 19 shardless corpora alongside the real one. Their displayName will
	// be [empty]; rendering cost is what we measure.
	now := time.Now()
	for i := 0; i < 19; i++ {
		seedCorpus(b, root, fakeCorpusHash(i+1), now)
	}

	corporaPath := filepath.Join(root, corporaDir)
	entries, err := enumerateCorpusDirs(corporaPath)
	if err != nil {
		b.Fatal(err)
	}
	cutoff := time.Now().Add(-defaultGCMaxAge)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := reportGCPlan(io.Discard, root, entries, cutoff); err != nil {
			b.Fatal(err)
		}
	}
}
