//go:build cgo && darwin && arm64

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/sys/unix"
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
	encoder, err := lateOn.semanticRowEncoder(t.Context(), false)
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

func TestRunCoreMLCacheDirectoryIsStableAcrossCacheRoots(t *testing.T) {
	// The model initializes the run time, which the cache key reads.
	semantic, err := newLateOnSemanticModel(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = semantic.Close() })
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
	// A rejected host never reads the compiled model again, so the collector
	// must be able to age it out. Refreshing the marker would pin it for ever.
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
	// A wrong provider on an already-built index makes the search fall back to
	// strict order. The fallback must record one provider fault, or the
	// provider never earns a demotion. The query model, the re-rank model and
	// the vector scan each fail with degenerate output on the second search;
	// the first search builds the vectors with a working model.
	for _, test := range []struct {
		name string
		fail func(*hybridTestScorer, error)
	}{
		{name: "query preparation", fail: func(scorer *hybridTestScorer, err error) { scorer.prepareErr = err }},
		{name: "re-ranking", fail: func(scorer *hybridTestScorer, err error) { scorer.scoreErr = err }},
		// A zero query token passes the query preparation and fails in the
		// scan's token normalization.
		{name: "vector scan", fail: func(scorer *hybridTestScorer, _ error) { scorer.queryVector = &semanticVector{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireTools(t)
			setTestUserCache(t)
			folder := t.TempDir()
			writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
			path := pinAcceleratedProviderForTest(t)
			search := func(scorer *hybridTestScorer) {
				t.Helper()
				output, err := seekWith(t, modelFactory(scorer), "alpha beta", folder)
				if err != nil || !strings.Contains(output, "strict.go") {
					t.Fatalf("output=%q err=%v, want the strict file", output, err)
				}
			}
			search(&hybridTestScorer{})
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("the build with a working model recorded a provider fault: %v", err)
			}
			failing := &hybridTestScorer{}
			test.fail(failing, fmt.Errorf("%s: %w", test.name, errDegenerateSemanticVector))
			search(failing)
			if failing.embedCalls.Load() != 0 {
				t.Fatalf("the second search embedded %d batches, want the built index", failing.embedCalls.Load())
			}
			if record := readLateOnProviderRecord(path); record.faults != 1 {
				t.Fatalf("provider record=%+v, want one fault from the %s fallback", record, test.name)
			}
		})
	}
}

