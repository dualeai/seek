//go:build cgo && darwin && arm64

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	ort "github.com/yalue/onnxruntime_go"
)

const lateOnProviderHasAccelerator = true

// lateOnAcceleratedProviderName names the accelerated provider in logs and in
// the values of SEEK_PROVIDER. Change it when the provider options change
// meaning.
const lateOnAcceleratedProviderName = "coreml-mlprogram-static"

// configureLateOnProvider adds the Core ML provider to options, with its
// model cache directory. The CPU part of the session keeps one thread, so it
// ignores the CPU count of a call.
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

// lateOnCoreMLProviderOptions are the Core ML options. EnableOnSubgraphs keeps
// its default: the model has no If, Loop or Scan node, so setting it would
// change only the cache key.
func lateOnCoreMLProviderOptions() map[string]string {
	return map[string]string{
		"ModelFormat":              "MLProgram",
		"MLComputeUnits":           "ALL",
		"RequireStaticInputShapes": "1",
	}
}

// lateOnRunCoreMLCacheDirectory computes the cache key once for this process
// and returns the cache directory. The key hashes the whole decoded model,
// which takes about 10 ms. Three call sites need the directory on one cold
// start: the session configuration, the provider check, and the cached-verdict
// read.
//
// The key also carries ort.GetVersion(), which is empty until
// ensureLateOnEnvironment runs. Every caller reaches this through
// newLateOnRowEncoder, which a *lateOnModel owns, and that model cannot exist
// before the environment is ready. An empty version is an error, so a broken
// order fails at once. A key with an empty version would send writers and
// readers to different directories. The only symptom would be a probe that
// repeats on every run with no message.
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

// lateOnHostOSBuild reports the macOS product version and kernel build. The
// Core ML compiler ships with the operating system, so an update can change
// both the compiled model and whether the provider is trustworthy. ONNX Runtime
// reuses a cached compiled model whenever the file exists, so the key must
// carry the build. An unreadable value degrades to "unknown", which keeps one
// shared key rather than failing the search.
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
	for _, key := range slices.Sorted(maps.Keys(providerOptions)) {
		writeSemanticHashField(hash, []byte(key))
		writeSemanticHashField(hash, []byte(providerOptions[key]))
	}
	writeSemanticHashField(hash, model)
	return hex.EncodeToString(hash.Sum(nil)[:16])
}

