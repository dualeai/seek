package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
)

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
		_ = os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("package f%d\n", i)), 0o644)
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
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(fmt.Sprintf("package f%d\n", i)), 0o644)
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
	_ = writeStateFile(dir, "abc123def456789a")
	b.ReportAllocs()
	for b.Loop() {
		readStateFile(dir)
	}
}

func BenchmarkWriteStateFile(b *testing.B) {
	dir := b.TempDir()
	b.ReportAllocs()
	for b.Loop() {
		_ = writeStateFile(dir, "abc123def456789a")
	}
}

func BenchmarkExtractV2Path(b *testing.B) {
	entry := "1 .M N... 100644 100644 100644 abc123def456 def456abc123 src/deeply/nested/package/file.go"
	b.ReportAllocs()
	for b.Loop() {
		extractV2Path(entry, 8)
	}
}

func BenchmarkEnsureGitExclude_AlreadyPresent(b *testing.B) {
	dir := b.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("/.seek-cache\n"), 0o644)
	paths := fallbackGitPaths(dir)
	b.ReportAllocs()
	for b.Loop() {
		ensureGitExclude(paths, cacheDir)
	}
}

// --- Formatter benchmarks ---

func BenchmarkFormatResults_1File_1Match(b *testing.B) {
	files := []zoekt.FileMatch{
		{
			FileName: "src/main.go", Repository: "repo", Language: "Go", Score: 10,
			LineMatches: []zoekt.LineMatch{
				{Line: []byte("func main() {\n"), LineNumber: 5},
			},
		},
	}
	b.ReportAllocs()
	for b.Loop() {
		formatResults(files, nil)
	}
}

func BenchmarkFormatResults_10Files_3Matches(b *testing.B) {
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
	b.ReportAllocs()
	for b.Loop() {
		formatResults(files, nil)
	}
}