func TestRecordFaultOnlyActsOnProviderFaults(t *testing.T) {
	// Every search-path fallback funnels through this, so it decides whether a
	// wrong provider on an already-built index is ever recorded.
	path := pinAcceleratedProviderForTest(t)
	future := &semanticModelFuture{}

	future.recordFault(errors.New("no space left on device"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an unrelated failure recorded a provider fault: %v", err)
	}
	future.recordFault(fmt.Errorf("scoring: %w", errDegenerateSemanticVector))
	if readLateOnProviderRecord(path).faults != 1 {
		t.Fatal("a provider fault on the search path was not recorded")
	}
}

// pinAcceleratedProviderForTest arranges the state a real selection leaves —
// the accelerated provider in use, with its verdict at a known path — and
// returns that path. Many tests need it, and the save and restore is easy to
// get subtly wrong in each of them.
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

// buildFolderWithFailingModel runs a real folder index build whose model
// returns failure, and requires the build to keep lexical search rather than
// fail.
func buildFolderWithFailingModel(t *testing.T, embedErr error) {
	t.Helper()
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// semantic folder text\n")
	plan := planFolderTestCorpus(t, folder)
	execution := testSemanticSearchExecution(t, modelFactory(semanticBuildTestEmbedder{embed: func(
		context.Context,
		[]semanticUnit,
	) ([]semanticUnitEmbedding, error) {
		return nil, embedErr
	}}))
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
	// A lone old fault expires, so a host that saw one benign cancellation
	// stops paying for a probe on every search.
	expired := time.Now().Add(-lateOnProviderFaultWindow - time.Hour).Unix()
	if err := os.WriteFile(path, fmt.Appendf(nil, "%s 1 %d\n", lateOnVerdictRecheck, expired), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readLateOnProviderRecord(path).faults; got != 0 {
		t.Fatalf("an expired fault counted %d, want 0", got)
	}
}

func TestLateOnProbeVerdict(t *testing.T) {
	// A provider that agrees on the fixed probe batch but fails on real work
	// must still earn a demotion. If a passing probe cleared the count, the two
	// records would alternate for ever and nothing would change.
	const at = 1_700_000_000
	fault := lateOnProviderRecord{faults: 1, at: at}
	for _, test := range []struct {
		name    string
		agree   bool
		prior   lateOnProviderRecord
		verdict string
		keep    bool
	}{
		{
			name:    "a passing probe keeps the fault and its time",
			agree:   true,
			prior:   fault,
			verdict: fmt.Sprintf("%s 1 %d", lateOnVerdictRecheck, at),
			keep:    true,
		},
		{name: "a passing probe with no fault trusts", agree: true, verdict: lateOnVerdictTrusted, keep: true},
		{name: "a failing probe rejects", verdict: lateOnVerdictRejected},
		{name: "a failing probe after a fault rejects", prior: fault, verdict: lateOnVerdictRejected},
		{
			name:    "a passing probe keeps a peer's rejection",
			agree:   true,
			prior:   lateOnProviderRecord{verdict: lateOnVerdictRejected},
			verdict: lateOnVerdictRejected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			verdict, keep, reason := lateOnProbeVerdict(test.agree, "token 3 differs", test.prior)
			if verdict != test.verdict {
				t.Fatalf("verdict = %q, want %q", verdict, test.verdict)
			}
			if keep != test.keep {
				t.Fatalf("keep = %v, want %v", keep, test.keep)
			}
			if !test.agree && reason != "token 3 differs" {
				t.Fatalf("reason = %q, want the difference", reason)
			}
		})
	}
}

// TestProviderProbeStoresTheRuleVerdict runs the real provider probe with one
// fault on the record. The probe agrees on a host where Core ML gives correct
// output. It disagrees on a host where Core ML does not (macOS 15 ARM64, ONNX
// Runtime issue 32569). No peer process writes the record here, so selection
// keeps Core ML if and only if the probe agreed. The test gets the probe
// result from the selected provider. It requires the record that
// lateOnProbeVerdict must give for that result: the kept fault on the first
// kind of host, a rejection on the second.
// On macOS 26 and later, the test also requires Core ML.
func TestProviderProbeStoresTheRuleVerdict(t *testing.T) {
	// The probe runs only with the automatic choice, so no pin may come from
	// the environment.
	t.Setenv(lateOnProviderEnv, "")
	rawModel, _, path := newModelWithVerdictPath(t)
	prior := lateOnProviderRecord{faults: 1, at: time.Now().Unix()}
	written := fmt.Sprintf("%s %d %d\n", lateOnVerdictRecheck, prior.faults, prior.at)
	if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	lateOn, ok := rawModel.(*lateOnModel)
	if !ok {
		t.Fatalf("model type is %T, want *lateOnModel", rawModel)
	}
	if _, err := lateOn.semanticRowEncoder(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	provider := semanticProviderName()
	// The probe writes its record through a rename, so a probe that finished
	// leaves a new file. With no new file, the provider tells nothing about
	// the probe: the Core ML session failed, or the probe stopped early.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatalf("no probe record was written; the selected provider is %q", provider)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(stored))
	// The records are fixed values, not a call to lateOnProbeVerdict, so a
	// fault in that rule also fails this test.
	want := lateOnVerdictRejected
	if provider == lateOnAcceleratedProviderName {
		// Agreement after a fault keeps the count and its time.
		want = strings.TrimSpace(written)
	}
	t.Logf("the probe selected %q and stored %q", provider, got)
	if got != want {
		t.Fatalf("stored record = %q, want %q", got, want)
	}
	// ONNX Runtime issue 32569 is a macOS 15 fault. On macOS 26 and later,
	// Core ML gives correct output. A probe that rejects it there is a defect:
	// every search would then use the CPU provider.
	if major := macOSMajorVersion(t); major >= 26 && provider != lateOnAcceleratedProviderName {
		t.Fatalf("the probe rejected Core ML on macOS %d and stored %q", major, got)
	}
}

// macOSMajorVersion returns the major macOS version of the host, from the
// same sysctl that the Core ML cache key reads.
func macOSMajorVersion(t *testing.T) int {
	t.Helper()
	product, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		t.Fatal(err)
	}
	major, err := strconv.Atoi(strings.SplitN(product, ".", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	return major
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

// newModelWithVerdictPath builds the model under a fresh cache, then returns
// the decoded model bytes and the path its provider verdict will live at.
// Several tests need exactly this preamble, and it has an ordering trap: the
// cache key carries the ONNX Runtime version, which is empty until the model
// initializes the run time, so resolving the directory first yields a different
// key.
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

// TestOverlayScanFaultIsRecorded searches a repository with no commit, whose
// vectors are an overlay only, with a zero query token: the overlay scan
// fails on degenerate output and records one provider fault.
func TestOverlayScanFaultIsRecorded(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	repo := t.TempDir()
	initEmptyGitRepoNoRemoteAt(t, repo)
	writeFileAt(t, repo, "strict.go", "package sample\n// alpha beta\n")
	path := pinAcceleratedProviderForTest(t)
	search := func(scorer *hybridTestScorer) {
		t.Helper()
		output, err := seekWith(t, modelFactory(scorer), "alpha beta", repo)
		if err != nil || !strings.Contains(output, "strict.go") {
			t.Fatalf("output=%q err=%v, want the strict file", output, err)
		}
	}
	search(&hybridTestScorer{})
	search(&hybridTestScorer{queryVector: &semanticVector{}})
	if record := readLateOnProviderRecord(path); record.faults != 1 {
		t.Fatalf("provider record=%+v, want one fault from the overlay scan", record)
	}
}

// TestOneCommandRecordsOneProviderFault runs one model search whose vector
// build and ranking both give degenerate output: the command records one
// provider fault, not one per failure.
func TestOneCommandRecordsOneProviderFault(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	path := pinAcceleratedProviderForTest(t)
	degenerate := fmt.Errorf("both: %w", errDegenerateSemanticVector)
	scorer := &hybridTestScorer{embedErr: degenerate, scoreErr: degenerate}
	output, err := seekWith(t, modelFactory(scorer), "alpha beta", folder)
	if err != nil || !strings.Contains(output, "strict.go") {
		t.Fatalf("output=%q err=%v, want the strict file", output, err)
	}
	if scorer.embedCalls.Load() == 0 || scorer.scoreCalls.Load() == 0 {
		t.Fatalf("embed calls=%d score calls=%d: both must fail", scorer.embedCalls.Load(), scorer.scoreCalls.Load())
	}
	if record := readLateOnProviderRecord(path); record.faults != 1 {
		t.Fatalf("provider record=%+v, want one fault for the command", record)
	}
}
