//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	ort "github.com/yalue/onnxruntime_go"
)

// lateOnModelBytes decodes the embedded model once per process. The first
// encoder and the index build session (newLateOnCPUBuildEncoder) both read it.
// A second decode took about 20 ms of the 60 ms that the build session took to
// make on 18 CPUs. The decoded model stays in memory for the life of the
// process.
var lateOnModelBytes = sync.OnceValues(func() ([]byte, error) {
	return decodeLateOnAsset(lateOnCompressedModel)
})

// lateOnSelectedProvider records the provider the encoder settled on. Index and
// search paths read it for their logs, and rejectLateOnAcceleratedProvider
// reads it to find the provider in use; they do not hold the encoder.
var lateOnSelectedProvider atomic.Value

// lateOnVerdictTrusted and lateOnVerdictRejected are the two verdict spellings
// that newLateOnRowEncoder compares. They are shared because this file also
// compiles where no accelerator exists. There cachedLateOnProviderVerdict
// returns an empty string that matches neither, so the comparisons stand and
// Seek reads and writes no verdict file.
//
// The rest of the record format (the recheck spelling, the fault count and the
// fault window) stays with the accelerated target. That file holds the only
// parser and the only writer. Anything else on disk counts as no verdict and
// makes the next process measure again.
//
// Keep these spellings stable: a machine that upgrades Seek keeps the verdict
// its previous version recorded.
const (
	lateOnVerdictTrusted  = "trusted"
	lateOnVerdictRejected = "rejected"
)

// lateOnCPUProviderName names the provider every supported target has. It is
// the reference the accelerated provider is compared against, and the provider
// Seek falls back to.
const lateOnCPUProviderName = "cpu"

// lateOnProviderEnv pins the semantic provider. It exists so a bug report can
// name one provider instead of whichever one this host selected. An empty
// value, or "auto", keeps the automatic choice.
//
// A name that this build cannot use stops a search. It does not stop seek gc
// or a --lexical-only search. They do not load the model, so they do not read
// the setting, and a typo in it must not stop them.
const lateOnProviderEnv = "SEEK_PROVIDER"

// lateOnForcedProvider returns the pinned provider name, or an empty string for
// the automatic choice. An unknown value is an error: a silent fallback would
// defeat the reason the operator pinned it.
func lateOnForcedProvider() (string, error) {
	// Plain conditions, not a switch: on targets without an accelerator both
	// provider names hold the same value, which a switch rejects as a duplicate
	// case.
	value := strings.ToLower(strings.TrimSpace(os.Getenv(lateOnProviderEnv)))
	if value == "" || value == "auto" {
		return "", nil
	}
	accepted := []string{lateOnCPUProviderName}
	if lateOnProviderHasAccelerator {
		accepted = append(accepted, lateOnAcceleratedProviderName)
	}
	if slices.Contains(accepted, value) {
		return value, nil
	}
	quoted := make([]string, 0, len(accepted)+1)
	for _, name := range accepted {
		quoted = append(quoted, strconv.Quote(name))
	}
	quoted = append(quoted, strconv.Quote("auto"))
	return "", fmt.Errorf(
		"%s is %q; this build accepts %s",
		lateOnProviderEnv,
		value,
		strings.Join(quoted, ", "),
	)
}

// semanticProviderName reports the provider in use. After an encoder chose a
// provider, it returns that name. While newLateOnRowEncoder runs, it returns
// "selecting:" and the intended name. The fallback logs print that value when
// provider selection itself failed. Before the first encoder attempt, it
// returns "unset". Log readers match on the prefix.
// rejectLateOnAcceleratedProvider compares it for equality, so a provider that
// is still being selected does not count as the one in use.
func semanticProviderName() string {
	if name, ok := lateOnSelectedProvider.Load().(string); ok {
		return name
	}
	return "unset"
}

// lateOnProbeActiveTokens is the number of scored tokens in the provider probe
// row. The row holds a class token, a document prefix, filler tokens, and a
// separator, then padding.
const lateOnProbeActiveTokens = 96

