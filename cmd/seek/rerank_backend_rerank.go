//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/gomlx/go-huggingface/tokenizers/api"
	"github.com/gomlx/go-huggingface/tokenizers/hftokenizer"
	"github.com/klauspost/compress/zstd"
	// TODO(https://github.com/microsoft/onnxruntime/issues/32535): Migrate to
	// the official Go binding and refactor native runtime packaging after
	// upstream publishes a tagged module and documents the release mapping.
	ort "github.com/yalue/onnxruntime_go"
)

const (
	lateOnSequenceLength   = 128
	lateOnEmbeddingSize    = 48
	lateOnQueryPrefix      = "[Q] "
	lateOnDocumentPrefix   = "[D] "
	lateOnQueryPrefixID    = 50_368
	lateOnDocumentPrefixID = 50_369
	lateOnPadTokenID       = 50_284
	lateOnCLSTokenID       = 50_281
	lateOnSEPTokenID       = 50_282
	lateOnX64PrecisionKey  = "session.x64quantprecision"
)

//go:embed rerank_assets/model_int8.onnx.zst
var lateOnCompressedModel []byte

//go:embed rerank_assets/tokenizer.json.zst
var lateOnCompressedTokenizer []byte

type lateOnRuntimeBundle struct {
	fileName   string
	version    string
	compressed []byte
	bytes      int64
	sha256     string
}

