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
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

// lateOnSelectedProvider records the provider the encoder settled on. Index and
// search paths read it for their fallback logs; they do not hold the encoder.
var lateOnSelectedProvider atomic.Value

// These are the on-disk provider-check verdicts. They live in the shared encoder
// because it reads them to decide whether to build an accelerated session at
// all; the accelerated targets write them. readLateOnProviderRecord is the only
// parser and writeLateOnProviderVerdict the only writer; anything else on disk
// counts as no verdict and makes the next process measure again.
//
// Keep the spellings stable: a machine that upgrades Seek keeps the verdict its
// previous version recorded.
const (
	lateOnVerdictTrusted  = "trusted"
	lateOnVerdictRejected = "rejected"
	// lateOnVerdictRecheck records real work that produced unusable output the
	// fixed probe row does not reproduce. The record reads "recheck <count>
	// <unix-seconds>", where the time is when the count last changed. One is
	// evidence, not proof: normalizeSemanticVector also rejects a vector whose
	// norm cancels exactly, which a healthy provider can produce. The next
	// process measures again rather than trusting one sample.
	//
	// The count must survive a passing probe. Without that, a provider that
	// agrees on the probe and fails on real work would alternate between the two
	// records for ever and never earn a demotion, so the continuous guards would
	// report the same fault on every build and nothing would change.
	lateOnVerdictRecheck = "recheck"

	// lateOnVerdictFaultsBeforeRejection is how many separate batches must fail
	// before this host stops using the accelerated provider.
	lateOnVerdictFaultsBeforeRejection = 2

	// lateOnProviderFaultWindow is how long one fault stays on the record. Two
	// faults inside it demote the provider; a lone fault outside it expires, so a
	// host that saw one benign cancellation returns to the cached-verdict path
	// instead of paying a second session and two full batches on every search.
	lateOnProviderFaultWindow = 7 * 24 * time.Hour
)

// lateOnCPUProviderName names the provider every supported target has. It is the
// reference the accelerated provider is compared against, and the provider Seek
// falls back to.
const lateOnCPUProviderName = "cpu"