// lateOnProbeSession runs the fixed probe batch through one session and hands
// the output to consume. The output is only valid inside the call, matching the
// pattern lateOnStaticRowEncoder.Run already uses. One probe output is 3 MB, so
// a caller that compares two providers should copy the first and read the
// second through this.
func lateOnProbeSession(
	ctx context.Context,
	pool *lateOnStaticRowEncoder,
	session *ort.DynamicAdvancedSession,
	ids []int64,
	attention []int64,
	consume func([]float32) error,
) error {
	// Borrow from the encoder's tensor pool. The probe runs twice and each
	// output is 3 MB. Its own outputs would make 6 MB of garbage, and the
	// encoder would allocate its first output again on the first real batch.
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

// providerArtifactTouchInterval bounds how often a search rewrites the
// providerArtifactUsedFile marker. It lives here, apart from the name it
// bounds, because this file holds the only writer. provider_artifact.go
// compiles on every target, and there the interval would be unused.
const providerArtifactTouchInterval = 24 * time.Hour

// touchProviderArtifactUse records that this compiled model is still wanted.
// After the compile, only the rare verdict writes change the directory.
// MkdirAll and Chmod do not change its modification time. Without the marker,
// a model used every day ages like an unused one. The collector then removes
// it, which costs a new compile AND a new provider check. It refreshes at most
// once a day, so most searches pay no write.
//
// It skips a rejected model, which this host does not read again and the
// collector must be free to remove. That check comes after the interval test,
// so a warm run pays one stat and no verdict read.
func touchProviderArtifactUse(directory string) {
	marker := filepath.Join(directory, providerArtifactUsedFile)
	if info, err := os.Stat(marker); err == nil &&
		time.Since(info.ModTime()) < providerArtifactTouchInterval {
		return
	}
	// A rejected verdict means every later run takes the CPU provider and does
	// not read the compiled model. A refresh would keep tens of megabytes that
	// the collector could never remove, on the hosts that this check is for.
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

// lateOnVerdictRecheck, lateOnVerdictFaultsBeforeRejection and
// lateOnProviderFaultWindow complete the on-disk record format whose two shared
// spellings semantic_encoder.go declares. They live here because this file
// holds the only parser and the only writer. Do not move them beside those
// two. That file also compiles where no accelerator exists, and there these
// three are unused. A second accelerated target would need a file tagged for
// both targets, not the shared encoder.

// lateOnVerdictRecheck records real work that produced unusable output the
// fixed probe batch does not reproduce. The record reads "recheck <count>
// <unix-seconds>", where the time is when the count last changed. One is
// evidence, not proof: normalizeSemanticVector also rejects a vector whose norm
// cancels exactly, which a healthy provider can produce. The next process
// measures again rather than trusting one sample.
//
// The count must survive a passing probe. Else a provider that agrees on the
// probe and fails on real work would alternate between the two records
// forever, and never get a demotion. The continuous guards would then report
// the same fault on every build, and nothing would change.
//
// Keep the spelling stable, the way the shared verdicts are kept: a machine
// that upgrades Seek keeps the record its previous version wrote.
const lateOnVerdictRecheck = "recheck"

// lateOnVerdictFaultsBeforeRejection is how many recorded faults stop this
// host from using the accelerated provider. A command records at most one
// (semanticModelFuture.recordFault).
const lateOnVerdictFaultsBeforeRejection = 2

// lateOnProviderFaultWindow is how long one fault stays on the record. Two
// faults inside it demote the provider. A lone fault outside it expires. So a
// host that saw one benign fault (a vector whose norm cancels to zero) returns
// to the cached-verdict path. It then does not pay a second session and two
// full batches on every search.
const lateOnProviderFaultWindow = 7 * 24 * time.Hour

// lateOnProviderVerdictFile names the cached probe result. It is in the Core
// ML model cache directory. The key of that directory covers the model bytes,
// the ONNX Runtime version, the provider options and the operating system
// build. So the verdict expires when any of those change.
const lateOnProviderVerdictFile = "provider-verdict"

// lateOnVerdictLocation holds the path of the verdict for the provider in use.
// rejectLateOnAcceleratedProvider runs long after selection, after a vector
// build, scan, or ranking fault (semanticModelFuture.recordFault). This path
// saves it from decoding the model again to rebuild a path that selection
// knew. Selection stores it. An empty value means that selection read no
// verdict in this process (a pinned provider), so there is nothing to reject.
var lateOnVerdictLocation atomic.Value

// rejectLateOnAcceleratedProvider records that the accelerated provider
// produced unusable output on real work.
//
// It does NOT demote on the first fault. One fault writes a counted recheck,
// and the next process measures again. One sample is not proof:
// normalizeSemanticVector also rejects a vector whose norm cancels to zero,
// which a healthy provider can produce. A second fault inside
// lateOnProviderFaultWindow writes the demotion. Only then does the next
// process take the CPU provider without an accelerated session.
//
// It acts only when the accelerated provider is the one in use. It never
// changes a demotion that a peer process already wrote, and it never fails a
// search.
func rejectLateOnAcceleratedProvider(reason string) {
	if semanticProviderName() != lateOnAcceleratedProviderName {
		return
	}
	path, ok := lateOnVerdictLocation.Load().(string)
	if !ok || path == "" {
		return
	}
	// One fault asks for another measurement. A second one, after a probe
	// that passed in between, shows a provider that agrees on the probe batch
	// and fails on real work. So demote it for good.
	record := readLateOnProviderRecord(path)
	if record.verdict == lateOnVerdictRejected {
		// A peer process already settled this. Rewriting the record would
		// downgrade its demotion to a first fault.
		return
	}
	// Read and write are not one step. Two searches that fault at the same
	// moment both read the same count and both write one more. The record then
	// goes up by one, not two, and the demotion needs one more fault. Faults
	// are rare and the window is a week, so the cost is a delay, never a wrong
	// demotion. So this path takes no lock: a reader always sees a whole
	// record, because each write is a rename (writeLateOnProviderVerdict).
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

// lateOnProviderRecord is a parsed verdict file. A settled verdict fills
// verdict and leaves faults at zero; a counted recheck does the opposite. A
// missing, malformed or expired record is the zero value, which means "measure
// again".
type lateOnProviderRecord struct {
	verdict string
	faults  int
	at      int64
}

// readLateOnProviderRecord reads and parses the verdict file once, and is the
// only place that parses it. One reader serves every caller, so each caller
// reads the same record format.
//
// A fault older than lateOnProviderFaultWindow expires. So one benign fault
// does not cost a probe on every search forever.
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

// cachedLateOnProviderVerdict reads a recorded verdict. The cache key
// depends only on the model bytes, the provider options, the ONNX Runtime
// version and the operating system build. So this needs no session. It runs
// before the accelerated session exists, so a rejected host does not compile
// and destroy a Core ML model on every run.
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

// verifyLateOnAcceleratedProvider reports whether selection keeps the
// accelerated session. It keeps it when the session agrees with the CPU
// provider on the fixed probe batch and no peer process rejected it.
//
// ONNX Runtime 1.30.0 returns wrong FP16 output from the Core ML MLProgram path
// on macOS 15 ARM64: https://github.com/microsoft/onnxruntime/issues/32569. The
// session still builds and reports no error, so only its numbers show the
// fault. A comparison with the CPU provider on this host needs no stored
// reference vector. Such a vector would need an update whenever the model, the
// tokenizer or the run time changes.
//
// No branch here selects a provider from the operating system version. The
// version and build enter the cache key only, through lateOnHostOSBuild, so a
// system update makes this measure again rather than deciding the answer.
//
// Agreement does not always write "trusted": lateOnProbeVerdict gives the
// record to store.
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
	// This read is not the one that cachedLateOnProviderVerdict did. During a
	// Core ML compile, a peer process can finish its own check and record a
	// verdict. Using that verdict saves a second probe.
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
		// The probe is one call while the user waits, so it uses the
		// interactive thread count. On a single thread, the reference is
		// slower than the provider it judges.
		if err := configureLateOnCPUProvider(
			options,
			lateOnCPUInteractiveThreads(runtime.GOMAXPROCS(0)),
		); err != nil {
			return err
		}
		// This session runs once and is destroyed. Its arena would keep the
		// activation memory of a full batch (measured at about 617 MiB) for
		// the life of the process. These hosts then do all their real work
		// through the accelerator and would never grow such an arena.
		if err := options.SetCpuMemArena(false); err != nil {
			return fmt.Errorf("disable the reference CPU arena: %w", err)
		}
		return options.SetMemPattern(false)
	})
	if err != nil {
		return true, "", fmt.Errorf("build the reference session: %w", err)
	}
	// Compare the reference output in place. Copying it would hold a second
	// 3 MB batch alongside the accelerated one for no gain.
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
	// Read the record again here, not at the top. A peer process can record a
	// fault or a rejection while the two probes and the session build run. That
	// record must survive this write. A peer record that lands between this
	// read and the rename is lost, as in rejectLateOnAcceleratedProvider. That
	// costs a delay: the next faults record it again.
	prior := readLateOnProviderRecord(verdictPath)
	verdict, keep, reason := lateOnProbeVerdict(agree, difference, prior)
	writeLateOnProviderVerdict(verdictPath, verdict)
	// Release the probe's own garbage now. The index build samples free memory
	// after this returns, to size its inference concurrency. Probe buffers
	// still held would make that reading low, and the free memory would then
	// rise under it. Two output batches plus one copy is about 9 MiB, against
	// a probe that already cost a session build and two inferences.
	runtime.GC()
	return keep, reason, nil
}

