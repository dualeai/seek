//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const seekBenchmarkBinaryEnv = "SEEK_BENCH_BINARY"

var (
	benchmarkLateOnScores    []float32
	benchmarkLateOnInputIDs  []int64
	benchmarkLateOnAttention []int64
	benchmarkLateOnScoreMask [][]bool
)

func BenchmarkCLIProcess(b *testing.B) {
	binary := seekBenchmarkBinary(b)
	if err := checkCtagsCached(); err != nil {
		b.Skipf("ctags required: %v", err)
	}

	root := b.TempDir()
	fixture := filepath.Join(root, "fixture")
	lexicalCache := filepath.Join(root, "lexical-cache")
	semanticCache := filepath.Join(root, "semantic-cache")
	// Use more than the model pool so the timed relaxed search also covers the
	// candidate bound before deep result cloning.
	for i := range 256 {
		terms := []string{"parse", "request", "handler"}
		if i >= 5 {
			terms = terms[i%len(terms) : i%len(terms)+1]
		}
		writeFileAt(
			b,
			fixture,
			fmt.Sprintf("candidate_%02d.go", i),
			fmt.Sprintf(
				"package fixture\n\n// %s\nfunc candidate%d() {}\n",
				strings.Join(terms, " "),
				i,
			),
		)
	}

	const queryText = "parse request handler"
	lexicalArgs := []string{"--lexical-only", queryText, fixture}
	ineligibleArgs := []string{"content:parse request handler", fixture}
	eligibleArgs := []string{queryText, fixture}
	eligibleDisplayContextZeroArgs := []string{"-C", "0", queryText, fixture}
	// The relaxed lexical query starts with one content candidate. Semantic
	// retrieval can expand it before final scoring.
	semanticExpansionArgs := []string{"candidate0 candidate_00", fixture}

	// Keep the lexical-only no-model check independent from default searches,
	// which can build semantic data even when a query is not eligible to rerank.
	output, err := runSeekBenchmarkCommand(
		b.Context(), binary, lexicalCache, fixture, lexicalArgs, false,
	)
	if err != nil || len(output) == 0 {
		b.Fatalf("warm seek %q: output=%q err=%v", lexicalArgs, output, err)
	}
	if _, err := os.Stat(filepath.Join(lexicalCache, "reranker")); !os.IsNotExist(err) {
		b.Fatalf("lexical-only search initialized the re-ranker: %v", err)
	}
	output, err = runSeekBenchmarkCommand(
		b.Context(), binary, semanticCache, fixture, ineligibleArgs, false,
	)
	if err != nil || len(output) == 0 {
		b.Fatalf("warm seek %q: output=%q err=%v", ineligibleArgs, output, err)
	}
	probeArgs := append([]string{"--verbose"}, eligibleArgs...)
	output, err = runSeekBenchmarkCommand(
		b.Context(),
		binary,
		semanticCache,
		fixture,
		probeArgs,
		false,
	)
	if err != nil || len(output) == 0 || bytes.Contains(output, []byte("Re-ranking failed")) {
		b.Fatalf("warm seek %q: output=%q err=%v", probeArgs, output, err)
	}
	semanticExpansionProbeArgs := append([]string{"--verbose"}, semanticExpansionArgs...)
	output, err = runSeekBenchmarkCommand(
		b.Context(),
		binary,
		semanticCache,
		fixture,
		semanticExpansionProbeArgs,
		false,
	)
	if err != nil || len(output) == 0 || bytes.Contains(output, []byte("Re-ranking failed")) {
		b.Fatalf("warm seek %q: output=%q err=%v", semanticExpansionProbeArgs, output, err)
	}

	b.Run("LexicalOnlyWarm", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, lexicalCache, fixture, lexicalArgs)
		}
	})
	b.Run("RerankIneligibleWarm", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, semanticCache, fixture, ineligibleArgs)
		}
	})
	b.Run("RerankEligibleFirstUse", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			// Remove the extracted run time, which is what "first use" measures.
			// Keep reranker/coreml: it holds the compiled Core ML model and the
			// provider verdict, and rebuilding both on every iteration would
			// measure the Core ML compiler rather than Seek.
			entries, err := os.ReadDir(filepath.Join(semanticCache, "reranker"))
			if err != nil && !os.IsNotExist(err) {
				b.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() == "coreml" {
					continue
				}
				if err := os.RemoveAll(filepath.Join(semanticCache, "reranker", entry.Name())); err != nil {
					b.Fatal(err)
				}
			}
			b.StartTimer()
			mustRunSeekBenchmark(b, binary, semanticCache, fixture, eligibleArgs)
		}
	})
	b.Run("RerankEligibleProviderCheck", func(b *testing.B) {
		// RerankEligibleFirstUse deliberately keeps the compiled Core ML model,
		// and the provider verdict lives in the same directory, so that case
		// never runs the provider check. Remove the verdict alone to measure the
		// check itself: a second session on the reference provider and one batch
		// through each. Without this the only cost this feature adds has no
		// benchmark at all.
		for b.Loop() {
			b.StopTimer()
			removeProviderVerdicts(b, semanticCache)
			b.StartTimer()
			mustRunSeekBenchmark(b, binary, semanticCache, fixture, eligibleArgs)
		}
	})
	b.Run("RerankEligibleWarm", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, semanticCache, fixture, eligibleArgs)
		}
	})
	b.Run("RerankEligibleWarmDisplayContextZero", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, semanticCache, fixture, eligibleDisplayContextZeroArgs)
		}
	})
	b.Run("RerankSemanticExpansionWarm", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, semanticCache, fixture, semanticExpansionArgs)
		}
	})
}