func BenchmarkFormatResults_100Files_WithDedup(b *testing.B) {
	files := make([]zoekt.FileMatch, 200)
	for i := range 100 {
		files[i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("file_%03d.go", i), Repository: "repo", Language: "Go",
			Score:       float64(i),
			LineMatches: []zoekt.LineMatch{{Line: []byte("match\n"), LineNumber: 1}},
		}
		// Duplicate as uncommitted
		files[100+i] = zoekt.FileMatch{
			FileName: fmt.Sprintf("file_%03d.go", i), Repository: repoUncommitted, Language: "Go",
			Score:       float64(i + 1),
			LineMatches: []zoekt.LineMatch{{Line: []byte("updated match\n"), LineNumber: 1}},
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		formatResults(files, nil)
	}
}

func BenchmarkFormatResults_WithSymbols(b *testing.B) {
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
	b.ReportAllocs()
	for b.Loop() {
		formatResults(files, nil)
	}
}

func BenchmarkDeduplicateFiles_100(b *testing.B) {
	files := make([]zoekt.FileMatch, 200)
	for i := range 100 {
		files[i] = zoekt.FileMatch{FileName: fmt.Sprintf("f%d.go", i), Repository: "repo"}
		files[100+i] = zoekt.FileMatch{FileName: fmt.Sprintf("f%d.go", i), Repository: repoUncommitted}
	}
	b.ReportAllocs()
	for b.Loop() {
		deduplicateFiles(files, nil)
	}
}

func BenchmarkSplitContextLines(b *testing.B) {
	data := []byte("line one\nline two\nline three\n")
	b.ReportAllocs()
	for b.Loop() {
		splitContextLines(data)
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
	dir := b.TempDir()
	const numFiles = 50
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

func BenchmarkStreamFiles_200Files(b *testing.B) {
	dir := b.TempDir()
	const numFiles = 200
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

func BenchmarkEndToEnd_ColdIndex(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
	requireTools(b)

	for b.Loop() {
		dir := initGitRepo(b, "app.go", "package main\n\nfunc main() {\n\t// benchmark_marker_cold\n}\n")
		ctx := context.Background()
		indexDir := filepath.Join(dir, cacheDir)
		_ = os.MkdirAll(indexDir, 0o755)

		state := gitRepoStateIn(ctx, dir)
		stateHash := computeStateHash(repoStateFingerprint(dir, state))
		_ = runIndexing(ctx, dir, indexDir, state, stateHash)
		_, _ = executeSearch(ctx, indexDir, "benchmark_marker_cold")
	}
}

func BenchmarkEndToEnd_WarmIndex(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n\nfunc main() {\n\t// benchmark_marker_warm\n}\n")
	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	_ = os.MkdirAll(indexDir, 0o755)

	// Cold run to build index
	state := gitRepoStateIn(ctx, dir)
	stateHash := computeStateHash(repoStateFingerprint(dir, state))
	_ = runIndexing(ctx, dir, indexDir, state, stateHash)

	b.ResetTimer()
	for b.Loop() {
		// Warm path: state check + search (no re-index)
		state := gitRepoStateIn(ctx, dir)
		currentState := computeStateHash(repoStateFingerprint(dir, state))
		cachedState := readStateFile(indexDir)
		if currentState != cachedState {
			_ = runIndexing(ctx, dir, indexDir, state, currentState)
		}
		_, _ = executeSearch(ctx, indexDir, "benchmark_marker_warm")
	}
}

func BenchmarkEndToEnd_DirtyReindex(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping end-to-end benchmark in short mode")
	}
	requireTools(b)

	dir := initGitRepo(b, "app.go", "package main\n\nfunc main() {\n\t// dirty_bench\n}\n")
	ctx := context.Background()
	indexDir := filepath.Join(dir, cacheDir)
	_ = os.MkdirAll(indexDir, 0o755)

	// Cold run
	state := gitRepoStateIn(ctx, dir)
	stateHash := computeStateHash(repoStateFingerprint(dir, state))
	_ = runIndexing(ctx, dir, indexDir, state, stateHash)

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		// Simulate edit on each iteration
		content := fmt.Sprintf("package main\n\nfunc main() {\n\t// dirty_bench_iter_%d\n}\n", i)
		_ = os.WriteFile(filepath.Join(dir, "app.go"), []byte(content), 0o644)

		state := gitRepoStateIn(ctx, dir)
		currentState := computeStateHash(repoStateFingerprint(dir, state))
		_ = runIndexing(ctx, dir, indexDir, state, currentState)
		_, _ = executeSearch(ctx, indexDir, "dirty_bench")
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

// setupLargeRepoBench ensures the index is warm and returns the repo/index dirs.
func setupLargeRepoBench(b *testing.B) (repoDir, indexDir string) {
	b.Helper()
	repoDir = requireBenchRepo(b)
	indexDir = filepath.Join(repoDir, cacheDir)
	_ = os.MkdirAll(indexDir, 0o755)
	ensureGitExclude(fallbackGitPaths(repoDir), cacheDir)

	ctx := context.Background()
	state := gitRepoStateIn(ctx, repoDir)
	currentState := computeStateHash(repoStateFingerprint(repoDir, state))
	cachedState := readStateFile(indexDir)
	if currentState != cachedState {
		if err := runIndexing(ctx, repoDir, indexDir, state, currentState); err != nil {
			b.Fatalf("initial indexing failed: %v", err)
		}
	}
	return repoDir, indexDir
}

func BenchmarkLargeRepo_WarmSearch(b *testing.B) {
	repoDir, indexDir := setupLargeRepoBench(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		state := gitRepoStateIn(ctx, repoDir)
		currentState := computeStateHash(repoStateFingerprint(repoDir, state))
		cachedState := readStateFile(indexDir)
		if currentState != cachedState {
			_ = runIndexing(ctx, repoDir, indexDir, state, currentState)
		}
		_, _ = executeSearch(ctx, indexDir, "func main")
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
	repoDir, indexDir := setupLargeRepoBench(b)
	ctx := context.Background()

	targets := findGoFiles(b, repoDir, n)
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
		for j, t := range targets {
			content := fmt.Appendf(originals[j][:len(originals[j]):len(originals[j])], "\n// bench_%d\n", i)
			_ = os.WriteFile(t, content, 0o644)
		}
		state := gitRepoStateIn(ctx, repoDir)
		currentState := computeStateHash(repoStateFingerprint(repoDir, state))
		_ = runIndexing(ctx, repoDir, indexDir, state, currentState)
		_, _ = executeSearch(ctx, indexDir, "func main")
	}
}

// BenchmarkLargeRepo_Phases breaks down the dirty-reindex path into
// individual phases so we can see where time is actually spent.
func BenchmarkLargeRepo_Phases(b *testing.B) {
	repoDir := requireBenchRepo(b)
	indexDir := filepath.Join(repoDir, cacheDir)
	_ = os.MkdirAll(indexDir, 0o755)
	paths := fallbackGitPaths(repoDir)
	ensureGitExclude(paths, cacheDir)
	ctx := context.Background()

	// Ensure index is warm
	state := gitRepoStateIn(ctx, repoDir)
	currentState := computeStateHash(repoStateFingerprint(repoDir, state))
	cachedState := readStateFile(indexDir)
	if currentState != cachedState {
		if err := runIndexing(ctx, repoDir, indexDir, state, currentState); err != nil {
			b.Fatalf("initial indexing: %v", err)
		}
	}

	target := findGoFile(b, repoDir)
	original, err := os.ReadFile(target)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = os.WriteFile(target, original, 0o644) })

	b.Run("gitRepoState", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			gitRepoStateIn(ctx, repoDir)
		}
	})

	b.Run("stateHash", func(b *testing.B) {
		state := gitRepoStateIn(ctx, repoDir)
		b.ReportAllocs()
		for b.Loop() {
			computeStateHash(repoStateFingerprint(repoDir, state))
		}
	})

	b.Run("checkCtags", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = checkCtags()
		}
	})

	b.Run("ensureGitExclude", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ensureGitExclude(paths, cacheDir)
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
			_ = indexCommitted(ctx, paths, indexDir, indexParallelism())
		}
	})

	b.Run("indexUncommitted_1file", func(b *testing.B) {
		// Dirty the file once, capture the file list, then benchmark just
		// the indexing loop without re-running git status each iteration.
		_ = os.WriteFile(target, append(original, []byte("\n// dirty\n")...), 0o644)
		state := gitRepoStateIn(ctx, repoDir)
		if len(state.Files) == 0 {
			b.Fatal("expected dirty files")
		}
		dirtyFiles := state.Files

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			content := fmt.Appendf(original[:len(original):len(original)], "\n// p_%d\n", i)
			_ = os.WriteFile(target, content, 0o644)
			fileCh := streamFiles(repoDir, dirtyFiles, indexParallelism())
			_ = indexUncommitted(ctx, repoDir, indexDir, fileCh, indexParallelism())
		}
	})

	b.Run("postVerify_gitStatus", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			postState := gitRepoStateIn(ctx, repoDir)
			computeStateHash(repoStateFingerprint(repoDir, postState))
		}
	})

	b.Run("postVerify_restat", func(b *testing.B) {
		// Lightweight alternative: re-stat only the known dirty files
		// instead of running a full git status. Uses the same state struct
		// (same RawOutput), but repoStateFingerprint re-Lstats each file.
		_ = os.WriteFile(target, append(original, []byte("\n// dirty_for_restat\n")...), 0o644)
		dirtyState := gitRepoStateIn(ctx, repoDir)
		if len(dirtyState.Files) == 0 {
			b.Fatal("expected dirty files")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			computeStateHash(repoStateFingerprint(repoDir, dirtyState))
		}
	})

	b.Run("executeSearch", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = executeSearch(ctx, indexDir, "func main")
		}
	})
}

// findGoFile returns the absolute path of a Go file suitable for editing.
func findGoFile(b *testing.B, repoDir string) string {
	b.Helper()
	targets := findGoFiles(b, repoDir, 1)
	return targets[0]
}

// findGoFiles returns absolute paths of n Go files suitable for editing.
func findGoFiles(b *testing.B, repoDir string, n int) []string {
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
		if filepath.Ext(path) == ".go" {
			result = append(result, path)
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	if len(result) < n {
		b.Skipf("repo has fewer than %d .go files", n)
	}
	return result
}