// lateOnProbeAgreementLimit bounds how far two providers may disagree on one
// token direction, as 1 minus cosine similarity. Measured on macOS 26 ARM64,
// Core ML against the CPU provider over 12,288 active tokens: worst 1 minus
// cosine 4.7e-06. This limit is about 200 times that, which leaves room for
// hardware this project has not sampled.
//
// The fault in https://github.com/microsoft/onnxruntime/issues/32569 changes
// the length of the token vectors, not their direction, so a cosine does not
// see it. lateOnProbeMagnitudeLimit measures it, in the same units as the
// reported fault.
const lateOnProbeAgreementLimit = 1e-3

// lateOnProbeMagnitudeLimit bounds how far two providers may disagree on one
// token magnitude, as a ratio minus one. Direction alone is not enough:
// lateOnMaxSimPrepared scores the raw output with no normalization. A provider
// that shrinks every vector by the same factor keeps every cosine at 1, but
// lowers every score by that factor. The macOS 15 fault moves the leading
// score about 13 percent. This limit is well below that and far above the
// measured agreement between healthy providers.
const lateOnProbeMagnitudeLimit = 2e-2

// lateOnProbeRows builds the fixed probe batch. Every row holds different
// content. The session always runs a static batch of semanticModelBatchRows
// rows. A probe with one filled row would pay for the whole batch and compare
// only one row. Distinct rows let the comparison sample the whole batch for
// the cost of the arithmetic alone.
//
// It uses token identifiers directly, because the probe runs inside encoder
// construction, where taking a tokenizer from the pool could deadlock.
func lateOnProbeRows() ([]int64, []int64) {
	ids := make([]int64, semanticModelBatchRows*lateOnSequenceLength)
	attention := make([]int64, len(ids))
	for index := range ids {
		ids[index] = lateOnPadTokenID
	}
	for row := range semanticModelBatchRows {
		base := row * lateOnSequenceLength
		ids[base] = lateOnCLSTokenID
		ids[base+1] = lateOnDocumentPrefixID
		for token := 2; token < lateOnProbeActiveTokens-1; token++ {
			// A fixed spread over the vocabulary, different for each row. The
			// values only need to be stable and to exercise the whole embedding
			// table.
			ids[base+token] = int64(100 + (row*7919+token*1601)%40_000)
		}
		ids[base+lateOnProbeActiveTokens-1] = lateOnSEPTokenID
		for token := range lateOnProbeActiveTokens {
			attention[base+token] = 1
		}
	}
	return ids, attention
}

// lateOnOutputsAgree reports whether two providers produced the same token
// vectors across the probe batch. It tests direction and magnitude separately:
// re-ranking scores raw output, so a uniform shrink is a real fault that leaves
// every cosine at 1. It also rejects a token vector with no direction: the
// macOS 15 fault can return a zero vector.
func lateOnOutputsAgree(accelerated, reference []float32) (bool, string) {
	want := semanticModelBatchRows * lateOnSequenceLength * semanticEmbeddingDimensions
	if len(accelerated) < want || len(reference) < want {
		return false, "probe output is too short"
	}
	// The body stays inline. A benchmark measured 12,288 token comparisons,
	// with the forms interleaved in one binary so thermal drift cannot favor
	// one form. This nested form takes about 380 microseconds, and a per-token
	// helper about 473. One flattened counter that gets the row and token with
	// a division and a modulo takes about 563. All three are far below the
	// inference the probe already pays, so the clearest form costs nothing.
	for row := range semanticModelBatchRows {
		for token := range lateOnProbeActiveTokens {
			offset := (row*lateOnSequenceLength + token) * semanticEmbeddingDimensions
			var dot, acceleratedNorm, referenceNorm float64
			for dimension := range semanticEmbeddingDimensions {
				left := float64(accelerated[offset+dimension])
				right := float64(reference[offset+dimension])
				if math.IsNaN(left) || math.IsInf(left, 0) {
					return false, fmt.Sprintf("row %d token %d is not finite", row, token)
				}
				dot += left * right
				acceleratedNorm += left * left
				referenceNorm += right * right
			}
			if acceleratedNorm == 0 {
				return false, fmt.Sprintf("row %d token %d has no direction", row, token)
			}
			if referenceNorm == 0 {
				// The reference provider produced nothing usable here, so the
				// comparison cannot decide. Let the continuous index guards
				// report the fault.
				continue
			}
			acceleratedLength := math.Sqrt(acceleratedNorm)
			referenceLength := math.Sqrt(referenceNorm)
			cosine := dot / (acceleratedLength * referenceLength)
			if 1-cosine > lateOnProbeAgreementLimit {
				return false, fmt.Sprintf(
					"row %d token %d direction differs by %.3g", row, token, 1-cosine)
			}
			ratio := acceleratedLength / referenceLength
			if math.Abs(ratio-1) > lateOnProbeMagnitudeLimit {
				return false, fmt.Sprintf(
					"row %d token %d magnitude ratio is %.4f", row, token, ratio)
			}
		}
	}
	return true, ""
}