// lateOnProviderEnv pins the semantic provider. It exists so a bug report can
// name one provider instead of whichever one this host selected. An empty value,
// or "auto", keeps the automatic choice.
//
// A name this build cannot use stops a search, but not a command that never
// loads the model: failing seek gc or a text-only search over a setting neither
// reads would turn a typo into an outage.
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
	if lateOnProviderHasAccelerator && lateOnAcceleratedProviderName != lateOnCPUProviderName {
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

// semanticProviderName reports the provider in use. It returns one of three
// things: a provider name once an encoder has settled on one; "selecting:" and
// the intended name while an encoder is being built, which is what the fallback
// logs print when provider selection is itself what failed; and "unset" before
// any encoder has been attempted. Callers use it only for logs, so match on the
// prefix rather than on equality.
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
// A turned token is far coarser than this limit, so the size of the fault in
// https://github.com/microsoft/onnxruntime/issues/32569 is not measured here.
// lateOnProbeMagnitudeLimit carries that comparison, in the same units as the
// reported fault.
const lateOnProbeAgreementLimit = 1e-3

// lateOnProbeMagnitudeLimit bounds how far two providers may disagree on one
// token magnitude, as a ratio minus one. Direction alone is not enough: MaxSim
// scores raw, un-normalized output in lateOnMaxSimPrepared, so a provider that
// shrinks every vector by the same factor keeps every cosine at 1 and still
// depresses every score by that factor. The macOS 15 fault moves the leading
// score about 13 percent. This limit sits well below that and far above the
// measured agreement between healthy providers.
const lateOnProbeMagnitudeLimit = 2e-2

// lateOnProbeRows builds the fixed probe batch. Every row carries different
// content: the model runs a fixed batch of semanticModelBatchRows whatever we put
// in it, so filling only one row would pay for the whole batch and compare a
// single row of it. Distinct rows make the comparison that follows sample the
// whole batch for the cost of the arithmetic alone.
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
// every cosine at 1. It also rejects a token vector that has no direction at
// all, which is the shape the macOS 15 fault takes when it returns a zero
// vector.
func lateOnOutputsAgree(accelerated, reference []float32) (bool, string) {
	want := semanticModelBatchRows * lateOnSequenceLength * semanticEmbeddingDimensions
	if len(accelerated) < want || len(reference) < want {
		return false, "probe output is too short"
	}
	// The body stays inline. Measured over 12,288 token comparisons, interleaved
	// in one benchmark binary so thermal drift cannot favour one shape: this
	// nested form runs in about 380 microseconds, a per-token helper in about
	// 473, and one flattened counter that recovers the row and token with a
	// division and a modulo in about 563. All three are far below the inference
	// the probe already pays, so this is about reading clearly first and paying
	// nothing for it second.
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
// Each call borrows a separate output tensor. outputMu protects only that tensor
// pool; it does not serialize inference. Close requires all Run calls to finish.
//
// The pool works before session is set, which is how the provider check borrows
// it while newLateOnRowEncoder is still deciding which session to keep.
type lateOnStaticRowEncoder struct {
	session    *ort.DynamicAdvancedSession
	callCPUs   int
	outputMu   sync.Mutex
	outputFree []*ort.Tensor[float32]
	outputAll  []*ort.Tensor[float32]
}

// newLateOnRowEncoder builds the encoder for this process and decides which
// execution provider it uses.
//
// On a target with an accelerator the order is: a pinned provider wins outright;
// otherwise a cached rejection skips the accelerated session entirely; otherwise
// the accelerated session is built and checked against the CPU provider, and a
// disagreement destroys it and falls back. Every path returns the reservation
// that matches the provider actually chosen, because the build scheduler freezes
// that value before its first batch and cannot revise it.
//
// The returned encoder is created before its session so the check can borrow its
// tensor pool. The deferred release covers every failure after that point; a
// success path fills the session in and the release becomes a no-op.
func newLateOnRowEncoder(ctx context.Context) (built *lateOnStaticRowEncoder, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	model, decodeErr := decodeLateOnAsset(lateOnCompressedModel)
	if decodeErr != nil {
		return nil, fmt.Errorf("load LateOn model: %w", decodeErr)
	}
	forced, forcedErr := lateOnForcedProvider()
	if forcedErr != nil {
		return nil, forcedErr
	}
	// Record the intent before the first session attempt. A failure during
	// construction is logged by the index and search paths, and those records
	// carry the provider; without this they would all read "unset", which is
	// least useful exactly when provider selection is what failed.
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
	// True only where an accelerator exists and we already know not to use it.
	// On a target without one there is nothing to skip: the ordinary path below
	// builds the CPU session through configureLateOnProvider.
	skipAcceleratedSession := lateOnProviderHasAccelerator &&
		(forced == lateOnCPUProviderName || cachedVerdict == lateOnVerdictRejected)
	if !lateOnProviderHasAccelerator || skipAcceleratedSession {
		providerCallCPUs = lateOnCPUIntraOpThreads(runtime.GOMAXPROCS(0))
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
		// The operator pinned the CPU provider. Do not build the accelerated
		// session at all, so the choice is reproducible in a bug report.
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
		return encoder, nil
	}
	session, providerErr = newLateOnStaticSession(model, func(options *ort.SessionOptions) error {
		return configureLateOnProvider(options, providerCallCPUs, model)
	})
	if providerErr != nil && lateOnProviderHasAccelerator && forced == lateOnAcceleratedProviderName {
		// The operator pinned the accelerated provider. Report the failure instead
		// of quietly using another one.
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
			// provider, so keep it and let the index guards report a real fault.
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
			providerErr = fmt.Errorf("%s disagrees with the CPU provider: %s",
				lateOnAcceleratedProviderName, reason)
		}
	}
	if providerErr != nil && lateOnProviderHasAccelerator {
		providerCallCPUs = lateOnCPUIntraOpThreads(runtime.GOMAXPROCS(0))
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
		// Treat a Core ML call as accelerator work for shared-gate accounting. Core
		// ML can still use the CPU, GPU, or Neural Engine. The adaptive call controller
		// limits measured throughput and memory use. A zero reservation prevents
		// provider waits from blocking ready tokenizer, ctags, or USearch CPU work.
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
		if reason == "" {
			reason = "default"
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
	return encoder, nil
}

// newLateOnCPUSession builds a session on the CPU provider. Two paths need one —
// a provider this host has already ruled out, and a fallback after the
// accelerated session proved unusable — and they must configure it identically,
// because the reservation the build scheduler freezes is derived from the same
// thread count.
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

// lateOnCPUIntraOpThreads keeps both ONNX kernel work and independent model
// calls available. Both dimensions grow with the effective CPU count. The
// outer controller then reduces the call count when live memory or measured
// throughput requires it.
func lateOnCPUIntraOpThreads(cpuLimit int) int {
	cpuLimit = max(1, cpuLimit)
	return max(1, int(math.Round(math.Sqrt(float64(cpuLimit)/2))))
}

func (encoder *lateOnStaticRowEncoder) CallCPUs() int {
	if encoder == nil {
		return 1
	}
	return max(0, encoder.callCPUs)
}

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
		semanticModelBatchRows,
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
