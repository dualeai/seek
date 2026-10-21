//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/daulet/tokenizers"
	"github.com/klauspost/compress/zstd"
	// TODO(https://github.com/microsoft/onnxruntime/issues/32535): Migrate to
	// the official Go binding and refactor native runtime packaging after
	// upstream publishes a tagged module and documents the release mapping.
	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/sync/errgroup"
)

const (
	// lateOnSequenceLength is Seek's fixed row width, not the upstream model
	// maximum. This lower limit bounds local inference time and memory.
	lateOnSequenceLength   = 128
	lateOnQueryPrefix      = "[Q] "
	lateOnDocumentPrefix   = "[D] "
	lateOnQueryPrefixID    = 50_368
	lateOnDocumentPrefixID = 50_369
	lateOnPadTokenID       = 50_284
	lateOnCLSTokenID       = 50_281
	lateOnSEPTokenID       = 50_282
)

//go:embed rerank_assets/model_fp16_static_b128.onnx.zst
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

// lateOnModel lazily creates one row encoder and ONNX session and borrows
// tokenizers from the process-wide pool. Active methods can run concurrently.
// The command closes the model only after all borrowers have finished.
type lateOnModel struct {
	punctuation map[int]struct{}
	encoderOnce sync.Once
	encoder     *lateOnStaticRowEncoder
	encoderErr  error
}

// lateOnTokenizerJSON is the decoded tokenizer asset, read once per process.
var lateOnTokenizerJSON = sync.OnceValues(func() ([]byte, error) {
	return decodeLateOnAsset(lateOnCompressedTokenizer)
})

// lateOnTokenizers is the process-wide LateOn tokenizer pool: the model and
// the index build's window split share it. A tokenizer takes tens of
// milliseconds to build, so a released one serves the next caller, and a
// caller that finds none free builds one instead of waiting.
var lateOnTokenizers lateOnTokenizerPool

// lateOnTokenizerPool holds the free tokenizers, and the punctuation IDs of
// the first tokenizer that it built.
type lateOnTokenizerPool struct {
	mu          sync.Mutex
	free        []*lateOnTokenizer
	punctuation map[int]struct{}
}

// take returns a free tokenizer, or builds one when none is free.
func (pool *lateOnTokenizerPool) take(ctx context.Context) (*lateOnTokenizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pool.mu.Lock()
	if last := len(pool.free) - 1; last >= 0 {
		tokenizer := pool.free[last]
		pool.free = pool.free[:last]
		pool.mu.Unlock()
		return tokenizer, nil
	}
	pool.mu.Unlock()
	content, err := lateOnTokenizerJSON()
	if err != nil {
		return nil, fmt.Errorf("load LateOn tokenizer: %w", err)
	}
	tokenizer, punctuation, err := newLateOnTokenizer(content)
	if err != nil {
		return nil, err
	}
	pool.mu.Lock()
	if pool.punctuation == nil {
		pool.punctuation = punctuation
	}
	pool.mu.Unlock()
	return tokenizer, nil
}

// release returns tokenizer to the pool for the next take.
func (pool *lateOnTokenizerPool) release(tokenizer *lateOnTokenizer) {
	if tokenizer == nil {
		return
	}
	pool.mu.Lock()
	pool.free = append(pool.free, tokenizer)
	pool.mu.Unlock()
}

// punctuationIDs returns the single-token punctuation IDs of the tokenizer.
func (pool *lateOnTokenizerPool) punctuationIDs(ctx context.Context) (map[int]struct{}, error) {
	tokenizer, err := pool.take(ctx)
	if err != nil {
		return nil, err
	}
	pool.release(tokenizer)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.punctuation, nil
}

type lateOnTokenizer struct {
	native *tokenizers.Tokenizer
}