// lateOnStaticRowEncoder owns one ONNX session shared by concurrent Run calls.
// Each call borrows a separate output tensor. outputMu protects only that
// tensor pool; it does not serialize inference. Close requires all Run calls
// to finish.
//
// The pool works before session is set, which is how the provider check
// borrows it while newLateOnRowEncoder is still deciding which session to
// keep.
type lateOnStaticRowEncoder struct {
	session  *ort.DynamicAdvancedSession
	callCPUs int
	// interactive marks a CPU session sized for one call at a time
	// (lateOnCPUInteractiveThreads).
	interactive bool
	outputMu    sync.Mutex
	outputFree  []*ort.Tensor[float32]
	outputAll   []*ort.Tensor[float32]
}

// newLateOnRowEncoder builds the first encoder of a model and decides which
// execution provider it uses.
//
// On a target with an accelerator, the order is:
//   - A pinned provider wins.
//   - Else a cached rejection skips the accelerated session.
//   - Else it builds the accelerated session. With no cached trust, it checks
//     that session against the CPU provider. A disagreement or a failed
//     session falls back to the CPU provider.
//
// Every path sets the CPU reservation (callCPUs) of the provider that it
// chose. It must, because the build scheduler reads that value once, before
// its first batch (SemanticCallCPUs), and cannot change it.
//
// interactive marks a first use of one call at a time (a query or the scores
// of a candidate set), not the concurrent index build. It sets the thread
// count of a CPU session, which the session keeps for its life.
//
// The function creates the encoder before its session, so the provider check
// can borrow its tensor pool. The deferred release covers every failure after
// that point. On success, built is set and the release does nothing.
func newLateOnRowEncoder(ctx context.Context, interactive bool) (built *lateOnStaticRowEncoder, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	model, decodeErr := lateOnModelBytes()
	if decodeErr != nil {
		return nil, fmt.Errorf("load LateOn model: %w", decodeErr)
	}
	forced, forcedErr := lateOnForcedProvider()
	if forcedErr != nil {
		return nil, forcedErr
	}
	// Record the intent before the first session attempt. The index and
	// search paths log a failure during construction with the provider.
	// Without this record, those logs read "unset", which helps least when
	// provider selection itself failed.
	intent := lateOnAcceleratedProviderName
	if !lateOnProviderHasAccelerator {
		intent = lateOnCPUProviderName
	}
	if forced != "" {
		intent = forced
	}
	lateOnSelectedProvider.Store("selecting:" + intent)
	providerCallCPUs := 1
	cachedVerdict := ""
	if lateOnProviderHasAccelerator && forced == "" {
		cachedVerdict = cachedLateOnProviderVerdict(model)
	}
	// True only where an accelerator exists and a CPU pin or a cached
	// rejection excludes it. On a target without one there is nothing to
	// skip: the path below builds the CPU session through
	// configureLateOnProvider.
	skipAcceleratedSession := lateOnProviderHasAccelerator &&
		(forced == lateOnCPUProviderName || cachedVerdict == lateOnVerdictRejected)
	if !lateOnProviderHasAccelerator || skipAcceleratedSession {
		providerCallCPUs = lateOnCPUThreads(interactive)
	}
	// The encoder exists before its session so the probe can borrow its tensor
	// pool. Any failure after that point must release what the pool holds, or a
	// probe tensor outlives the function.
	encoder := &lateOnStaticRowEncoder{}
	defer func() {
		if built == nil {
			_ = encoder.Close()
		}
	}()
	var session *ort.DynamicAdvancedSession
	var providerErr error
	if skipAcceleratedSession {
		// The operator pinned the CPU provider, or a cached check rejected the
		// accelerated one. Do not build the accelerated session at all: a pin
		// stays reproducible in a bug report, and a rejected host compiles no
		// Core ML model.
		session, providerErr = newLateOnCPUSession(model, providerCallCPUs)
		if providerErr != nil {
			return nil, fmt.Errorf("create LateOn CPU session: %w", providerErr)
		}
		cpuReason := "pinned by " + lateOnProviderEnv
		if forced == "" {
			cpuReason = "rejected by a cached provider check"
		}
		lateOnSelectedProvider.Store(lateOnCPUProviderName)
		slog.Debug(
			"Selected semantic provider",
			"provider", lateOnCPUProviderName,
			"reason", cpuReason,
			"call_cpus", providerCallCPUs,
		)
		encoder.session = session
		encoder.callCPUs = providerCallCPUs
		encoder.interactive = interactive
		return encoder, nil
	}
	session, providerErr = newLateOnStaticSession(model, func(options *ort.SessionOptions) error {
		return configureLateOnProvider(options, providerCallCPUs, model)
	})
	if providerErr != nil && lateOnProviderHasAccelerator && forced == lateOnAcceleratedProviderName {
		// The operator pinned the accelerated provider. Report the failure
		// instead of quietly using another one.
		return nil, fmt.Errorf(
			"create pinned LateOn %s session: %w",
			lateOnAcceleratedProviderName,
			providerErr,
		)
	}
	rejection := ""
	acceptance := ""
	if providerErr == nil && lateOnProviderHasAccelerator && forced == "" &&
		cachedVerdict != lateOnVerdictTrusted {
		trusted, reason, verifyErr := verifyLateOnAcceleratedProvider(ctx, encoder, model, session)
		switch {
		case verifyErr != nil:
			// The check could not finish. That is not evidence against the
			// provider, so keep it and let the index guards report a real
			// fault.
			slog.Debug("Semantic provider check did not finish", "error", verifyErr)
			acceptance = "provider check did not finish"
		case trusted:
			acceptance = reason
		default:
			rejection = reason
			if destroyErr := session.Destroy(); destroyErr != nil {
				return nil, fmt.Errorf("release the rejected LateOn session: %w", destroyErr)
			}
			session = nil
			providerErr = fmt.Errorf("the provider check rejected %s: %s",
				lateOnAcceleratedProviderName, reason)
		}
	}
	if providerErr != nil && lateOnProviderHasAccelerator {
		providerCallCPUs = lateOnCPUThreads(interactive)
		var cpuErr error
		session, cpuErr = newLateOnCPUSession(model, providerCallCPUs)
		if cpuErr != nil {
			return nil, fmt.Errorf(
				"create LateOn session: %w",
				errors.Join(
					fmt.Errorf("accelerated provider: %w", providerErr),
					fmt.Errorf("CPU provider: %w", cpuErr),
				),
			)
		}
		slog.Debug("Accelerated session unusable; using the CPU provider", "error", providerErr)
	}
	if providerErr != nil && session == nil {
		return nil, providerErr
	}
	accountedCallCPUs := providerCallCPUs
	if lateOnProviderHasAccelerator && providerErr == nil {
		// Treat a Core ML call as accelerator work for shared-gate accounting.
		// Core ML can still use the CPU, GPU, or Neural Engine. The adaptive
		// call controller sets the call count from measured throughput and
		// memory use. A zero reservation prevents provider waits from blocking
		// ready tokenizer, ctags, or USearch CPU work.
		accountedCallCPUs = 0
	}
	selected := lateOnCPUProviderName
	reason := "accelerated session failed"
	if rejection != "" {
		reason = "rejected by the provider check: " + rejection
	}
	if lateOnProviderHasAccelerator && providerErr == nil {
		selected = lateOnAcceleratedProviderName
		reason = acceptance
		if cachedVerdict == lateOnVerdictTrusted {
			reason = "cached verdict"
		}
	} else if !lateOnProviderHasAccelerator {
		reason = "only provider"
	}
	if forced != "" {
		reason = "pinned by " + lateOnProviderEnv
	}
	lateOnSelectedProvider.Store(selected)
	slog.Debug(
		"Selected semantic provider",
		"provider", selected,
		"reason", reason,
		"call_cpus", accountedCallCPUs,
	)
	encoder.session = session
	encoder.callCPUs = accountedCallCPUs
	// Only a CPU session has threads to size; an accelerated call reserves
	// none (accountedCallCPUs 0).
	encoder.interactive = interactive && accountedCallCPUs > 0
	return encoder, nil
}

