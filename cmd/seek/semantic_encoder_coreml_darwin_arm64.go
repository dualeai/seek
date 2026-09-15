//go:build cgo && darwin && arm64

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	ort "github.com/yalue/onnxruntime_go"
)

const lateOnProviderHasAccelerator = true

// lateOnAcceleratedProviderName identifies the accelerated provider in logs and in
// the provider verdict. Change it when the provider options change meaning.
const lateOnAcceleratedProviderName = "coreml-mlprogram-static"

func configureLateOnProvider(options *ort.SessionOptions, _ int, model []byte) error {
	if err := configureLateOnCPUProvider(options, 1); err != nil {
		return err
	}
	providerOptions := lateOnCoreMLProviderOptions()
	cacheDirectory, err := lateOnRunCoreMLCacheDirectory(model)
	if err != nil {
		return err
	}
	providerOptions["ModelCacheDirectory"] = cacheDirectory
	if err := options.AppendExecutionProviderCoreMLV2(providerOptions); err != nil {
		return fmt.Errorf("enable Core ML: %w", err)
	}
	return nil
}

func lateOnCoreMLProviderOptions() map[string]string {
	return map[string]string{
		"ModelFormat":              "MLProgram",
		"MLComputeUnits":           "ALL",
		"RequireStaticInputShapes": "1",
		"EnableOnSubgraphs":        "0",
	}
}

// lateOnRunCoreMLCacheDirectory resolves the cache directory once for this
// process. The key hashes the whole decoded model, which measures about 10 ms,
// and three call sites need the directory on one cold start: the session
// configuration, the provider check, and the cached-verdict read.
//
// The key also carries ort.GetVersion(), which is empty until
// ensureLateOnEnvironment runs. Every caller reaches this through
// newLateOnRowEncoder, which a *lateOnModel owns, and that model cannot exist
// before the environment is ready. Refusing an empty version keeps that
// requirement loud: caching one would send writers and readers to different
// directories, and the only symptom would be a probe that silently repeats on
// every invocation.
func lateOnRunCoreMLCacheDirectory(model []byte) (string, error) {
	lateOnRunCoreMLCache.mutex.Lock()
	defer lateOnRunCoreMLCache.mutex.Unlock()
	if lateOnRunCoreMLCache.key == "" {
		if ort.GetVersion() == "" {
			return "", fmt.Errorf("the ONNX Runtime is not initialized yet")
		}
		lateOnRunCoreMLCache.key = lateOnCoreMLCacheKey(
			model,
			ort.GetVersion(),
			lateOnCoreMLProviderOptions(),
		)
	}
	return lateOnCoreMLCacheDirectoryForKey(lateOnRunCoreMLCache.key)
}

var lateOnRunCoreMLCache struct {
	mutex sync.Mutex
	key   string
}

// lateOnCoreMLCacheDirectoryForKey creates the directory on every call. Seek
// garbage-collects compiled models, so a resolved path can disappear while the
// process runs.
func lateOnCoreMLCacheDirectoryForKey(key string) (string, error) {
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(cacheRoot, "reranker", "coreml", "mlprogram-static", key)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create Core ML model cache: %w", err)
	}
	// MkdirAll creates with the right mode, so only an older or tampered
	// directory needs fixing. Checking first keeps a metadata write off every
	// process start.
	if info, err := os.Stat(directory); err == nil && info.Mode().Perm() != 0o700 {
		if err := os.Chmod(directory, 0o700); err != nil {
			return "", fmt.Errorf("set Core ML model cache permissions: %w", err)
		}
	}
	touchProviderArtifactUse(directory)
	return directory, nil
}

// lateOnHostOSBuild reports the macOS product version and kernel build. The Core
// ML compiler ships with the operating system, so an update can change both the
// compiled model and whether the provider is trustworthy. ONNX Runtime reuses a
// cached compiled model whenever the file exists, so the key must carry the
// build. An unreadable value degrades to "unknown", which keeps one shared key
// rather than failing the search.
func lateOnHostOSBuild() string {
	product, productErr := unix.Sysctl("kern.osproductversion")
	build, buildErr := unix.Sysctl("kern.osversion")
	if productErr != nil || buildErr != nil {
		return "unknown"
	}
	return product + "-" + build
}

func lateOnCoreMLCacheKey(model []byte, runtimeVersion string, providerOptions map[string]string) string {
	hash := sha256.New()
	hash.Write([]byte("seek-coreml-cache-v2\x00"))
	writeSemanticHashField(hash, []byte(runtimeVersion))
	writeSemanticHashField(hash, []byte(lateOnHostOSBuild()))
	keys := make([]string, 0, len(providerOptions))
	for key := range providerOptions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		writeSemanticHashField(hash, []byte(key))
		writeSemanticHashField(hash, []byte(providerOptions[key]))
	}
	writeSemanticHashField(hash, model)
	return hex.EncodeToString(hash.Sum(nil)[:16])
}