type lateOnTokenSpan struct {
	Start int
	End   int
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

func newLateOnSemanticModel(ctx context.Context) (semanticModel, error) {
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
	punctuation, err := lateOnTokenizers.punctuationIDs(ctx)
	if err != nil {
		return nil, err
	}
	if err := ensureLateOnEnvironment(runtimePath); err != nil {
		return nil, err
	}
	return &lateOnModel{punctuation: punctuation}, nil
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

func (s *lateOnModel) takeTokenizer(ctx context.Context) (*lateOnTokenizer, error) {
	return lateOnTokenizers.take(ctx)
}

func (s *lateOnModel) releaseTokenizer(tokenizer *lateOnTokenizer) {
	lateOnTokenizers.release(tokenizer)
}

func runLateOnSessionRows(
	ctx context.Context,
	session *ort.DynamicAdvancedSession,
	inputIDs []int64,
	attention []int64,
	batchSize int,
	outputTensor *ort.Tensor[float32],
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	shape := ort.NewShape(int64(batchSize), lateOnSequenceLength)
	idsTensor, err := ort.NewTensor(shape, inputIDs)
	if err != nil {
		return fmt.Errorf("create input IDs: %w", err)
	}
	defer func() { _ = idsTensor.Destroy() }()
	attentionTensor, err := ort.NewTensor(shape, attention)
	if err != nil {
		return fmt.Errorf("create attention mask: %w", err)
	}
	defer func() { _ = attentionTensor.Destroy() }()
	runOptions, err := ort.NewRunOptions()
	if err != nil {
		return fmt.Errorf("create LateOn run options: %w", err)
	}
	defer func() { _ = runOptions.Destroy() }()
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = runOptions.Terminate()
		case <-watchStop:
		}
	}()
	runErr := session.RunWithOptions(
		[]ort.Value{idsTensor, attentionTensor},
		[]ort.Value{outputTensor},
		runOptions,
	)
	close(watchStop)
	<-watchDone
	if err := ctx.Err(); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("run LateOn model: %w", runErr)
	}
	return nil
}

func (s *lateOnModel) EmbedSemanticUnits(
	ctx context.Context,
	units []semanticUnit,
) ([]semanticUnitEmbedding, error) {
	if len(units) == 0 {
		return nil, ctx.Err()
	}
	if len(units) > semanticModelBatchRows {
		return nil, fmt.Errorf(
			"semantic model batch has %d rows, limit is %d",
			len(units),
			semanticModelBatchRows,
		)
	}
	encoder, err := s.semanticRowEncoder(ctx)
	if err != nil {
		return nil, err
	}
	return s.embedSemanticUnitBatch(ctx, encoder, units)
}

func (s *lateOnModel) SemanticCallCPUs(ctx context.Context) int {
	encoder, err := s.semanticRowEncoder(ctx)
	if err != nil {
		return 1
	}
	return max(0, encoder.CallCPUs())
}