// newLateOnCPUBuildEncoder builds a CPU encoder with the thread count of the
// index build. It makes no provider choice: semanticRowEncoder calls it only
// after the first encoder chose a CPU session sized for one call.
func newLateOnCPUBuildEncoder(ctx context.Context) (*lateOnStaticRowEncoder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	model, err := lateOnModelBytes()
	if err != nil {
		return nil, fmt.Errorf("load LateOn model: %w", err)
	}
	threads := lateOnCPUThreads(false)
	session, err := newLateOnCPUSession(model, threads)
	if err != nil {
		return nil, fmt.Errorf("create LateOn CPU build session: %w", err)
	}
	return &lateOnStaticRowEncoder{session: session, callCPUs: threads}, nil
}

// newLateOnCPUSession builds a session on the CPU provider. Three paths need
// one:
//   - a CPU pin or a cached rejection of the accelerated provider;
//   - a fallback after the accelerated session failed or the check rejected it;
//   - the index build session (newLateOnCPUBuildEncoder).
//
// They configure it in the same way, because the build scheduler takes its
// CPU reservation from the same thread count.
func newLateOnCPUSession(model []byte, threads int) (*ort.DynamicAdvancedSession, error) {
	return newLateOnStaticSession(model, func(options *ort.SessionOptions) error {
		return configureLateOnCPUProvider(options, threads)
	})
}