// lateOnProbeVerdict gives the record that the provider probe stores, whether
// selection keeps the accelerated provider, and the reason to log. It uses
// the probe result and the record on disk before the write. It reads and
// writes nothing. So a test can check the rule on every host that builds this
// file, whatever the probe result on that host.
//
// Disagreement rejects the provider, and the difference is the reason.
// Agreement with no fault on the record trusts it. Agreement after a fault
// keeps the count. Without the count, a provider that passes the probe batch
// and fails on real work never gets a demotion. It also keeps the original
// timestamp. A new timestamp would start the window again on every passing
// probe. The count would then never expire, and this host would probe on
// every search. Agreement after a peer process rejected the provider keeps the
// rejection, as rejectLateOnAcceleratedProvider does. That peer saw two faults
// on real work or a probe that disagreed, and one passing probe clears
// neither. Selection keeps the provider only when the record is not a
// rejection.
func lateOnProbeVerdict(
	agree bool,
	difference string,
	prior lateOnProviderRecord,
) (verdict string, keep bool, reason string) {
	if !agree {
		return lateOnVerdictRejected, false, difference
	}
	if prior.verdict == lateOnVerdictRejected {
		return lateOnVerdictRejected, false, "a peer process rejected the provider during the probe"
	}
	if prior.faults > 0 {
		return fmt.Sprintf("%s %d %d", lateOnVerdictRecheck, prior.faults, prior.at), true,
			"matches the CPU provider after an earlier fault"
	}
	return lateOnVerdictTrusted, true, "matches the CPU provider"
}

// writeLateOnProviderVerdict stores the verdict beside the compiled model. A
// write failure only costs the next process another probe, so it never fails
// the search.
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