type lateOnRuntimeManifest struct {
	Target   string `json:"target"`
	FileName string `json:"file_name"`
	Version  string `json:"version"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

type lateOnScorer struct {
	session     *ort.DynamicAdvancedSession
	tokenizer   *hftokenizer.Tokenizer
	punctuation map[int]struct{}
}

var (
	lateOnEnvironmentOnce sync.Once
	lateOnEnvironmentErr  error
)

func lateOnRuntimeBundleFromManifest(
	manifestJSON []byte,
	compressed []byte,
) (lateOnRuntimeBundle, error) {
	var manifest lateOnRuntimeManifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return lateOnRuntimeBundle{}, fmt.Errorf("decode runtime manifest: %w", err)
	}
	target := runtime.GOOS + "-" + runtime.GOARCH
	if manifest.Target != target {
		return lateOnRuntimeBundle{}, fmt.Errorf(
			"runtime manifest target is %q, want %q",
			manifest.Target,
			target,
		)
	}
	wantFileName := "libonnxruntime.so"
	if runtime.GOOS == "darwin" {
		wantFileName = "libonnxruntime.dylib"
	}
	if manifest.FileName != wantFileName || manifest.Version == "" || manifest.Bytes <= 0 {
		return lateOnRuntimeBundle{}, fmt.Errorf("runtime manifest is incomplete")
	}
	digest, err := hex.DecodeString(manifest.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return lateOnRuntimeBundle{}, fmt.Errorf("runtime manifest has an invalid SHA-256")
	}
	if len(compressed) == 0 {
		return lateOnRuntimeBundle{}, fmt.Errorf("compressed runtime is empty")
	}
	return lateOnRuntimeBundle{
		fileName:   manifest.FileName,
		version:    manifest.Version,
		compressed: compressed,
		bytes:      manifest.Bytes,
		sha256:     manifest.SHA256,
	}, nil
}

func rerankBackendBundled() bool {
	_, err := lateOnRuntimeForPlatform()
	return err == nil
}

func newLateOnRerankScorer(ctx context.Context) (rerankScorer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		return nil, err
	}
	runtimePath, _, err := ensureLateOnRuntime(bundle)
	if err != nil {
		return nil, fmt.Errorf("prepare ONNX Runtime: %w", err)
	}
	model, err := decodeLateOnAsset(lateOnCompressedModel)
	if err != nil {
		return nil, fmt.Errorf("load LateOn model: %w", err)
	}
	tokenizerJSON, err := decodeLateOnAsset(lateOnCompressedTokenizer)
	if err != nil {
		return nil, fmt.Errorf("load LateOn tokenizer: %w", err)
	}
	tokenizer, punctuation, err := newLateOnTokenizer(tokenizerJSON)
	if err != nil {
		return nil, err
	}

	if err := ensureLateOnEnvironment(runtimePath); err != nil {
		return nil, err
	}
	sessionOptions, err := newLateOnSessionOptions()
	if err != nil {
		return nil, err
	}
	defer func() { _ = sessionOptions.Destroy() }()
	session, err := ort.NewDynamicAdvancedSessionWithONNXData(
		model,
		[]string{"input_ids", "attention_mask"},
		[]string{"output"},
		sessionOptions,
	)
	if err != nil {
		return nil, fmt.Errorf("create LateOn session: %w", err)
	}
	return &lateOnScorer{
		session:     session,
		tokenizer:   tokenizer,
		punctuation: punctuation,
	}, nil
}

func newLateOnSessionOptions() (*ort.SessionOptions, error) {
	options, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("create ONNX Runtime session options: %w", err)
	}
	if err := options.SetGraphOptimizationLevel(ort.GraphOptimizationLevelEnableAll); err != nil {
		_ = options.Destroy()
		return nil, fmt.Errorf("enable ONNX Runtime graph optimizations: %w", err)
	}
	// ONNX Runtime can overflow during x64 U8S8 matrix multiplication. At the
	// maximum graph optimization level set above, this setting converts S8
	// weights to U8 on affected AVX2 and AVX512 paths, so ONNX Runtime uses its
	// slower U8U8 path. It does not cover SSE4.1-only paths.
	if err := options.AddSessionConfigEntry(lateOnX64PrecisionKey, "1"); err != nil {
		_ = options.Destroy()
		return nil, fmt.Errorf("enable precise x64 quantization: %w", err)
	}
	return options, nil
}

func ensureLateOnEnvironment(runtimePath string) error {
	lateOnEnvironmentOnce.Do(func() {
		ort.SetSharedLibraryPath(runtimePath)
		if err := ort.InitializeEnvironment(); err != nil {
			lateOnEnvironmentErr = fmt.Errorf("initialize ONNX Runtime: %w", err)
			return
		}
		if err := ort.DisableTelemetry(); err != nil {
			lateOnEnvironmentErr = fmt.Errorf("disable ONNX Runtime telemetry: %w", err)
		}
	})
	return lateOnEnvironmentErr
}

func (s *lateOnScorer) Scores(
	ctx context.Context,
	modelQuery string,
	documents []rerankDocument,
) ([]float32, error) {
	if s.session == nil || s.tokenizer == nil {
		return nil, errRerankUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(documents) == 0 {
		return nil, nil
	}

	inputIDs, attention, scoreMask := tokenizeLateOnBatch(
		s.tokenizer,
		s.punctuation,
		modelQuery,
		documents,
	)
	batchSize := len(documents) + 1
	shape := ort.NewShape(int64(batchSize), lateOnSequenceLength)
	idsTensor, err := ort.NewTensor(shape, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("create input IDs: %w", err)
	}
	defer func() { _ = idsTensor.Destroy() }()
	attentionTensor, err := ort.NewTensor(shape, attention)
	if err != nil {
		return nil, fmt.Errorf("create attention mask: %w", err)
	}
	defer func() { _ = attentionTensor.Destroy() }()

	outputShape := ort.NewShape(
		int64(batchSize),
		lateOnSequenceLength,
		lateOnEmbeddingSize,
	)
	outputTensor, err := ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		return nil, fmt.Errorf("create LateOn output: %w", err)
	}
	defer func() { _ = outputTensor.Destroy() }()
	if err := s.session.Run(
		[]ort.Value{idsTensor, attentionTensor},
		[]ort.Value{outputTensor},
	); err != nil {
		return nil, fmt.Errorf("run LateOn model: %w", err)
	}
	scores, err := lateOnMaxSim(
		outputTensor.GetData(),
		scoreMask,
		batchSize,
		lateOnSequenceLength,
		lateOnEmbeddingSize,
	)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return scores, nil
}

func (s *lateOnScorer) Close() error {
	var closeErr error
	if s.session != nil {
		closeErr = s.session.Destroy()
		s.session = nil
	}
	// Keep the shared environment loaded for the process lifetime. Unloading the
	// ONNX library after inference can race with its native thread cleanup.
	return closeErr
}

func newLateOnTokenizer(
	content []byte,
) (*hftokenizer.Tokenizer, map[int]struct{}, error) {
	tokenizer, err := hftokenizer.NewFromContent(nil, content)
	if err != nil {
		return nil, nil, fmt.Errorf("parse LateOn tokenizer: %w", err)
	}
	if err := tokenizer.With(api.EncodeOptions{
		AddSpecialTokens: true,
		IncludeSpans:     true,
	}); err != nil {
		return nil, nil, fmt.Errorf("configure LateOn tokenizer: %w", err)
	}
	for _, special := range []struct {
		token string
		want  int
	}{
		{token: lateOnQueryPrefix, want: lateOnQueryPrefixID},
		{token: lateOnDocumentPrefix, want: lateOnDocumentPrefixID},
		{token: "[MASK]", want: lateOnPadTokenID},
		{token: "[CLS]", want: lateOnCLSTokenID},
		{token: "[SEP]", want: lateOnSEPTokenID},
	} {
		token, want := special.token, special.want
		got, ok := tokenizer.TokenToID(token)
		if !ok || got != want {
			return nil, nil, fmt.Errorf("LateOn token %q has ID %d, want %d", token, got, want)
		}
	}
	punctuation := make(map[int]struct{})
	for _, char := range `!"#$%&'()*+,-./:;<=>?@[\]^_` + "`" + `{|}~` {
		if id, ok := tokenizer.TokenToID(string(char)); ok {
			punctuation[id] = struct{}{}
		}
	}
	return tokenizer, punctuation, nil
}

func tokenizeLateOnBatch(
	tokenizer *hftokenizer.Tokenizer,
	punctuation map[int]struct{},
	queryText string,
	documents []rerankDocument,
) ([]int64, []int64, [][]bool) {
	rows := len(documents) + 1
	inputIDs := make([]int64, rows*lateOnSequenceLength)
	attention := make([]int64, len(inputIDs))
	scoreMask := make([][]bool, rows)
	for row := range rows {
		var encoded []int
		if row == 0 {
			encoded = encodeLateOnText(
				tokenizer,
				lateOnQueryPrefix+queryText,
			)
		} else {
			encoded = encodeLateOnDocument(
				tokenizer,
				documents[row-1],
				lateOnSequenceLength,
			)
		}
		scoreMask[row] = make([]bool, lateOnSequenceLength)
		for column := range lateOnSequenceLength {
			position := row*lateOnSequenceLength + column
			inputIDs[position] = lateOnPadTokenID
			if column >= len(encoded) {
				continue
			}
			id := encoded[column]
			inputIDs[position] = int64(id)
			attention[position] = 1
			_, isPunctuation := punctuation[id]
			scoreMask[row][column] = row == 0 || !isPunctuation
		}
	}
	return inputIDs, attention, scoreMask
}

func encodeLateOnDocument(
	tokenizer *hftokenizer.Tokenizer,
	document rerankDocument,
	limit int,
) []int {
	serialized, matchAt, matchEnd := serializeLateOnDocumentWithMatch(document)
	text := lateOnDocumentPrefix + serialized
	prefixBytes := len(lateOnDocumentPrefix)
	encoded := tokenizer.EncodeWithAnnotations(text)
	return truncateLateOnDocumentTokens(
		encoded.IDs,
		encoded.Spans,
		prefixBytes+matchAt,
		prefixBytes+matchEnd,
		limit,
	)
}

func truncateLateOnDocumentTokens(
	ids []int,
	spans []api.TokenSpan,
	matchAt int,
	matchEnd int,
	limit int,
) []int {
	if len(ids) <= limit {
		return ids
	}
	if limit <= 0 || len(spans) != len(ids) || matchEnd <= matchAt {
		return truncateLateOnHead(ids, limit)
	}

	prefixEnd := 0
	for i, id := range ids {
		if id == lateOnDocumentPrefixID {
			prefixEnd = i + 1
			break
		}
	}
	contentEnd := len(ids)
	if contentEnd > 0 && ids[contentEnd-1] == lateOnSEPTokenID {
		contentEnd--
	}
	contentBudget := limit - prefixEnd - 1
	if prefixEnd == 0 || prefixEnd >= contentEnd || contentBudget <= 0 {
		return truncateLateOnHead(ids, limit)
	}

	matchFirst, matchLast := -1, -1
	for i := prefixEnd; i < contentEnd; i++ {
		span := spans[i]
		if span.End > matchAt && span.Start < matchEnd {
			if matchFirst < 0 {
				matchFirst = i
			}
			matchLast = i
		}
	}
	if matchFirst < 0 {
		return truncateLateOnHead(ids, limit)
	}

	windowStart := matchFirst
	matchTokens := matchLast - matchFirst + 1
	if matchTokens < contentBudget {
		windowStart = max(prefixEnd, matchFirst-(contentBudget-matchTokens)/2)
	}
	windowEnd := min(contentEnd, windowStart+contentBudget)
	windowStart = max(prefixEnd, windowEnd-contentBudget)

	out := make([]int, 0, limit)
	out = append(out, ids[:prefixEnd]...)
	out = append(out, ids[windowStart:windowEnd]...)
	out = append(out, lateOnSEPTokenID)
	return out
}

func encodeLateOnText(
	tokenizer *hftokenizer.Tokenizer,
	text string,
) []int {
	encoded := tokenizer.Encode(text)
	return truncateLateOnHead(encoded, lateOnSequenceLength)
}

func truncateLateOnHead(encoded []int, limit int) []int {
	if len(encoded) <= limit {
		return encoded
	}
	if limit <= 0 {
		return nil
	}
	encoded = append([]int(nil), encoded[:limit]...)
	encoded[len(encoded)-1] = lateOnSEPTokenID
	return encoded
}

func lateOnMaxSim(
	embeddings []float32,
	scoreMask [][]bool,
	batchSize int,
	sequenceLength int,
	embeddingSize int,
) ([]float32, error) {
	want := batchSize * sequenceLength * embeddingSize
	if len(embeddings) != want || len(scoreMask) != batchSize {
		return nil, fmt.Errorf("invalid LateOn tensor size")
	}
	for _, mask := range scoreMask {
		if len(mask) != sequenceLength {
			return nil, fmt.Errorf("invalid LateOn mask size")
		}
	}

	scores := make([]float32, batchSize-1)
	for document := 1; document < batchSize; document++ {
		var sum float32
		for queryToken := range sequenceLength {
			if !scoreMask[0][queryToken] {
				continue
			}
			maximum := float32(math.Inf(-1))
			queryOffset := queryToken * embeddingSize
			for documentToken := range sequenceLength {
				if !scoreMask[document][documentToken] {
					continue
				}
				documentOffset :=
					(document*sequenceLength + documentToken) * embeddingSize
				var dot float32
				for dimension := range embeddingSize {
					dot += embeddings[queryOffset+dimension] *
						embeddings[documentOffset+dimension]
				}
				if dot > maximum {
					maximum = dot
				}
			}
			if math.IsInf(float64(maximum), -1) {
				return nil, fmt.Errorf("LateOn document has no scoreable token")
			}
			sum += maximum
		}
		scores[document-1] = sum
	}
	return scores, nil
}

func decodeLateOnAsset(compressed []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, err
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("asset is empty")
	}
	return decoded, nil
}

func ensureLateOnRuntime(bundle lateOnRuntimeBundle) (string, bool, error) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return "", false, err
	}
	directory := filepath.Join(
		cacheRoot,
		"reranker",
		bundle.version,
		bundle.sha256,
	)
	path := filepath.Join(directory, bundle.fileName)
	if validFileDigest(path, bundle.bytes, bundle.sha256) {
		return path, false, nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", false, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", false, err
	}
	temporary, err := os.CreateTemp(directory, ".onnxruntime-*.tmp")
	if err != nil {
		return "", false, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return "", false, err
	}
	decoder, err := zstd.NewReader(
		bytes.NewReader(bundle.compressed),
		zstd.WithDecoderConcurrency(1),
	)
	if err != nil {
		_ = temporary.Close()
		return "", false, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(
		io.MultiWriter(temporary, hash),
		io.LimitReader(decoder, bundle.bytes+1),
	)
	decoder.Close()
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil {
		return "", false, errors.Join(copyErr, closeErr)
	}
	if written != bundle.bytes || hex.EncodeToString(hash.Sum(nil)) != bundle.sha256 {
		return "", false, fmt.Errorf("extracted runtime digest mismatch")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if validFileDigest(path, bundle.bytes, bundle.sha256) {
			return path, false, nil
		}
		return "", false, err
	}
	return path, true, nil
}

func validFileDigest(path string, wantBytes int64, wantSHA256 string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != wantBytes {
		return false
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false
	}
	return hex.EncodeToString(hash.Sum(nil)) == wantSHA256
}
