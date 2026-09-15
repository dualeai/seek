//go:build cgo && darwin && arm64

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

func TestLateOnCoreMLCacheDirectoryTracksModel(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", cacheRoot)
	options := lateOnCoreMLProviderOptions()
	version := ort.GetVersion()
	first, err := lateOnCoreMLCacheDirectoryForKey(
		lateOnCoreMLCacheKey([]byte("first model"), version, options),
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := lateOnCoreMLCacheDirectoryForKey(
		lateOnCoreMLCacheKey([]byte("second model"), version, options),
	)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different models shared a Core ML cache directory")
	}
	if !filepath.IsAbs(first) || filepath.Dir(filepath.Dir(filepath.Dir(first))) != filepath.Join(cacheRoot, "reranker") {
		t.Fatalf("unexpected Core ML cache path %q", first)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("Core ML cache mode=%o, want 700", info.Mode().Perm())
	}
}

func TestCacheDirectoryRefreshesItsAccessMarker(t *testing.T) {
	// Without this the collector ages a compiled model on a timestamp nothing
	// refreshes, so a model in daily use is removed like an abandoned one.
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	directory, err := lateOnCoreMLCacheDirectoryForKey("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, providerArtifactUsedFile)
	first, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("resolving the directory did not record its use: %v", err)
	}
	// A second resolution inside the interval must not rewrite it.
	if _, err := lateOnCoreMLCacheDirectoryForKey("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().Equal(first.ModTime()) {
		t.Fatal("the marker was rewritten inside the touch interval")
	}
	// Once it is older than the interval it must be refreshed.
	stale := time.Now().Add(-providerArtifactTouchInterval - time.Hour)
	if err := os.Chtimes(marker, stale, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := lateOnCoreMLCacheDirectoryForKey("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	third, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !third.ModTime().After(stale) {
		t.Fatal("a stale marker was not refreshed")
	}
}

func TestLateOnCoreMLCacheKeyTracksRuntimeAndProvider(t *testing.T) {
	model := []byte("model")
	options := lateOnCoreMLProviderOptions()
	base := lateOnCoreMLCacheKey(model, "runtime one", options)
	if base == lateOnCoreMLCacheKey(model, "runtime two", options) {
		t.Fatal("different ONNX Runtime versions shared a Core ML cache key")
	}
	changedOptions := lateOnCoreMLProviderOptions()
	changedOptions["MLComputeUnits"] = "CPUOnly"
	if base == lateOnCoreMLCacheKey(model, "runtime one", changedOptions) {
		t.Fatal("different Core ML provider settings shared a cache key")
	}
	reorderedOptions := map[string]string{}
	for _, key := range []string{
		"RequireStaticInputShapes",
		"ModelFormat",
		"EnableOnSubgraphs",
		"MLComputeUnits",
	} {
		reorderedOptions[key] = options[key]
	}
	if base != lateOnCoreMLCacheKey(model, "runtime one", reorderedOptions) {
		t.Fatal("Core ML cache key depends on map iteration order")
	}
}

func TestLateOnRejectedVerdictSelectsCPUProvider(t *testing.T) {
	rawModel, _, verdict := newModelWithVerdictPath(t)
	lateOn, ok := rawModel.(*lateOnModel)
	if !ok {
		t.Fatalf("model type is %T, want *lateOnModel", rawModel)
	}
	if err := os.WriteFile(verdict, []byte(lateOnVerdictRejected+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	encoder, err := lateOn.semanticRowEncoder(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := semanticProviderName(); got != lateOnCPUProviderName {
		after, readErr := os.ReadFile(verdict)
		t.Fatalf("provider = %q, want %q; verdict %q content=%q err=%v",
			got, lateOnCPUProviderName, verdict, string(after), readErr)
	}
	// A CPU call must reserve CPU in the shared gate. Zero is the accelerator
	// reservation and would let the scheduler oversubscribe the host.
	if encoder.CallCPUs() < 1 {
		t.Fatalf("CallCPUs() = %d, want at least 1 for the CPU provider", encoder.CallCPUs())
	}
	// The encoder must still work after the demotion.
	ids, attention := lateOnProbeRows()
	if _, err := lateOnRunProbe(t.Context(), encoder, encoder.session, ids, attention); err != nil {
		t.Fatalf("demoted encoder failed to run: %v", err)
	}
}

func TestFolderBuildFaultRecordsProviderRejection(t *testing.T) {
	verdict := pinAcceleratedProviderForTest(t)
	// A model that fails the way a wrong provider does: output with no usable
	// direction, reported through normalizeSemanticVector.
	var degenerate semanticVector
	faultErr := normalizeSemanticVector(&degenerate)
	if !errors.Is(faultErr, errDegenerateSemanticVector) {
		t.Fatalf("normalize error = %v, want a degenerate-output error", faultErr)
	}
	buildFolderWithFailingModel(t, fmt.Errorf("semantic unit 0: %w", faultErr))

	stored, err := os.ReadFile(verdict)
	if err != nil {
		t.Fatalf("the build did not record a provider rejection: %v", err)
	}
	if fields := strings.Fields(string(stored)); len(fields) != 3 ||
		fields[0] != lateOnVerdictRecheck || fields[1] != "1" {
		t.Fatalf("first fault verdict = %q, want one stamped fault", string(stored))
	}
	// A second fault, with the recheck already on disk, demotes for good.
	rejectLateOnAcceleratedProvider(faultErr.Error())
	stored, err = os.ReadFile(verdict)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stored)) != lateOnVerdictRejected {
		t.Fatalf("second fault verdict = %q, want %q", string(stored), lateOnVerdictRejected)
	}
}

func TestFolderBuildOtherFaultKeepsProvider(t *testing.T) {
	// A build can fail for reasons that say nothing about the provider. Those
	// must not demote it, or one full disk would cost every later search.
	verdict := pinAcceleratedProviderForTest(t)
	buildFolderWithFailingModel(t, errors.New("no space left on device"))
	if _, err := os.Stat(verdict); !os.IsNotExist(err) {
		t.Fatalf("an unrelated build failure demoted the provider: %v", err)
	}
}
func TestRunCoreMLCacheDirectoryRefusesUninitializedRuntime(t *testing.T) {
	// The key carries the ONNX Runtime version, which is empty until the
	// environment is ready. Caching an empty one would send the verdict writer
	// and reader to different directories, and the only symptom would be a probe
	// that repeats on every run.
	if ort.GetVersion() != "" {
		t.Skip("the run time is already initialized in this test binary")
	}
	if _, err := lateOnRunCoreMLCacheDirectory([]byte("model")); err == nil {
		t.Fatal("an uninitialized run time produced a cache directory")
	}
}

func TestRunCoreMLCacheDirectoryIsStableAcrossCacheRoots(t *testing.T) {
	if _, err := newLateOnSemanticModel(t.Context()); err != nil {
		t.Fatal(err)
	}
	model, err := decodeLateOnAsset(lateOnCompressedModel)
	if err != nil {
		t.Fatal(err)
	}
	// The resolved key is reused for the whole process, but the cache root is
	// read on every call, so a changed root must still be honoured.
	firstRoot := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", firstRoot)
	first, err := lateOnRunCoreMLCacheDirectory(model)
	if err != nil {
		t.Fatal(err)
	}
	secondRoot := t.TempDir()
	t.Setenv("SEEK_CACHE_DIR", secondRoot)
	second, err := lateOnRunCoreMLCacheDirectory(model)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, firstRoot) || !strings.HasPrefix(second, secondRoot) {
		t.Fatalf("directories ignore the cache root: %q then %q", first, second)
	}
	if filepath.Base(first) != filepath.Base(second) {
		t.Fatalf("the resolved key changed between calls: %q then %q", first, second)
	}
}

func TestRejectedModelStopsRefreshingItsAccessMarker(t *testing.T) {
	// A rejected host never reads the compiled model again, so the collector must
	// be able to age it out. Refreshing the marker would pin it for ever.
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	const key = "fedcba9876543210fedcba9876543210"
	directory, err := lateOnCoreMLCacheDirectoryForKey(key)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, providerArtifactUsedFile)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	verdict := filepath.Join(directory, lateOnProviderVerdictFile)
	if err := os.WriteFile(verdict, []byte(lateOnVerdictRejected+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lateOnCoreMLCacheDirectoryForKey(key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a rejected model kept refreshing its access marker: %v", err)
	}
	// A trusted model still refreshes it.
	if err := os.WriteFile(verdict, []byte(lateOnVerdictTrusted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lateOnCoreMLCacheDirectoryForKey(key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a trusted model stopped recording its use: %v", err)
	}
}

func TestSearchFallbacksRecordAProviderFault(t *testing.T) {
	// Every search-path fallback must report a provider fault, not only the
	// re-rank one. A wrong provider on an already-built index otherwise drops
	// every search to text ranking and never records itself.
	for _, name := range []string{"joined search", "query preparation", "re-ranking"} {
		t.Run(name, func(t *testing.T) {
			path := pinAcceleratedProviderForTest(t)
			recordSemanticProviderFault(fmt.Errorf("%s: %w", name, errDegenerateSemanticVector))
			if readLateOnProviderRecord(path).faults != 1 {
				t.Fatalf("%s did not record a provider fault", name)
			}
		})
	}
}

func TestRecordSemanticProviderFaultOnlyActsOnProviderFaults(t *testing.T) {
	// Every search-path fallback funnels through this, so it decides whether a
	// wrong provider on an already-built index is ever recorded.
	path := pinAcceleratedProviderForTest(t)

	recordSemanticProviderFault(errors.New("no space left on device"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an unrelated failure recorded a provider fault: %v", err)
	}
	recordSemanticProviderFault(fmt.Errorf("scoring: %w", errDegenerateSemanticVector))
	if readLateOnProviderRecord(path).faults != 1 {
		t.Fatal("a provider fault on the search path was not recorded")
	}
}

// pinAcceleratedProviderForTest arranges the state a real selection leaves — the
// accelerated provider in use, with its verdict at a known path — and returns
// that path. Five tests need it, and the save and restore is easy to get subtly
// wrong in each of them.
func pinAcceleratedProviderForTest(t *testing.T) string {
	t.Helper()
	verdict := filepath.Join(t.TempDir(), lateOnProviderVerdictFile)
	previousProvider := lateOnSelectedProvider.Load()
	previousLocation := lateOnVerdictLocation.Load()
	t.Cleanup(func() {
		if previousProvider != nil {
			lateOnSelectedProvider.Store(previousProvider)
		}
		if previousLocation != nil {
			lateOnVerdictLocation.Store(previousLocation)
		}
	})
	lateOnSelectedProvider.Store(lateOnAcceleratedProviderName)
	lateOnVerdictLocation.Store(verdict)
	return verdict
}

// buildFolderWithFailingModel runs a real folder index build whose model returns
// failure, and requires the build to keep lexical search rather than fail.
func buildFolderWithFailingModel(t *testing.T, embedErr error) {
	t.Helper()
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// semantic folder text\n")
	plan := planFolderTestCorpus(t, folder)
	future := newSemanticModelFuture(func(context.Context) (semanticModel, error) {
		return semanticBuildTestEmbedder{embed: func(
			context.Context,
			[]semanticUnit,
		) ([]semanticUnitEmbedding, error) {
			return nil, embedErr
		}}, nil
	})
	t.Cleanup(func() { _ = future.Close() })
	execution := searchExecution{policy: defaultSearchPolicy(), model: future}
	if _, err := ensureFolderCorpusFreshWithExecution(t.Context(), plan, execution); err != nil {
		t.Fatalf("the build must keep lexical search, not fail: %v", err)
	}
}

func TestReadLateOnProviderRecordRejectsMalformedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), lateOnProviderVerdictFile)
	if readLateOnProviderRecord(path).faults != 0 {
		t.Fatal("a missing record counted a fault")
	}
	now := time.Now().Unix()
	for _, stored := range []string{
		lateOnVerdictTrusted,
		lateOnVerdictRejected,
		"",
		lateOnVerdictRecheck,
		fmt.Sprintf("%s 1", lateOnVerdictRecheck),
		fmt.Sprintf("%s not-a-number %d", lateOnVerdictRecheck, now),
		fmt.Sprintf("%s -3 %d", lateOnVerdictRecheck, now),
		fmt.Sprintf("%s 0 %d", lateOnVerdictRecheck, now),
		fmt.Sprintf("%s 1 not-a-time", lateOnVerdictRecheck),
	} {
		if err := os.WriteFile(path, []byte(stored+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readLateOnProviderRecord(path).faults; got != 0 {
			t.Fatalf("record %q counted %d faults, want 0", stored, got)
		}
	}
	if err := os.WriteFile(path, fmt.Appendf(nil, "%s 1 %d\n", lateOnVerdictRecheck, now), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readLateOnProviderRecord(path).faults; got != 1 {
		t.Fatalf("counted %d faults, want 1", got)
	}
	// A lone old fault expires, so a host that saw one benign cancellation stops
	// paying for a probe on every search.
	expired := time.Now().Add(-lateOnProviderFaultWindow - time.Hour).Unix()
	if err := os.WriteFile(path, fmt.Appendf(nil, "%s 1 %d\n", lateOnVerdictRecheck, expired), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readLateOnProviderRecord(path).faults; got != 0 {
		t.Fatalf("an expired fault counted %d, want 0", got)
	}
}

func TestProviderFaultCountSurvivesAPassingProbe(t *testing.T) {
	// A provider that agrees on the fixed probe row but fails on real work must
	// still earn a demotion. If a passing probe cleared the count, the two
	// records would alternate for ever and nothing would change.
	rawModel, _, path := newModelWithVerdictPath(t)
	stamped := fmt.Sprintf("%s 1 %d\n", lateOnVerdictRecheck, time.Now().Unix())
	if err := os.WriteFile(path, []byte(stamped), 0o600); err != nil {
		t.Fatal(err)
	}
	lateOn, ok := rawModel.(*lateOnModel)
	if !ok {
		t.Fatalf("model type is %T, want *lateOnModel", rawModel)
	}
	if _, err := lateOn.semanticRowEncoder(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := semanticProviderName(); got != lateOnAcceleratedProviderName {
		t.Skipf("this host selected %q, so no probe ran", got)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if readLateOnProviderRecord(path).faults != 1 {
		t.Fatalf("a passing probe cleared the fault count: %q", string(stored))
	}
}

func TestRejectDoesNotDowngradeASettledRejection(t *testing.T) {
	path := pinAcceleratedProviderForTest(t)
	if err := os.WriteFile(path, []byte(lateOnVerdictRejected+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rejectLateOnAcceleratedProvider("a later fault")
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stored)) != lateOnVerdictRejected {
		t.Fatalf("a peer's settled rejection was downgraded to %q", string(stored))
	}
}

func TestRecheckVerdictMakesTheNextProcessMeasureAgain(t *testing.T) {
	_, model, path := newModelWithVerdictPath(t)
	for _, test := range []struct {
		stored string
		want   string
	}{
		{stored: lateOnVerdictTrusted, want: lateOnVerdictTrusted},
		{stored: lateOnVerdictRejected, want: lateOnVerdictRejected},
		// A recheck and an unreadable value both mean measure again, so one bad
		// batch cannot settle the question on its own.
		{stored: lateOnVerdictRecheck + " 1 1", want: ""},
		{stored: "something else", want: ""},
		{stored: "", want: ""},
	} {
		if err := os.WriteFile(path, []byte(test.stored+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := cachedLateOnProviderVerdict(model); got != test.want {
			t.Fatalf("stored %q gave %q, want %q", test.stored, got, test.want)
		}
	}
}

// newModelWithVerdictPath builds the model under a fresh cache, then returns the
// decoded model bytes and the path its provider verdict will live at. Four tests
// need exactly this preamble, and it has an ordering trap: the cache key carries
// the ONNX Runtime version, which is empty until the model initializes the run
// time, so resolving the directory first yields a different key.
func newModelWithVerdictPath(t *testing.T) (semanticModel, []byte, string) {
	t.Helper()
	t.Setenv("SEEK_CACHE_DIR", t.TempDir())
	built, err := newLateOnSemanticModel(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := built.Close(); err != nil {
			t.Errorf("close model: %v", err)
		}
	})
	model, err := decodeLateOnAsset(lateOnCompressedModel)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := lateOnRunCoreMLCacheDirectory(model)
	if err != nil {
		t.Fatal(err)
	}
	return built, model, filepath.Join(directory, lateOnProviderVerdictFile)
}