// lateOnProbeSession runs the fixed probe batch through one session and hands the
// output to consume. The output is only valid inside the call, matching the
// pattern lateOnStaticRowEncoder.Run already uses. One probe output is 3 MB, so a
// caller that compares two providers should copy the first and read the second
// through this.
func lateOnProbeSession(
	ctx context.Context,
	pool *lateOnStaticRowEncoder,
	session *ort.DynamicAdvancedSession,
	ids []int64,
	attention []int64,
	consume func([]float32) error,
) error {
	// Borrow from the encoder's tensor pool. The probe runs twice and each output
	// is 3 MB, so allocating its own would make 6 MB of garbage and then leave
	// the encoder to allocate its first output again on the first real batch.
	// takeOutput needs no session, only the pool fields.
	output, err := pool.takeOutput()
	if err != nil {
		return fmt.Errorf("create probe output: %w", err)
	}
	defer pool.releaseOutput(output)
	if err := runLateOnSessionRows(
		ctx,
		session,
		ids,
		attention,
		semanticModelBatchRows,
		output,
	); err != nil {
		return err
	}
	rows := semanticModelBatchRows * lateOnSequenceLength * semanticEmbeddingDimensions
	return consume(output.GetData()[:rows])
}

// lateOnRunProbe runs the fixed probe batch and returns a copy of every row.
func lateOnRunProbe(
	ctx context.Context,
	pool *lateOnStaticRowEncoder,
	session *ort.DynamicAdvancedSession,
	ids []int64,
	attention []int64,
) ([]float32, error) {
	var copied []float32
	err := lateOnProbeSession(ctx, pool, session, ids, attention, func(output []float32) error {
		copied = append([]float32(nil), output...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return copied, nil
}

// touchProviderArtifactUse records that this compiled model is still wanted.
// Nothing else writes into the directory after the compile and the single
// verdict write, and neither MkdirAll nor Chmod moves a directory's modification
// time, so without this a model used every day ages exactly like an abandoned
// one and the collector removes it — costing a recompile AND a fresh provider
// check. It refreshes at most once a day, so a search does not pay a write.
//
// It skips a rejected model, which this host will never read again and the
// collector must be free to reclaim. That check sits after the interval test, so
// a warm run pays one stat and no verdict read.
func touchProviderArtifactUse(directory string) {
	marker := filepath.Join(directory, providerArtifactUsedFile)
	if info, err := os.Stat(marker); err == nil &&
		time.Since(info.ModTime()) < providerArtifactTouchInterval {
		return
	}
	// Do not refresh the marker for a model this host has stopped using. A
	// rejected verdict means every later run takes the CPU provider without
	// reading the compiled model, so refreshing it would pin tens of megabytes
	// the collector could never reclaim — on exactly the hosts this check exists
	// for. The stat above runs first, so a warm run never reads the verdict here.
	verdict := filepath.Join(directory, lateOnProviderVerdictFile)
	if readLateOnProviderRecord(verdict).verdict == lateOnVerdictRejected {
		return
	}
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	if err := file.Close(); err != nil {
		return
	}
	now := time.Now()
	_ = os.Chtimes(marker, now, now)
}

// lateOnProviderVerdictFile names the cached probe result. It sits in the Core
// ML model cache directory, whose key already covers the model bytes, the ONNX
// Runtime version, the provider options and the operating system build, so the
// verdict expires exactly when any of those change.
const lateOnProviderVerdictFile = "provider-verdict"

// lateOnVerdictLocation remembers where the verdict for the provider in use
// lives. rejectLateOnAcceleratedProvider runs long after selection, on the index
// build path, and this spares it decoding the model again only to rebuild a path
// selection already knew. Selection stores it; an empty value means no provider
// check has run in this process, so there is nothing to reject.
var lateOnVerdictLocation atomic.Value

// rejectLateOnAcceleratedProvider records that the accelerated provider produced
// unusable output on real work.
//
// It does NOT demote on the first fault. One fault writes a counted recheck, and
// the next process measures again rather than trusting a single sample, because
// normalizeSemanticVector also rejects a vector whose norm cancels exactly, which
// a healthy provider can produce. A second fault inside
// lateOnProviderFaultWindow writes the demotion, and only then does the next
// process take the CPU provider without building an accelerated session.
//
// It only acts when the accelerated provider is the one in use, it never
// downgrades a demotion a peer process already settled, and it never fails a
// search.
func rejectLateOnAcceleratedProvider(reason string) {
	if semanticProviderName() != lateOnAcceleratedProviderName {
		return
	}
	path, ok := lateOnVerdictLocation.Load().(string)
	if !ok || path == "" {
		return
	}
	// One bad batch asks for another measurement. A second one, after a probe
	// that passed in between, is a provider that agrees on the fixed row and
	// fails on real work, so demote it for good.
	record := readLateOnProviderRecord(path)
	if record.verdict == lateOnVerdictRejected {
		// A peer process already settled this. Rewriting the record would
		// downgrade its demotion to a first fault.
		return
	}
	// Read and write are not one step. Two searches that fault at the same moment
	// both read the same count and both write one more, so the record advances by
	// one rather than two and the demotion needs one further fault. Faults are
	// rare and the window is a week, so the cost is a delay, never a wrong
	// demotion; a lock here would be paid on every search to shorten an unlikely
	// path.
	faults := record.faults + 1
	verdict := fmt.Sprintf("%s %d %d", lateOnVerdictRecheck, faults, time.Now().Unix())
	if faults >= lateOnVerdictFaultsBeforeRejection {
		verdict = lateOnVerdictRejected
	}
	slog.Debug(
		"Recorded a semantic provider fault",
		"provider", lateOnAcceleratedProviderName,
		"verdict", verdict,
		"reason", reason,
	)
	writeLateOnProviderVerdict(path, verdict)
}

// lateOnProviderRecord is a parsed verdict file. A settled verdict fills verdict
// and leaves faults at zero; a counted recheck does the opposite. A missing,
// malformed or expired record is the zero value, which means "measure again".
type lateOnProviderRecord struct {
	verdict string
	faults  int
	at      int64
}

// readLateOnProviderRecord reads and parses the verdict file once, and is the
// only place that parses it. One reader serves every caller: the file is small,
// but it grew four separate parsers that differed only in which field they
// returned, and between them read it about seven times on a cold start.
//
// A fault older than lateOnProviderFaultWindow expires, which is what stops one
// benign cancellation from costing a probe on every search for ever.
func readLateOnProviderRecord(path string) lateOnProviderRecord {
	stored, err := os.ReadFile(path)
	if err != nil {
		return lateOnProviderRecord{}
	}
	fields := strings.Fields(string(stored))
	if len(fields) == 0 {
		return lateOnProviderRecord{}
	}
	switch fields[0] {
	case lateOnVerdictTrusted, lateOnVerdictRejected:
		return lateOnProviderRecord{verdict: fields[0]}
	case lateOnVerdictRecheck:
	default:
		return lateOnProviderRecord{}
	}
	if len(fields) != 3 {
		return lateOnProviderRecord{}
	}
	faults, err := strconv.Atoi(fields[1])
	if err != nil || faults < 1 {
		return lateOnProviderRecord{}
	}
	seconds, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return lateOnProviderRecord{}
	}
	if time.Since(time.Unix(seconds, 0)) > lateOnProviderFaultWindow {
		return lateOnProviderRecord{}
	}
	return lateOnProviderRecord{faults: faults, at: seconds}
}

// cachedLateOnProviderVerdict reads a recorded verdict. The cache directory is a
// pure function of the model bytes, the provider options, the ONNX Runtime
// version and the operating system build, so this needs no session. Reading it
// before the accelerated session is built keeps a rejected host from compiling
// and destroying a Core ML model on every run.
func cachedLateOnProviderVerdict(model []byte) string {
	directory, err := lateOnRunCoreMLCacheDirectory(model)
	if err != nil {
		return ""
	}
	path := filepath.Join(directory, lateOnProviderVerdictFile)
	lateOnVerdictLocation.Store(path)
	// A recheck, or anything unreadable, means measure again.
	return readLateOnProviderRecord(path).verdict
}

// verifyLateOnAcceleratedProvider reports whether the accelerated session agrees
// with the CPU provider on one fixed row.
//
// ONNX Runtime 1.30.0 returns wrong FP16 output from the Core ML MLProgram path
// on macOS 15 ARM64: https://github.com/microsoft/onnxruntime/issues/32569. The
// session still builds and reports no error, so only its numbers reveal the
// fault. Comparing against the CPU provider on this host avoids a stored
// reference vector, which would have to be re-pinned whenever the model, the
// tokenizer or the run time changes.
//
// No branch here selects a provider from the operating system version. The
// version and build enter the cache key only, through lateOnHostOSBuild, so a
// system update makes this measure again rather than deciding the answer.
//
// Agreement does not always write "trusted". A fault already on the record is
// carried forward with its original timestamp, because a provider that passes
// this row and fails on real work must still be able to reach a demotion, and
// restamping would restart the expiry window on every probe.
func verifyLateOnAcceleratedProvider(
	ctx context.Context,
	pool *lateOnStaticRowEncoder,
	model []byte,
	accelerated *ort.DynamicAdvancedSession,
) (bool, string, error) {
	directory, err := lateOnRunCoreMLCacheDirectory(model)
	if err != nil {
		return true, "", err
	}
	verdictPath := filepath.Join(directory, lateOnProviderVerdictFile)
	lateOnVerdictLocation.Store(verdictPath)
	// This re-read is not the one cachedLateOnProviderVerdict already did. A Core
	// ML compile takes long enough that a peer process can finish its own check
	// and record a verdict while this one waits, and honouring it saves a second
	// probe.
	switch readLateOnProviderRecord(verdictPath).verdict {
	case lateOnVerdictTrusted:
		return true, "cached verdict", nil
	case lateOnVerdictRejected:
		return false, "cached verdict", nil
	}
	ids, attention := lateOnProbeRows()
	acceleratedOutput, err := lateOnRunProbe(ctx, pool, accelerated, ids, attention)
	if err != nil {
		return true, "", fmt.Errorf("probe the accelerated provider: %w", err)
	}
	reference, err := newLateOnStaticSession(model, func(options *ort.SessionOptions) error {
		// Match the thread count the CPU provider would really use. A single
		// thread made the reference slower than the provider it judges, and this
		// runs while the user waits.
		if err := configureLateOnCPUProvider(
			options,
			lateOnCPUIntraOpThreads(runtime.GOMAXPROCS(0)),
		); err != nil {
			return err
		}
		// This session runs once and is destroyed. Its arena would keep the
		// activation memory for a full batch — measured at about 617 MiB — for
		// the life of the process, on hosts that then do all their real work
		// through the accelerator and would never have grown one.
		if err := options.SetCpuMemArena(false); err != nil {
			return fmt.Errorf("disable the reference CPU arena: %w", err)
		}
		return options.SetMemPattern(false)
	})
	if err != nil {
		return true, "", fmt.Errorf("build the reference session: %w", err)
	}
	// Read the record again here, not at the top: the accelerated probe and this
	// session build take long enough for a peer process to record a fault, and
	// that fault must survive this write.
	prior := readLateOnProviderRecord(verdictPath)
	// Compare the reference output in place. Copying it would hold a second 3 MB
	// batch alongside the accelerated one for no gain.
	agree, difference := false, ""
	err = lateOnProbeSession(ctx, pool, reference, ids, attention, func(output []float32) error {
		agree, difference = lateOnOutputsAgree(acceleratedOutput, output)
		return nil
	})
	// Release the reference session before the caller measures free memory for
	// its inference concurrency budget.
	closeErr := reference.Destroy()
	if err != nil {
		return true, "", fmt.Errorf("probe the reference provider: %w", err)
	}
	if closeErr != nil {
		return true, "", fmt.Errorf("release the reference session: %w", closeErr)
	}
	verdict := lateOnVerdictRejected
	reason := difference
	if agree {
		verdict = lateOnVerdictTrusted
		reason = "matches the CPU provider"
		if prior.faults > 0 {
			// Keep the count. Clearing it here is what would stop a provider
			// that passes this row and fails real work from ever being demoted.
			// Keep the original timestamp. Restamping it here would restart the
			// window on every passing probe, so the count would never expire and
			// this host would probe on every search for ever.
			verdict = fmt.Sprintf(
				"%s %d %d",
				lateOnVerdictRecheck,
				prior.faults,
				prior.at,
			)
			reason = "matches the CPU provider after an earlier fault"
		}
	}
	writeLateOnProviderVerdict(verdictPath, verdict)
	// Release the probe's own garbage now. The index build samples free memory to
	// size its inference concurrency, and it samples after this returns, so
	// probe buffers still held would make that reading low and then rise under
	// it. Two output batches plus one copy is about 9 MiB, against a probe that
	// already cost a session build and two inferences.
	runtime.GC()
	return agree, reason, nil
}

// writeLateOnProviderVerdict stores the verdict beside the compiled model. A
// write failure only costs the next process another probe, so it never fails the
// search.
func writeLateOnProviderVerdict(path string, verdict string) {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".verdict-*")
	if err != nil {
		slog.Debug("Could not record the semantic provider verdict", "error", err)
		return
	}
	name := temporary.Name()
	_, writeErr := temporary.WriteString(verdict + "\n")
	closeErr := temporary.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		slog.Debug("Could not record the semantic provider verdict", "error", errors.Join(writeErr, closeErr))
		return
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		slog.Debug("Could not record the semantic provider verdict", "error", err)
		return
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		slog.Debug("Could not record the semantic provider verdict", "error", err)
	}
}