func newLateOnStaticSession(
	model []byte,
	configure func(*ort.SessionOptions) error,
) (*ort.DynamicAdvancedSession, error) {
	options, err := newLateOnSessionOptions()
	if err != nil {
		return nil, err
	}
	defer func() { _ = options.Destroy() }()
	if err := configure(options); err != nil {
		return nil, err
	}
	session, err := ort.NewDynamicAdvancedSessionWithONNXData(
		model,
		[]string{"input_ids", "attention_mask"},
		[]string{"output"},
		options,
	)
	if err != nil {
		return nil, fmt.Errorf("create LateOn session: %w", err)
	}
	return session, nil
}

func configureLateOnCPUProvider(options *ort.SessionOptions, intraOpThreads int) error {
	intraOpThreads = max(1, intraOpThreads)
	if err := options.SetIntraOpNumThreads(intraOpThreads); err != nil {
		return fmt.Errorf("set LateOn intra-op threads: %w", err)
	}
	if err := options.SetInterOpNumThreads(1); err != nil {
		return fmt.Errorf("set LateOn inter-op threads: %w", err)
	}
	return nil
}

// lateOnCPUIntraOpThreads is the thread count of a CPU session for the index
// build. It keeps both ONNX kernel work and independent model calls
// available. Both dimensions grow with the effective CPU count. The outer
// controller then reduces the call count when live memory or measured
// throughput requires it.
func lateOnCPUIntraOpThreads(cpuLimit int) int {
	cpuLimit = max(1, cpuLimit)
	return max(1, int(math.Round(math.Sqrt(float64(cpuLimit)/2))))
}