func (s *lateOnModel) embedSemanticUnitBatch(
	ctx context.Context,
	encoder *lateOnStaticRowEncoder,
	units []semanticUnit,
) ([]semanticUnitEmbedding, error) {
	// A window row holds its token IDs; only a unit without them needs a
	// tokenizer.
	var tokenizer *lateOnTokenizer
	if slices.ContainsFunc(units, func(unit semanticUnit) bool { return len(unit.modelInput) == 0 }) {
		var err error
		if tokenizer, err = s.takeTokenizer(ctx); err != nil {
			return nil, err
		}
	}
	inputIDs, attention, masks, tokenizeErr := tokenizeLateOnSemanticUnits(
		tokenizer,
		s.punctuation,
		units,
	)
	if tokenizer != nil {
		s.releaseTokenizer(tokenizer)
	}
	if tokenizeErr != nil {
		return nil, tokenizeErr
	}
	vectors := make([]semanticUnitEmbedding, len(units))
	err := encoder.Run(ctx, inputIDs, attention, len(units), func(embeddings []float32) error {
		var workspace semanticCentroidWorkspace
		for row := range vectors {
			vector, rowErr := workspace.makeUnitEmbedding(embeddings, masks[row], row)
			if rowErr != nil {
				return fmt.Errorf("semantic unit %d: %w", units[row].row, rowErr)
			}
			vectors[row] = vector
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return vectors, nil
}

func (s *lateOnModel) PrepareSemanticQuery(
	ctx context.Context,
	modelQuery string,
) (*semanticQueryEmbedding, error) {
	tokenizer, err := s.takeTokenizer(ctx)
	if err != nil {
		return nil, err
	}
	windows, encodeErr := lateOnQueryWindows(tokenizer, modelQuery)
	s.releaseTokenizer(tokenizer)
	if encodeErr != nil {
		return nil, encodeErr
	}
	inputIDs, attention, masks := packLateOnRows(windows, nil, true)
	encoder, err := s.semanticRowEncoder(ctx)
	if err != nil {
		return nil, err
	}
	var embeddings []float32
	err = encoder.Run(ctx, inputIDs, attention, len(windows), func(output []float32) error {
		embeddings = append([]float32(nil), output...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	scoreMask := make([]bool, 0, len(masks)*lateOnSequenceLength)
	for window, mask := range masks {
		// The same provider fault that zeroes a document row can zero a query
		// row. A zero query gives every candidate a finite score of 0, which no
		// later guard can tell apart from a genuine miss.
		if !lateOnRowHasUsableToken(embeddings, window, mask) {
			return nil, fmt.Errorf(
				"%w: LateOn query has no usable token vector",
				errDegenerateSemanticVector,
			)
		}
		scoreMask = append(scoreMask, mask...)
	}
	return &semanticQueryEmbedding{
		tokens:     embeddings,
		scoreMask:  scoreMask,
		modelQuery: modelQuery,
	}, nil
}

func (s *lateOnModel) ScoresWithSemanticQuery(
	ctx context.Context,
	prepared *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	if !validSemanticQuery(prepared) {
		return nil, fmt.Errorf("semantic query embedding is invalid")
	}
	if len(documents) == 0 {
		return nil, nil
	}
	encoder, err := s.semanticRowEncoder(ctx)
	if err != nil {
		return nil, err
	}
	// One encoder call holds one static 128-row batch: the scorer runs
	// ceil(N/128) calls, at the same time up to the model call limit, so any
	// number of candidates gets a score.
	scores := make([]float32, len(documents))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(semanticModelCallLimit(runtime.GOMAXPROCS(0), max(0, encoder.CallCPUs())))
	for start := 0; start < len(documents); start += semanticModelBatchRows {
		batch := documents[start:min(start+semanticModelBatchRows, len(documents))]
		group.Go(func() error {
			values, err := s.scoreDocumentBatch(groupCtx, encoder, prepared, batch)
			if err == nil && len(values) != len(batch) {
				err = fmt.Errorf("semantic scorer returned %d scores for %d documents", len(values), len(batch))
			}
			copy(scores[start:], values)
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return scores, nil
}

// scoreDocumentBatch scores at most semanticModelBatchRows documents against
// the prepared query with one encoder call.
func (s *lateOnModel) scoreDocumentBatch(
	ctx context.Context,
	encoder *lateOnStaticRowEncoder,
	prepared *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	tokenizer, err := s.takeTokenizer(ctx)
	if err != nil {
		return nil, err
	}
	inputIDs, attention, masks, tokenizeErr := tokenizeLateOnDocuments(
		tokenizer,
		s.punctuation,
		documents,
	)
	s.releaseTokenizer(tokenizer)
	if tokenizeErr != nil {
		return nil, tokenizeErr
	}
	var scores []float32
	err = encoder.Run(ctx, inputIDs, attention, len(documents), func(embeddings []float32) error {
		var scoreErr error
		scores, scoreErr = lateOnMaxSimPrepared(prepared.tokens, prepared.scoreMask, embeddings, masks)
		return scoreErr
	})
	return scores, err
}

func (s *lateOnModel) semanticRowEncoder(ctx context.Context) (*lateOnStaticRowEncoder, error) {
	s.encoderOnce.Do(func() {
		s.encoder, s.encoderErr = newLateOnRowEncoder(ctx)
	})
	return s.encoder, s.encoderErr
}

func (s *lateOnModel) Close() error {
	var closeErr error
	if s.encoder != nil {
		closeErr = s.encoder.Close()
		s.encoder = nil
	}
	// Keep the process-wide tokenizers and the shared environment loaded for
	// the process lifetime. Unloading the ONNX library after inference can
	// race with its native thread cleanup.
	return closeErr
}

func newLateOnTokenizer(
	content []byte,
) (*lateOnTokenizer, map[int]struct{}, error) {
	// Serialized rerank input is at most 16 KiB. This larger tokenizer bound
	// keeps match-centered truncation under Seek's control.
	native, err := tokenizers.FromBytesWithTruncation(
		content,
		uint32(maxRerankDocumentBytes*4),
		tokenizers.TruncationDirectionRight,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("parse LateOn tokenizer: %w", err)
	}
	tokenizer := &lateOnTokenizer{native: native}
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
		ids, _, encodeErr := tokenizer.encode(token, false, false)
		if encodeErr != nil {
			_ = tokenizer.Close()
			return nil, nil, fmt.Errorf("encode LateOn token %q: %w", token, encodeErr)
		}
		if len(ids) != 1 || ids[0] != want {
			_ = tokenizer.Close()
			return nil, nil, fmt.Errorf("LateOn token %q has IDs %v, want [%d]", token, ids, want)
		}
	}
	punctuation := make(map[int]struct{})
	for _, char := range `!"#$%&'()*+,-./:;<=>?@[\]^_` + "`" + `{|}~` {
		ids, _, encodeErr := tokenizer.encode(string(char), false, false)
		if encodeErr != nil {
			_ = tokenizer.Close()
			return nil, nil, fmt.Errorf("encode LateOn punctuation %q: %w", char, encodeErr)
		}
		if len(ids) == 1 {
			punctuation[ids[0]] = struct{}{}
		}
	}
	return tokenizer, punctuation, nil
}

func (t *lateOnTokenizer) encode(
	text string,
	addSpecialTokens bool,
	includeOffsets bool,
) ([]int, []lateOnTokenSpan, error) {
	if t == nil || t.native == nil {
		return nil, nil, fmt.Errorf("LateOn tokenizer is closed")
	}
	text = sanitizeLateOnTokenizerInput(text)
	var options []tokenizers.EncodeOption
	if includeOffsets {
		options = append(options, tokenizers.WithReturnOffsets())
	}
	encoded, err := t.native.EncodeWithOptionsErr(text, addSpecialTokens, options...)
	if err != nil {
		return nil, nil, fmt.Errorf("encode LateOn input: %w", err)
	}
	ids := make([]int, len(encoded.IDs))
	for index, id := range encoded.IDs {
		ids[index] = int(id)
	}
	if !includeOffsets {
		return ids, nil, nil
	}
	if len(encoded.Offsets) != len(encoded.IDs) {
		return nil, nil, fmt.Errorf("LateOn tokenizer returned invalid offsets")
	}
	spans := make([]lateOnTokenSpan, len(encoded.Offsets))
	for index, offset := range encoded.Offsets {
		spans[index] = lateOnTokenSpan{Start: int(offset[0]), End: int(offset[1])}
	}
	return ids, spans, nil
}

func (t *lateOnTokenizer) Close() error {
	if t == nil || t.native == nil {
		return nil
	}
	err := t.native.Close()
	t.native = nil
	return err
}

func sanitizeLateOnTokenizerInput(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	return strings.ReplaceAll(text, "\x00", "\uFFFD")
}

func tokenizeLateOnSemanticUnits(
	tokenizer *lateOnTokenizer,
	punctuation map[int]struct{},
	units []semanticUnit,
) ([]int64, []int64, [][]bool, error) {
	encoded := make([][]int, len(units))
	for index, unit := range units {
		if len(unit.modelInput) > 0 {
			encoded[index] = unit.modelInput
			continue
		}
		var err error
		encoded[index], err = encodeLateOnDocument(
			tokenizer,
			lateOnSemanticDocument(unit),
			lateOnSequenceLength,
		)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	inputIDs, attention, masks := packLateOnRows(encoded, punctuation, false)
	return inputIDs, attention, masks, nil
}

// lateOnSemanticDocument keeps file context but does not repeat ctags symbol
// metadata in the proxy input. A symbol unit starts at its source definition,
// so its name and signature are already present. The stored symbol remains in
// the checked document used by final MaxSim scoring.
func lateOnSemanticDocument(unit semanticUnit) rerankDocument {
	text := strings.ToValidUTF8(string(unit.text), "\uFFFD")
	return rerankDocument{
		Path:     unit.path,
		Language: unit.language,
		Text:     text,
		matchEnd: len(text),
	}
}

func tokenizeLateOnDocuments(
	tokenizer *lateOnTokenizer,
	punctuation map[int]struct{},
	documents []rerankDocument,
) ([]int64, []int64, [][]bool, error) {
	encoded := make([][]int, len(documents))
	for index, document := range documents {
		var err error
		encoded[index], err = encodeLateOnDocument(
			tokenizer,
			document,
			lateOnSequenceLength,
		)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	inputIDs, attention, masks := packLateOnRows(encoded, punctuation, false)
	return inputIDs, attention, masks, nil
}

func packLateOnRows(
	encoded [][]int,
	punctuation map[int]struct{},
	query bool,
) ([]int64, []int64, [][]bool) {
	inputIDs := make([]int64, len(encoded)*lateOnSequenceLength)
	attention := make([]int64, len(inputIDs))
	masks := make([][]bool, len(encoded))
	for row, ids := range encoded {
		masks[row] = make([]bool, lateOnSequenceLength)
		for column := range lateOnSequenceLength {
			position := row*lateOnSequenceLength + column
			inputIDs[position] = lateOnPadTokenID
			if column >= len(ids) {
				continue
			}
			id := ids[column]
			inputIDs[position] = int64(id)
			attention[position] = 1
			_, isPunctuation := punctuation[id]
			masks[row][column] = query || !isPunctuation
		}
	}
	return inputIDs, attention, masks
}

func lateOnMaxSimPrepared(
	query []float32,
	queryMask []bool,
	documents []float32,
	documentMasks [][]bool,
) ([]float32, error) {
	queryRowBytes := lateOnSequenceLength * semanticEmbeddingDimensions
	if len(queryMask) == 0 || len(queryMask)%lateOnSequenceLength != 0 ||
		len(query) != len(queryMask)*semanticEmbeddingDimensions ||
		len(documents) != len(documentMasks)*queryRowBytes {
		return nil, fmt.Errorf("invalid prepared LateOn tensor size")
	}
	scores := make([]float32, len(documentMasks))
	for document, mask := range documentMasks {
		if len(mask) != lateOnSequenceLength {
			return nil, fmt.Errorf("invalid prepared LateOn mask size")
		}
		if !lateOnRowHasUsableToken(documents, document, mask) {
			return nil, fmt.Errorf(
				"%w: LateOn document %d has no usable token vector",
				errDegenerateSemanticVector,
				document,
			)
		}
		var sum float32
		for queryToken, keepQuery := range queryMask {
			if !keepQuery {
				continue
			}
			maximum := float32(math.Inf(-1))
			queryOffset := queryToken * semanticEmbeddingDimensions
			for documentToken, keepDocument := range mask {
				if !keepDocument {
					continue
				}
				documentOffset := (document*lateOnSequenceLength + documentToken) * semanticEmbeddingDimensions
				var dot float32
				for dimension := range semanticEmbeddingDimensions {
					dot += query[queryOffset+dimension] * documents[documentOffset+dimension]
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
		if math.IsNaN(float64(sum)) || math.IsInf(float64(sum), 0) {
			return nil, fmt.Errorf("LateOn score is not finite")
		}
		scores[document] = sum
	}
	return scores, nil
}

// lateOnRowHasUsableToken reports whether a row (a document or a query window)
// keeps one masked token with a finite, non-zero vector. A provider fault can return a zero vector. That
// vector contributes a dot product of zero, which keeps the score finite and
// hides the fault, so the existing "no scoreable token" guard never sees it.
func lateOnRowHasUsableToken(documents []float32, document int, mask []bool) bool {
	for documentToken, keepDocument := range mask {
		if !keepDocument {
			continue
		}
		offset := (document*lateOnSequenceLength + documentToken) * semanticEmbeddingDimensions
		var squaredNorm float64
		finite := true
		for dimension := range semanticEmbeddingDimensions {
			value := float64(documents[offset+dimension])
			if math.IsNaN(value) || math.IsInf(value, 0) {
				// Keep looking. A later token can still carry the row, which is
				// what the scoring loop of lateOnMaxSimPrepared relies on: a
				// comparison against a non-finite value is false, so such a token
				// never wins the maximum.
				finite = false
				break
			}
			squaredNorm += value * value
		}
		if finite && squaredNorm > 0 {
			return true
		}
	}
	return false
}

func encodeLateOnDocument(
	tokenizer *lateOnTokenizer,
	document rerankDocument,
	limit int,
) ([]int, error) {
	serialized, metadataEnd, matchAt, matchEnd := serializeLateOnDocumentWithMatch(document)
	serialized, metadataEnd, matchAt, matchEnd = sanitizeLateOnDocumentInput(
		serialized,
		metadataEnd,
		matchAt,
		matchEnd,
	)
	text := lateOnDocumentPrefix + serialized
	prefixBytes := len(lateOnDocumentPrefix)
	ids, spans, err := tokenizer.encode(text, true, true)
	if err != nil {
		return nil, err
	}
	return truncateLateOnDocumentTokens(
		ids,
		spans,
		prefixBytes+metadataEnd,
		prefixBytes+matchAt,
		prefixBytes+matchEnd,
		limit,
	), nil
}

func sanitizeLateOnDocumentInput(
	text string,
	metadataEnd int,
	matchAt int,
	matchEnd int,
) (string, int, int, int) {
	if metadataEnd < 0 || metadataEnd > matchAt || matchAt > matchEnd || matchEnd > len(text) {
		sanitized := sanitizeLateOnTokenizerInput(text)
		return sanitized, 0, 0, len(sanitized)
	}
	metadata := sanitizeLateOnTokenizerInput(text[:metadataEnd])
	beforeMatch := sanitizeLateOnTokenizerInput(text[metadataEnd:matchAt])
	match := sanitizeLateOnTokenizerInput(text[matchAt:matchEnd])
	afterMatch := sanitizeLateOnTokenizerInput(text[matchEnd:])
	sanitizedMetadataEnd := len(metadata)
	sanitizedMatchAt := sanitizedMetadataEnd + len(beforeMatch)
	sanitizedMatchEnd := sanitizedMatchAt + len(match)
	return metadata + beforeMatch + match + afterMatch,
		sanitizedMetadataEnd,
		sanitizedMatchAt,
		sanitizedMatchEnd
}

// truncateLateOnDocumentTokens packs one annotated document into limit tokens.
// The byte offsets refer to the prefixed serialized text. It keeps the model
// prefix and separator, uses at most one quarter of the content slots for
// metadata, and centers the remaining evidence on the match. An invalid
// annotated layout uses head truncation.
func truncateLateOnDocumentTokens(
	ids []int,
	spans []lateOnTokenSpan,
	metadataEnd int,
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
	if ids[contentEnd-1] == lateOnSEPTokenID {
		contentEnd--
	}
	contentBudget := limit - prefixEnd - 1
	if prefixEnd == 0 || contentBudget <= 0 {
		return truncateLateOnHead(ids, limit)
	}
	metadataTokenEnd := prefixEnd
	for metadataTokenEnd < contentEnd && spans[metadataTokenEnd].Start < metadataEnd {
		metadataTokenEnd++
	}
	metadataBudget := min(metadataTokenEnd-prefixEnd, contentBudget/4)
	bodyStart := metadataTokenEnd
	evidenceBudget := contentBudget - metadataBudget

	matchFirst, matchLast := -1, -1
	for i := bodyStart; i < contentEnd; i++ {
		span := spans[i]
		if span.End > matchAt && span.Start < matchEnd {
			if matchFirst < 0 {
				matchFirst = i
			}
			matchLast = i
		}
	}
	if matchFirst < 0 {
		windowEnd := min(contentEnd, bodyStart+evidenceBudget)
		out := make([]int, 0, limit)
		out = append(out, ids[:prefixEnd]...)
		out = append(out, ids[prefixEnd:prefixEnd+metadataBudget]...)
		out = append(out, ids[bodyStart:windowEnd]...)
		out = append(out, lateOnSEPTokenID)
		return out
	}

	windowStart := matchFirst
	matchTokens := matchLast - matchFirst + 1
	if matchTokens < evidenceBudget {
		windowStart = max(bodyStart, matchFirst-(evidenceBudget-matchTokens)/2)
	}
	windowEnd := min(contentEnd, windowStart+evidenceBudget)
	windowStart = max(bodyStart, windowEnd-evidenceBudget)

	out := make([]int, 0, limit)
	out = append(out, ids[:prefixEnd]...)
	out = append(out, ids[prefixEnd:prefixEnd+metadataBudget]...)
	out = append(out, ids[windowStart:windowEnd]...)
	out = append(out, lateOnSEPTokenID)
	return out
}

func encodeLateOnText(
	tokenizer *lateOnTokenizer,
	text string,
) ([]int, error) {
	encoded, _, err := tokenizer.encode(text, true, false)
	if err != nil {
		return nil, err
	}
	return truncateLateOnHead(encoded, lateOnSequenceLength), nil
}

// lateOnQueryWindows splits a query into 128-token windows: each window holds
// CLS, the query prefix, a part of the query tokens, and SEP, so a long query
// keeps every token. A query that fits gives one window.
func lateOnQueryWindows(tokenizer *lateOnTokenizer, modelQuery string) ([][]int, error) {
	encoded, _, err := tokenizer.encode(lateOnQueryPrefix+modelQuery, true, false)
	if err != nil {
		return nil, err
	}
	if len(encoded) <= lateOnSequenceLength {
		return [][]int{encoded}, nil
	}
	head := slices.Index(encoded, lateOnQueryPrefixID) + 1
	if head <= 0 || encoded[len(encoded)-1] != lateOnSEPTokenID || head >= len(encoded)-1 {
		return nil, fmt.Errorf("LateOn query tokens have no prefix or end")
	}
	body := encoded[head : len(encoded)-1]
	room := lateOnSequenceLength - head - 1
	var windows [][]int
	for start := 0; start < len(body); start += room {
		window := make([]int, 0, lateOnSequenceLength)
		window = append(window, encoded[:head]...)
		window = append(window, body[start:min(start+room, len(body))]...)
		windows = append(windows, append(window, lateOnSEPTokenID))
	}
	return windows, nil
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
	return installPrivateAsset(
		directory,
		bundle.fileName,
		".onnxruntime-*.tmp",
		func(path string) bool {
			return validFileDigest(path, bundle.bytes, bundle.sha256)
		},
		func(writer io.Writer) error {
			decoder, err := zstd.NewReader(
				bytes.NewReader(bundle.compressed),
				zstd.WithDecoderConcurrency(1),
			)
			if err != nil {
				return err
			}
			defer decoder.Close()
			hash := sha256.New()
			written, err := io.Copy(
				io.MultiWriter(writer, hash),
				io.LimitReader(decoder, bundle.bytes+1),
			)
			if err != nil {
				return err
			}
			if written != bundle.bytes || hex.EncodeToString(hash.Sum(nil)) != bundle.sha256 {
				return fmt.Errorf("extracted runtime digest mismatch")
			}
			return nil
		},
	)
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
