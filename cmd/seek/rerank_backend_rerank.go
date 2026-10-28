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
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

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

// lateOnRuntimeBundle is the ONNX Runtime library that this target embeds:
// the fields of its manifest (rerank_backend_assets_<target>.go), and the
// compressed library.
type lateOnRuntimeBundle struct {
	Target     string `json:"target"`
	FileName   string `json:"file_name"`
	Version    string `json:"version"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	compressed []byte
}

// lateOnModel lazily creates its row encoders, each with one ONNX session,
// and borrows tokenizers from the process-wide pool. The first call makes the
// first encoder; an index build can add a second CPU encoder
// (semanticRowEncoder). Active methods can run concurrently. The command
// closes the model only after all borrowers have finished.
type lateOnModel struct {
	punctuation map[int]struct{}
	encoderOnce sync.Once
	encoder     *lateOnStaticRowEncoder
	encoderErr  error
	// buildOnce and build are a second CPU session for the index build, made
	// only when the first one is sized for one call (semanticRowEncoder).
	buildOnce sync.Once
	build     *lateOnStaticRowEncoder
	buildErr  error
	// closed makes every encoder request (semanticRowEncoder) after Close
	// return errLateOnModelClosed.
	closed atomic.Bool
}

// lateOnTokenizerJSON is the decoded tokenizer asset, read once per process.
var lateOnTokenizerJSON = sync.OnceValues(func() ([]byte, error) {
	return decodeLateOnAsset(lateOnCompressedTokenizer)
})

// lateOnTokenizers is the process-wide LateOn tokenizer pool: the model and
// the index build's window split share it. A tokenizer takes tens of
// milliseconds to build, so a released one serves the next caller. A caller
// that finds none free builds one and does not wait.
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

// lateOnRuntimeForPlatform gives the ONNX Runtime bundle that this target
// embeds (rerank_backend_assets_<target>.go).
func lateOnRuntimeForPlatform() (lateOnRuntimeBundle, error) {
	return lateOnRuntimeBundleFromManifest(lateOnRuntimeManifestJSON, lateOnCompressedRuntime)
}

// lateOnRuntimeBundleFromManifest decodes the manifest of the embedded
// runtime. It checks that the manifest names this target and its library file,
// with a version, a size and a SHA-256.
func lateOnRuntimeBundleFromManifest(
	manifestJSON []byte,
	compressed []byte,
) (lateOnRuntimeBundle, error) {
	var bundle lateOnRuntimeBundle
	if err := json.Unmarshal(manifestJSON, &bundle); err != nil {
		return lateOnRuntimeBundle{}, fmt.Errorf("decode runtime manifest: %w", err)
	}
	target := runtime.GOOS + "-" + runtime.GOARCH
	if bundle.Target != target {
		return lateOnRuntimeBundle{}, fmt.Errorf(
			"runtime manifest target is %q, want %q",
			bundle.Target,
			target,
		)
	}
	wantFileName := "libonnxruntime.so"
	if runtime.GOOS == "darwin" {
		wantFileName = "libonnxruntime.dylib"
	}
	if bundle.FileName != wantFileName || bundle.Version == "" || bundle.Bytes <= 0 {
		return lateOnRuntimeBundle{}, fmt.Errorf("runtime manifest is incomplete")
	}
	if !validSHA256Hex(bundle.SHA256) {
		return lateOnRuntimeBundle{}, fmt.Errorf("runtime manifest has an invalid SHA-256")
	}
	if len(compressed) == 0 {
		return lateOnRuntimeBundle{}, fmt.Errorf("compressed runtime is empty")
	}
	bundle.compressed = compressed
	return bundle, nil
}

// newLateOnSemanticModel makes the model. Three steps before its first query do
// not depend on each other, so they overlap. The runtime check reads and
// hashes the installed library. The tokenizer build parses its asset, and the
// model asset decodes for the first encoder. On warm searches of the Seek
// repository, the overlap took 40 to 55 ms off searches of 500 to 590 ms. A
// runtime error still comes before a tokenizer error.
func newLateOnSemanticModel(ctx context.Context) (semanticModel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bundle, err := lateOnRuntimeForPlatform()
	if err != nil {
		return nil, err
	}
	go func() { _, _ = lateOnModelBytes() }()
	var runtimePath string
	var runtimeErr error
	installed := make(chan struct{})
	go func() {
		runtimePath, runtimeErr = ensureLateOnRuntime(bundle)
		close(installed)
	}()
	punctuation, punctuationErr := lateOnTokenizers.punctuationIDs(ctx)
	<-installed
	if runtimeErr != nil {
		return nil, fmt.Errorf("prepare ONNX Runtime: %w", runtimeErr)
	}
	if punctuationErr != nil {
		return nil, punctuationErr
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

// ensureLateOnEnvironment loads the ONNX Runtime library at runtimePath and
// turns off its telemetry, once per process. Each later call returns the
// result of the first.
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

// runLateOnSessionRows runs one static batch of semanticModelBatchRows rows
// through session: the model has that fixed batch size.
func runLateOnSessionRows(
	ctx context.Context,
	session *ort.DynamicAdvancedSession,
	inputIDs []int64,
	attention []int64,
	outputTensor *ort.Tensor[float32],
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	shape := ort.NewShape(semanticModelBatchRows, lateOnSequenceLength)
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
	// A cancel stops the run. A Terminate that started must end before the
	// deferred Destroy.
	terminated := make(chan struct{})
	stopTerminate := context.AfterFunc(ctx, func() {
		defer close(terminated)
		_ = runOptions.Terminate()
	})
	runErr := session.RunWithOptions(
		[]ort.Value{idsTensor, attentionTensor},
		[]ort.Value{outputTensor},
		runOptions,
	)
	if !stopTerminate() {
		<-terminated
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("run LateOn model: %w", runErr)
	}
	return nil
}

// EmbedSemanticUnits embeds one build batch: 1 to semanticModelBatchRows units
// (sendBatch). Run rejects a batch of any other size.
func (s *lateOnModel) EmbedSemanticUnits(
	ctx context.Context,
	units []semanticUnit,
) ([]semanticUnitEmbedding, error) {
	encoder, err := s.semanticRowEncoder(ctx, false)
	if err != nil {
		return nil, err
	}
	return s.embedSemanticUnitBatch(ctx, encoder, units)
}

func (s *lateOnModel) SemanticCallCPUs(ctx context.Context) int {
	encoder, err := s.semanticRowEncoder(ctx, false)
	if err != nil {
		return 1
	}
	return encoder.CallCPUs()
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
		if tokenizer, err = lateOnTokenizers.take(ctx); err != nil {
			return nil, err
		}
	}
	inputIDs, attention, masks, tokenizeErr := tokenizeLateOnSemanticUnits(
		tokenizer,
		s.punctuation,
		units,
	)
	if tokenizer != nil {
		lateOnTokenizers.release(tokenizer)
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

// PrepareSemanticQuery encodes the query windows (lateOnQueryWindows) and the
// empty document in one model call. It gives the token vectors of the query,
// their score mask, and the score of the empty document (emptyScore).
func (s *lateOnModel) PrepareSemanticQuery(
	ctx context.Context,
	modelQuery string,
) (*semanticQueryEmbedding, error) {
	tokenizer, err := lateOnTokenizers.take(ctx)
	if err != nil {
		return nil, err
	}
	windows, encodeErr := lateOnQueryWindows(tokenizer, modelQuery)
	// The empty document goes through the scorer's own tokenizer path, so its
	// row is the row that ScoresWithSemanticQuery encodes for it.
	var emptyIDs, emptyAttention []int64
	var emptyMasks [][]bool
	if encodeErr == nil {
		emptyIDs, emptyAttention, emptyMasks, encodeErr = tokenizeLateOnDocuments(
			tokenizer,
			s.punctuation,
			[]rerankDocument{{}},
		)
	}
	lateOnTokenizers.release(tokenizer)
	if encodeErr != nil {
		return nil, encodeErr
	}
	// A query keeps its punctuation tokens: no punctuation set.
	inputIDs, attention, masks := packLateOnRows(windows, nil)
	// The static batch always runs semanticModelBatchRows rows, so the empty
	// document in the row after the last window costs no model call.
	inputIDs = append(inputIDs, emptyIDs...)
	attention = append(attention, emptyAttention...)
	scoreMask := make([]bool, 0, len(masks)*lateOnSequenceLength)
	for _, mask := range masks {
		scoreMask = append(scoreMask, mask...)
	}
	encoder, err := s.semanticRowEncoder(ctx, true)
	if err != nil {
		return nil, err
	}
	var tokens []float32
	var emptyScore float32
	err = encoder.Run(ctx, inputIDs, attention, len(windows)+1, func(output []float32) error {
		queryValues := len(windows) * lateOnSequenceLength * semanticEmbeddingDimensions
		tokens = append([]float32(nil), output[:queryValues]...)
		for window, mask := range masks {
			// The same provider fault that zeroes a document row can zero a
			// query row. A zero query gives every candidate a finite score of
			// 0, which no later guard can tell apart from a genuine miss.
			if !lateOnRowHasUsableToken(tokens, window, mask) {
				return fmt.Errorf(
					"%w: LateOn query has no usable token vector",
					errDegenerateSemanticVector,
				)
			}
		}
		scores, scoreErr := lateOnMaxSimPrepared(tokens, scoreMask, output[queryValues:], emptyMasks)
		if scoreErr != nil {
			return fmt.Errorf("score the empty LateOn document: %w", scoreErr)
		}
		emptyScore = scores[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &semanticQueryEmbedding{
		tokens:     tokens,
		scoreMask:  scoreMask,
		emptyScore: emptyScore,
	}, nil
}

// ScoresWithSemanticQuery gives the MaxSim score of each document against the
// prepared query, in the order of documents.
func (s *lateOnModel) ScoresWithSemanticQuery(
	ctx context.Context,
	prepared *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	if !validSemanticQuery(prepared) {
		return nil, fmt.Errorf("semantic query embedding is invalid")
	}
	encoder, err := s.semanticRowEncoder(ctx, true)
	if err != nil {
		return nil, err
	}
	// One encoder call holds one static batch of 128 rows. The scorer runs
	// ceil(N/128) calls, up to the model call limit at the same time. So any
	// number of candidates gets a score.
	scores := make([]float32, len(documents))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(semanticModelCallLimit(runtime.GOMAXPROCS(0), encoder.CallCPUs()))
	for start := 0; start < len(documents); start += semanticModelBatchRows {
		batch := documents[start:min(start+semanticModelBatchRows, len(documents))]
		group.Go(func() error {
			values, err := s.scoreDocumentBatch(groupCtx, encoder, prepared, batch)
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
// the prepared query with one encoder call. It gives one score per document
// (lateOnMaxSimPrepared), or an error.
func (s *lateOnModel) scoreDocumentBatch(
	ctx context.Context,
	encoder *lateOnStaticRowEncoder,
	prepared *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	tokenizer, err := lateOnTokenizers.take(ctx)
	if err != nil {
		return nil, err
	}
	inputIDs, attention, masks, tokenizeErr := tokenizeLateOnDocuments(
		tokenizer,
		s.punctuation,
		documents,
	)
	lateOnTokenizers.release(tokenizer)
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

// semanticRowEncoder returns the encoder for one kind of call. interactive
// reports a call that runs once while the user waits: a query, or the scores
// of one candidate set. The first call builds the first encoder, and its kind
// sets the session's CPU threads (newLateOnRowEncoder). A query gets many
// threads for one call at a time. An index build gets few threads for many
// calls at the same time. So a warm search that uses the model makes one
// session.
// The CLI starts the query before any vector build. So when a command also
// builds vectors on the CPU provider, a build call gets a second CPU session
// with the build's thread count. That session takes about 40 ms to make. If
// it fails, build calls use the first session.
// On 18 CPUs, one call at a time on 12 threads encodes about 245 rows/s
// (523 ms per 128 rows). The build's calls, 3 threads each, encode about
// 650 rows/s in total (the 100k-row pass took 153.9 s).
func (s *lateOnModel) semanticRowEncoder(ctx context.Context, interactive bool) (*lateOnStaticRowEncoder, error) {
	if s.closed.Load() {
		return nil, errLateOnModelClosed
	}
	s.encoderOnce.Do(func() {
		s.encoder, s.encoderErr = newLateOnRowEncoder(ctx, interactive)
	})
	if interactive || s.encoderErr != nil || !s.encoder.interactive {
		return s.encoder, s.encoderErr
	}
	s.buildOnce.Do(func() {
		s.build, s.buildErr = newLateOnCPUBuildEncoder(ctx)
	})
	if s.buildErr != nil {
		// The first session still works, with fewer calls at a time.
		slog.Debug("Index build session unavailable; using the query session", "error", s.buildErr)
		return s.encoder, nil
	}
	return s.build, nil
}

// errLateOnModelClosed is the error of every encoder request
// (semanticRowEncoder) after Close.
var errLateOnModelClosed = errors.New("the semantic model is closed")

func (s *lateOnModel) Close() error {
	s.closed.Store(true)
	// Each Once waits for a construction that runs now. With none, it stops a
	// later call from building one. So Close reads the encoder fields after
	// their write. Close writes neither encoder pointer, because a call that
	// passed the closed check can still read both.
	s.encoderOnce.Do(func() { s.encoderErr = errLateOnModelClosed })
	s.buildOnce.Do(func() { s.buildErr = errLateOnModelClosed })
	var closeErr error
	for _, encoder := range []*lateOnStaticRowEncoder{s.encoder, s.build} {
		closeErr = errors.Join(closeErr, encoder.Close())
	}
	// Keep the process-wide tokenizers and the shared environment loaded for
	// the process lifetime. Unloading the ONNX library after inference can
	// race with its native thread cleanup.
	return closeErr
}

// newLateOnTokenizer parses the tokenizer asset and checks the IDs of its
// special tokens. It also gives the IDs of the ASCII punctuation characters
// that encode to one token.
func newLateOnTokenizer(
	content []byte,
) (*lateOnTokenizer, map[int]struct{}, error) {
	// Serialized rerank input is at most 16 KiB. The tokenizer bound is
	// larger, so Seek's truncation around the match decides what stays.
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

// sanitizeLateOnTokenizerInput replaces invalid UTF-8 and each NUL byte with
// U+FFFD: the native tokenizer takes a C string, which cannot hold a NUL.
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
	inputIDs, attention, masks := packLateOnRows(encoded, punctuation)
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
	inputIDs, attention, masks := packLateOnRows(encoded, punctuation)
	return inputIDs, attention, masks, nil
}

// packLateOnRows pads each row of token IDs to lateOnSequenceLength. Its mask
// keeps every token that is not in punctuation; a nil set keeps every token.
func packLateOnRows(
	encoded [][]int,
	punctuation map[int]struct{},
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
			masks[row][column] = !isPunctuation
		}
	}
	return inputIDs, attention, masks
}

// lateOnMaxSimPrepared gives the MaxSim score of each document. For each kept
// query token, it takes the largest dot product with a kept document token. It
// sums these values in query token order as a float32. NumKong's exact
// batched float32 dot products give, in one call, the dot products of a block
// of documents and every kept query token. They sum each dot product in
// float64.
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
	var kept []float32
	for queryToken, keepQuery := range queryMask {
		if keepQuery {
			kept = append(kept, query[queryToken*semanticEmbeddingDimensions:(queryToken+1)*semanticEmbeddingDimensions]...)
		}
	}
	tokens := len(kept) / semanticEmbeddingDimensions
	var packed semanticNumKongPack
	if tokens > 0 {
		backend, err := semanticNumKongBackend()
		if err != nil {
			return nil, err
		}
		packed = packSemanticF32Query(backend, kept)
	}
	// One kernel call scores lateOnMaxSimBlockDocuments documents, so its dot
	// products stay in the CPU cache.
	const lateOnMaxSimBlockDocuments = 8
	products := make([]float64, min(lateOnMaxSimBlockDocuments, len(documentMasks))*lateOnSequenceLength*tokens)
	scores := make([]float32, len(documentMasks))
	for first := 0; first < len(documentMasks); first += lateOnMaxSimBlockDocuments {
		count := min(lateOnMaxSimBlockDocuments, len(documentMasks)-first)
		if tokens > 0 {
			semanticNumKongDotsF32(documents[first*queryRowBytes:(first+count)*queryRowBytes], &packed, products)
		}
		for offset := range count {
			document := first + offset
			mask := documentMasks[document]
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
			lines := products[offset*lateOnSequenceLength*tokens : (offset+1)*lateOnSequenceLength*tokens]
			var sum float32
			for queryToken := range tokens {
				maximum := math.Inf(-1)
				for documentToken, keepDocument := range mask {
					if !keepDocument {
						continue
					}
					if dot := lines[documentToken*tokens+queryToken]; dot > maximum {
						maximum = dot
					}
				}
				if math.IsInf(maximum, -1) {
					return nil, fmt.Errorf("LateOn document has no scoreable token")
				}
				sum += float32(maximum)
			}
			if math.IsNaN(float64(sum)) || math.IsInf(float64(sum), 0) {
				return nil, fmt.Errorf("LateOn score is not finite")
			}
			scores[document] = sum
		}
	}
	return scores, nil
}

// lateOnRowHasUsableToken reports whether a row (a document or a query
// window) keeps one masked token with a finite, non-zero vector. A provider
// fault can return a zero vector. That vector gives a dot product of zero,
// which keeps the score finite and hides the fault. So the "no scoreable
// token" guard of lateOnMaxSimPrepared never sees it.
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
				// Keep looking: a later token can still carry the row. In the
				// scoring loop of lateOnMaxSimPrepared, a NaN or -Inf dot
				// product never wins the maximum. A +Inf one makes the score
				// not finite, which fails the call.
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

// encodeLateOnDocument gives the token IDs of one serialized document, at most
// limit of them, around its match (truncateLateOnDocumentTokens).
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

// sanitizeLateOnDocumentInput sanitizes each part of text and moves the three
// offsets to the sanitized text. serializeLateOnDocumentWithMatch gives
// 0 <= metadataEnd <= matchAt <= matchEnd <= len(text).
func sanitizeLateOnDocumentInput(
	text string,
	metadataEnd int,
	matchAt int,
	matchEnd int,
) (string, int, int, int) {
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

// truncateLateOnDocumentTokens packs one annotated document into limit tokens
// (a positive limit). spans holds one span for each ID, as encode gives them.
// The byte offsets refer to the prefixed serialized text. It keeps the model
// prefix and separator. It uses at most one quarter of the content slots for
// metadata, and centers the rest of the evidence on the match. When no token
// overlaps the match, it keeps the start of the body. An invalid annotated
// layout keeps the head.
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
	if matchEnd <= matchAt {
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

// errLateOnQueryTooLong reports a query with more windows than one model call
// holds next to the empty document.
var errLateOnQueryTooLong = errors.New("the query is too long for the model")

// lateOnQueryWindows splits a query into 128-token windows. Each window holds
// CLS, the query prefix, a part of the query tokens, and SEP, so a long query
// keeps every token. A query that fits gives one window. The windows and the
// empty document go in one model call (PrepareSemanticQuery). So a query of
// more than semanticModelBatchRows - 1 windows fails with
// errLateOnQueryTooLong.
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
	if count := (len(body) + room - 1) / room; count > semanticModelBatchRows-1 {
		return nil, fmt.Errorf("%w: %d windows, the limit is %d",
			errLateOnQueryTooLong, count, semanticModelBatchRows-1)
	}
	var windows [][]int
	for start := 0; start < len(body); start += room {
		window := make([]int, 0, lateOnSequenceLength)
		window = append(window, encoded[:head]...)
		window = append(window, body[start:min(start+room, len(body))]...)
		windows = append(windows, append(window, lateOnSEPTokenID))
	}
	return windows, nil
}

// truncateLateOnHead keeps the first limit-1 IDs and a separator. The caller
// gives a positive limit below len(encoded).
func truncateLateOnHead(encoded []int, limit int) []int {
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

// ensureLateOnRuntime installs the ONNX Runtime library of bundle in its cache
// folder (lateOnRuntimeDir), unless a file with its digest is there. It
// returns the path. It marks the directory as used for GC (touchRuntimeUse).
func ensureLateOnRuntime(bundle lateOnRuntimeBundle) (string, error) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return "", err
	}
	directory := lateOnRuntimeDir(cacheRoot, bundle)
	defer touchRuntimeUse(directory)
	return installPrivateAsset(
		directory,
		bundle.FileName,
		".onnxruntime-*.tmp",
		bundle.Bytes,
		bundle.SHA256,
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
				io.LimitReader(decoder, bundle.Bytes+1),
			)
			if err != nil {
				return err
			}
			if written != bundle.Bytes || hex.EncodeToString(hash.Sum(nil)) != bundle.SHA256 {
				return fmt.Errorf("extracted runtime digest mismatch")
			}
			return nil
		},
	)
}

// lateOnRuntimeDir is the cache folder of the ONNX Runtime library of bundle:
// reranker/<version>/<sha256>. The collector keeps the folder of this binary's
// bundle and removes the others (retiredRuntimes).
func lateOnRuntimeDir(cacheRoot string, bundle lateOnRuntimeBundle) string {
	return filepath.Join(cacheRoot, rerankerDir, bundle.Version, bundle.SHA256)
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
