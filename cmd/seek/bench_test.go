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
	b.ReportAllocs()
	for b.Loop() {
		ensureGitExclude(dir, cacheDir)
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
		formatResults(files)
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
		formatResults(files)
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
		formatResults(files)
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
		formatResults(files)
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
		deduplicateFiles(files)
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