// lateOnCPUInteractiveThreads is the thread count of a session whose first
// use is one call at a time. A warm search that uses the model prepares one
// query, then scores its candidates in batches of semanticModelBatchRows rows.
// The thread count of the index build leaves most CPUs idle for such a call.
// On 18 CPUs, one batch of 20 documents took 828 ms on 3 threads and 559 ms
// on 6. It took 523 ms on 12 threads and 624 ms on 18. So it takes two thirds
// of the CPUs.
func lateOnCPUInteractiveThreads(cpuLimit int) int {
	return max(1, int(math.Round(float64(max(1, cpuLimit))*2/3)))
}

// lateOnCPUThreads is the CPU provider's thread count for a session whose
// first use is interactive or the index build.
func lateOnCPUThreads(interactive bool) int {
	if interactive {
		return lateOnCPUInteractiveThreads(runtime.GOMAXPROCS(0))
	}
	return lateOnCPUIntraOpThreads(runtime.GOMAXPROCS(0))
}

// CallCPUs reports the CPU cost of one Run call, which limits how many calls
// run at the same time (semanticModelCallLimit). An accelerated session costs
// 0.
func (encoder *lateOnStaticRowEncoder) CallCPUs() int {
	return encoder.callCPUs
}

// Run encodes the token rows in inputIDs and passes their output to consume.
// It pads a short batch to semanticModelBatchRows rows, because the session
// takes only that static shape. The output is valid only inside consume.
func (encoder *lateOnStaticRowEncoder) Run(
	ctx context.Context,
	inputIDs []int64,
	attention []int64,
	rows int,
	consume func([]float32) error,
) error {
	if encoder == nil || encoder.session == nil || rows < 1 || rows > semanticModelBatchRows ||
		len(inputIDs) != rows*lateOnSequenceLength || len(attention) != len(inputIDs) || consume == nil {
		return fmt.Errorf("LateOn batch has an invalid shape")
	}
	paddedIDs := inputIDs
	paddedAttention := attention
	if rows < semanticModelBatchRows {
		paddedIDs = make([]int64, semanticModelBatchRows*lateOnSequenceLength)
		paddedAttention = make([]int64, len(paddedIDs))
		for index := range paddedIDs {
			paddedIDs[index] = lateOnPadTokenID
		}
		copy(paddedIDs, inputIDs)
		copy(paddedAttention, attention)
	}
	output, err := encoder.takeOutput()
	if err != nil {
		return err
	}
	defer encoder.releaseOutput(output)
	if err := runLateOnSessionRows(
		ctx,
		encoder.session,
		paddedIDs,
		paddedAttention,
		output,
	); err != nil {
		return err
	}
	return consume(output.GetData()[:rows*lateOnSequenceLength*semanticEmbeddingDimensions])
}

func (encoder *lateOnStaticRowEncoder) takeOutput() (*ort.Tensor[float32], error) {
	encoder.outputMu.Lock()
	defer encoder.outputMu.Unlock()
	last := len(encoder.outputFree) - 1
	if last >= 0 {
		output := encoder.outputFree[last]
		encoder.outputFree = encoder.outputFree[:last]
		return output, nil
	}
	shape := ort.NewShape(
		semanticModelBatchRows,
		lateOnSequenceLength,
		semanticEmbeddingDimensions,
	)
	output, err := ort.NewEmptyTensor[float32](shape)
	if err != nil {
		return nil, fmt.Errorf("create LateOn output: %w", err)
	}
	encoder.outputAll = append(encoder.outputAll, output)
	return output, nil
}

func (encoder *lateOnStaticRowEncoder) releaseOutput(output *ort.Tensor[float32]) {
	encoder.outputMu.Lock()
	encoder.outputFree = append(encoder.outputFree, output)
	encoder.outputMu.Unlock()
}

func (encoder *lateOnStaticRowEncoder) Close() error {
	if encoder == nil {
		return nil
	}
	encoder.outputMu.Lock()
	outputs := encoder.outputAll
	encoder.outputAll = nil
	encoder.outputFree = nil
	encoder.outputMu.Unlock()
	var closeErr error
	for _, output := range outputs {
		closeErr = errors.Join(closeErr, output.Destroy())
	}
	if encoder.session != nil {
		closeErr = errors.Join(closeErr, encoder.session.Destroy())
		encoder.session = nil
	}
	return closeErr
}