func BenchmarkLateOnModelStartup_WarmRuntime(b *testing.B) {
	b.Setenv("SEEK_CACHE_DIR", b.TempDir())
	warm, err := newLateOnSemanticModel(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	if err := warm.Close(); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		scorer, err := newLateOnSemanticModel(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		closeErr := scorer.Close()
		b.StartTimer()
		if closeErr != nil {
			b.Fatal(closeErr)
		}
	}
}

func BenchmarkLateOnScoring_Top20(b *testing.B) {
	benchmarkLateOnScoring(b, 20)
}

func BenchmarkLateOnScoring_FullBatch(b *testing.B) {
	benchmarkLateOnScoring(b, rerankCandidateLimit)
}

func benchmarkLateOnScoring(b *testing.B, documentCount int) {
	b.Setenv("SEEK_CACHE_DIR", b.TempDir())
	scorer, err := newLateOnSemanticModel(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := scorer.Close(); err != nil {
			b.Errorf("close scorer: %v", err)
		}
	})

	documents := lateOnBenchmarkDocuments(documentCount)
	const modelQuery = "find the request parser and handler"
	scores, err := scoreWithTestSemanticEmbedder(b.Context(), scorer, modelQuery, documents)
	if err != nil || len(scores) != len(documents) {
		b.Fatalf("warm scorer: scores=%d err=%v", len(scores), err)
	}
	benchmarkLateOnScores = scores

	b.ReportAllocs()
	for b.Loop() {
		benchmarkLateOnScores, err = scoreWithTestSemanticEmbedder(
			b.Context(),
			scorer,
			modelQuery,
			documents,
		)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSemanticModelPass100k measures the production indexing scheduler
// with a fixed set of prepared model rows. Setup, model creation, and one warm
// provider call stay outside the timer. The timed pass includes input packing,
// model inference, and the production coarse and fine vector calculation.
func BenchmarkSemanticModelPass100k(b *testing.B) {
	b.Setenv("SEEK_CACHE_DIR", b.TempDir())
	b.StopTimer()
	scorer, err := newLateOnSemanticModel(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	embedder := scorer
	b.Cleanup(func() {
		if err := scorer.Close(); err != nil {
			b.Errorf("close scorer: %v", err)
		}
	})

	const rowCount = 100_000
	units := make([]semanticUnit, rowCount)
	modelInputs := make([]int, rowCount*lateOnSequenceLength)
	for row := range units {
		modelInput := modelInputs[row*lateOnSequenceLength : (row+1)*lateOnSequenceLength]
		modelInput[0] = lateOnCLSTokenID
		for index := 1; index < len(modelInput)-1; index++ {
			modelInput[index] = 100 + (row*17+index*31)%49_000
		}
		modelInput[len(modelInput)-1] = lateOnSEPTokenID
		units[row] = semanticUnit{row: uint64(row), modelInput: modelInput}
	}
	if vectors, err := embedder.EmbedSemanticUnits(b.Context(), units[:semanticModelBatchRows]); err != nil ||
		len(vectors) != semanticModelBatchRows {
		b.Fatalf("warm semantic model: vectors=%d error=%v", len(vectors), err)
	}
	callCPUs := semanticModelCallCPUs(b.Context(), embedder)
	b.ResetTimer()
	b.StartTimer()

	for b.Loop() {
		b.StopTimer()
		batchCount := (len(units) + semanticModelBatchRows - 1) / semanticModelBatchRows
		batches := make(chan semanticBuildBatch, batchCount)
		for sequence := range batchCount {
			start := sequence * semanticModelBatchRows
			end := min(start+semanticModelBatchRows, len(units))
			batches <- semanticBuildBatch{sequence: sequence, units: units[start:end]}
		}
		close(batches)
		encoded := make(chan semanticEncodedBatch)
		ctx, cancel := context.WithCancel(b.Context())
		b.StartTimer()
		go encodeSemanticBatches(ctx, embedder, batches, encoded, cancel, searchResources{
			now:             time.Now,
			effectiveCPUs:   func() int { return runtime.GOMAXPROCS(0) },
			availableMemory: semanticHostAvailableMemory,
			callCPUs:        callCPUs,
		})
		completed := 0
		var modelErr error
		for batch := range encoded {
			completed += len(batch.embeddings)
			if modelErr == nil && batch.err != nil {
				modelErr = batch.err
			}
		}
		b.StopTimer()
		cancel()
		if modelErr != nil {
			b.Fatal(modelErr)
		}
		if completed != rowCount {
			b.Fatalf("completed model rows=%d, want %d", completed, rowCount)
		}
		b.StartTimer()
	}
}

func BenchmarkLateOnDocumentTokenization_Top20Truncated(b *testing.B) {
	benchmarkLateOnDocumentTokenization(b, 20)
}

func BenchmarkLateOnDocumentTokenization_FullBatchTruncated(b *testing.B) {
	benchmarkLateOnDocumentTokenization(b, rerankCandidateLimit)
}

func benchmarkLateOnDocumentTokenization(b *testing.B, documentCount int) {
	tokenizer, punctuation := openLateOnTestTokenizer(b)
	documents := lateOnBenchmarkTruncatedDocuments(documentCount)
	serialized, _, _, _ := serializeLateOnDocumentWithMatch(documents[0])
	encoded, _, err := tokenizer.encode(lateOnDocumentPrefix+serialized, true, true)
	if err != nil {
		b.Fatal(err)
	}
	if len(encoded) <= lateOnSequenceLength {
		b.Fatalf("benchmark document has %d tokens; want more than %d", len(encoded), lateOnSequenceLength)
	}

	benchmarkLateOnInputIDs, benchmarkLateOnAttention, benchmarkLateOnScoreMask, err =
		tokenizeLateOnDocuments(
			tokenizer,
			punctuation,
			documents,
		)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(documents) * len(documents[0].Text)))
	b.ReportAllocs()
	for b.Loop() {
		benchmarkLateOnInputIDs, benchmarkLateOnAttention, benchmarkLateOnScoreMask, err =
			tokenizeLateOnDocuments(
				tokenizer,
				punctuation,
				documents,
			)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLateOnRuntimeCache(b *testing.B) {
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		b.Fatal(err)
	}

	b.Run("Cold", func(b *testing.B) {
		cache := b.TempDir()
		b.Setenv("SEEK_CACHE_DIR", cache)
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			if err := os.RemoveAll(filepath.Join(cache, "reranker")); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, installed, err := ensureLateOnRuntime(bundle); err != nil || !installed {
				b.Fatalf("cold runtime: installed=%t err=%v", installed, err)
			}
		}
	})
	b.Run("Warm", func(b *testing.B) {
		b.Setenv("SEEK_CACHE_DIR", b.TempDir())
		if _, installed, err := ensureLateOnRuntime(bundle); err != nil || !installed {
			b.Fatalf("prepare runtime: installed=%t err=%v", installed, err)
		}
		b.ReportAllocs()
		for b.Loop() {
			if _, installed, err := ensureLateOnRuntime(bundle); err != nil || installed {
				b.Fatalf("warm runtime: installed=%t err=%v", installed, err)
			}
		}
	})
}

func lateOnBenchmarkDocuments(count int) []rerankDocument {
	documents := make([]rerankDocument, count)
	for i := range documents {
		text := fmt.Sprintf(
			"func parseRequestHandler%d(request Request) error {\n"+
				"    parsed, err := parseRequest(request)\n"+
				"    if err != nil { return err }\n"+
				"    return handleRequest(parsed)\n"+
				"}\n",
			i,
		)
		matchAt := strings.Index(text, "parseRequest")
		documents[i] = rerankDocument{
			Path:     fmt.Sprintf("cmd/server/handler_%02d.go", i),
			Language: "Go",
			Symbol:   fmt.Sprintf("function parseRequestHandler%d", i),
			Text:     text,
			matchAt:  matchAt,
			matchEnd: matchAt + len("parseRequest"),
		}
	}
	return documents
}

func lateOnBenchmarkTruncatedDocuments(count int) []rerankDocument {
	const match = "parseRequestHandler"
	before := strings.Repeat("prefixContextValue ", 55)
	after := strings.Repeat(" suffixContextValue", 55)
	text := before + match + after
	documents := make([]rerankDocument, count)
	for i := range documents {
		documents[i] = rerankDocument{
			Path: fmt.Sprintf(
				"platform/services/router/src/internal/request/handlers/component_%02d/parse_request_handler.go",
				i,
			),
			Language: "Go",
			Symbol:   "function parseRequestHandler",
			Text:     text,
			matchAt:  len(before),
			matchEnd: len(before) + len(match),
		}
	}
	return documents
}

func seekBenchmarkBinary(b *testing.B) string {
	b.Helper()
	path := os.Getenv(seekBenchmarkBinaryEnv)
	if path == "" {
		b.Skipf("%s is not set", seekBenchmarkBinaryEnv)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		b.Fatalf("benchmark binary %q is not executable: %v", path, err)
	}
	return path
}

func mustRunSeekBenchmark(
	b *testing.B,
	binary, cache, directory string,
	args []string,
) {
	b.Helper()
	if _, err := runSeekBenchmarkCommand(
		b.Context(),
		binary,
		cache,
		directory,
		args,
		true,
	); err != nil {
		b.Fatalf("seek %q: %v", args, err)
	}
}

func runSeekBenchmarkCommand(
	ctx context.Context,
	binary, cache, directory string,
	args []string,
	discardOutput bool,
) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = directory
	cmd.Env = append(
		os.Environ(),
		"NO_COLOR=1",
		"SEEK_CACHE_DIR="+cache,
	)
	if discardOutput {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		return nil, cmd.Run()
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.Bytes(), err
}

// removeProviderVerdicts deletes every recorded provider verdict under the cache,
// keeping the compiled models beside them. The next run therefore repeats the
// provider check without repeating the Core ML compile.
func removeProviderVerdicts(b *testing.B, cacheDir string) {
	b.Helper()
	root := filepath.Join(cacheDir, "reranker", "coreml")
	formats, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		b.Fatal(err)
	}
	for _, format := range formats {
		keys, err := os.ReadDir(filepath.Join(root, format.Name()))
		if err != nil {
			b.Fatal(err)
		}
		for _, key := range keys {
			verdict := filepath.Join(root, format.Name(), key.Name(), "provider-verdict")
			if err := os.Remove(verdict); err != nil && !os.IsNotExist(err) {
				b.Fatal(err)
			}
		}
	}
}
