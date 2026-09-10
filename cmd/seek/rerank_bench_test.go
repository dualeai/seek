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
	"strings"
	"testing"
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
	cache := filepath.Join(root, "cache")
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
	defaultArgs := []string{queryText, fixture}
	ineligibleArgs := []string{"--rerank", "content:parse request handler", fixture}
	eligibleArgs := []string{"--rerank", queryText, fixture}
	eligibleDisplayContextZeroArgs := []string{"--rerank", "-C", "0", queryText, fixture}
	// The first term matches one file. The second term matches its path only,
	// so the content-only relaxed query has one candidate and skips scoring.
	oneCandidateArgs := []string{"--rerank", "candidate0 candidate_00", fixture}

	// Build the index and check each public path before the timer starts.
	for _, args := range [][]string{defaultArgs, ineligibleArgs} {
		output, err := runSeekBenchmarkCommand(b.Context(), binary, cache, fixture, args, false)
		if err != nil || len(output) == 0 {
			b.Fatalf("warm seek %q: output=%q err=%v", args, output, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, "reranker")); !os.IsNotExist(err) {
		b.Fatalf("default or ineligible search initialized the re-ranker: %v", err)
	}
	probeArgs := append([]string{"--verbose"}, eligibleArgs...)
	output, err := runSeekBenchmarkCommand(
		b.Context(),
		binary,
		cache,
		fixture,
		probeArgs,
		false,
	)
	if err != nil || len(output) == 0 || bytes.Contains(output, []byte("Re-ranking failed")) {
		b.Fatalf("warm seek %q: output=%q err=%v", probeArgs, output, err)
	}
	oneCandidateProbeArgs := append([]string{"--verbose"}, oneCandidateArgs...)
	output, err = runSeekBenchmarkCommand(
		b.Context(),
		binary,
		cache,
		fixture,
		oneCandidateProbeArgs,
		false,
	)
	if err != nil || len(output) == 0 || !bytes.Contains(output, []byte("Re-ranking failed")) {
		b.Fatalf("warm seek %q: output=%q err=%v", oneCandidateProbeArgs, output, err)
	}

	b.Run("DefaultSearchWarm", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, cache, fixture, defaultArgs)
		}
	})
	b.Run("RerankIneligibleWarm", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, cache, fixture, ineligibleArgs)
		}
	})
	b.Run("RerankEligibleFirstUse", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			if err := os.RemoveAll(filepath.Join(cache, "reranker")); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			mustRunSeekBenchmark(b, binary, cache, fixture, eligibleArgs)
		}
	})
	b.Run("RerankEligibleWarm", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, cache, fixture, eligibleArgs)
		}
	})
	b.Run("RerankEligibleWarmDisplayContextZero", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, cache, fixture, eligibleDisplayContextZeroArgs)
		}
	})
	b.Run("RerankEligibleOneCandidateWarm", func(b *testing.B) {
		for b.Loop() {
			mustRunSeekBenchmark(b, binary, cache, fixture, oneCandidateArgs)
		}
	})
}

func BenchmarkLateOnScorerStartup_WarmRuntime(b *testing.B) {
	b.Setenv("SEEK_CACHE_DIR", b.TempDir())
	warm, err := newLateOnRerankScorer(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	if err := warm.Close(); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		scorer, err := newLateOnRerankScorer(b.Context())
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
	b.Setenv("SEEK_CACHE_DIR", b.TempDir())
	scorer, err := newLateOnRerankScorer(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := scorer.Close(); err != nil {
			b.Errorf("close scorer: %v", err)
		}
	})

	documents := lateOnBenchmarkDocuments()
	const modelQuery = "find the request parser and handler"
	scores, err := scorer.Scores(b.Context(), modelQuery, documents)
	if err != nil || len(scores) != len(documents) {
		b.Fatalf("warm scorer: scores=%d err=%v", len(scores), err)
	}
	benchmarkLateOnScores = scores

	b.ReportAllocs()
	for b.Loop() {
		benchmarkLateOnScores, err = scorer.Scores(
			b.Context(),
			modelQuery,
			documents,
		)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLateOnTokenization_Top20Truncated(b *testing.B) {
	tokenizerJSON, err := decodeLateOnAsset(lateOnCompressedTokenizer)
	if err != nil {
		b.Fatal(err)
	}
	tokenizer, punctuation, err := newLateOnTokenizer(tokenizerJSON)
	if err != nil {
		b.Fatal(err)
	}
	documents := lateOnBenchmarkTruncatedDocuments()
	serialized, _, _, _ := serializeLateOnDocumentWithMatch(documents[0])
	if encoded := tokenizer.EncodeWithAnnotations(lateOnDocumentPrefix + serialized); len(encoded.IDs) <= lateOnSequenceLength {
		b.Fatalf("benchmark document has %d tokens; want more than %d", len(encoded.IDs), lateOnSequenceLength)
	}

	benchmarkLateOnInputIDs, benchmarkLateOnAttention, benchmarkLateOnScoreMask =
		tokenizeLateOnBatch(
			tokenizer,
			punctuation,
			"find the request parser and handler",
			documents,
		)
	b.SetBytes(int64(len(documents) * len(documents[0].Text)))
	b.ReportAllocs()
	for b.Loop() {
		benchmarkLateOnInputIDs, benchmarkLateOnAttention, benchmarkLateOnScoreMask =
			tokenizeLateOnBatch(
				tokenizer,
				punctuation,
				"find the request parser and handler",
				documents,
			)
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

func lateOnBenchmarkDocuments() []rerankDocument {
	documents := make([]rerankDocument, rerankCandidateLimit)
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

func lateOnBenchmarkTruncatedDocuments() []rerankDocument {
	const match = "parseRequestHandler"
	before := strings.Repeat("prefixContextValue ", 55)
	after := strings.Repeat(" suffixContextValue", 55)
	text := before + match + after
	documents := make([]rerankDocument, rerankCandidateLimit)
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
