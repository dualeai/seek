package main

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt/index"
)

func TestSemanticDocumentLanguageMatchesZoektDetection(t *testing.T) {
	options := indexBuildOptions("", 1)
	options.SetDefaults()
	checker := &index.DocChecker{}
	tests := []struct {
		name     string
		document fileContent
		want     string
	}{
		{
			name: "Go source",
			document: fileContent{
				name:    "cmd/seek/main.go",
				content: []byte("package main\nfunc main() {}\n"),
			},
			want: "Go",
		},
		{
			name: "skipped Python source",
			document: fileContent{
				name:       "tool.py",
				content:    []byte("print('ignored')\n"),
				skipReason: index.SkipReasonTooLarge,
			},
			want: "Python",
		},
		{
			name: "unknown extension",
			document: fileContent{
				name:    "NOTICE.unknown-seek-test",
				content: []byte("plain words with no source markers\n"),
			},
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := semanticDocumentLanguage(checker, options, test.document)
			if got != test.want {
				t.Fatalf("language=%q, want %q", got, test.want)
			}
		})
	}
}

type semanticBuildTestEmbedder struct {
	embed func(context.Context, []semanticUnit) ([]semanticUnitEmbedding, error)
}

func (semanticBuildTestEmbedder) PackSemanticUnits(
	_ context.Context,
	units []semanticUnit,
) ([]semanticUnit, error) {
	return units, nil
}

func (embedder semanticBuildTestEmbedder) EmbedSemanticUnits(
	ctx context.Context,
	units []semanticUnit,
) ([]semanticUnitEmbedding, error) {
	return embedder.embed(ctx, units)
}

func (semanticBuildTestEmbedder) PrepareSemanticQuery(
	context.Context,
	string,
) (*semanticQueryEmbedding, error) {
	return nil, errors.New("unexpected query")
}

func (semanticBuildTestEmbedder) ScoresWithSemanticQuery(
	context.Context,
	*semanticQueryEmbedding,
	[]rerankDocument,
) ([]float32, error) {
	return nil, errors.New("unexpected score")
}

func (semanticBuildTestEmbedder) SemanticCallCPUs(context.Context) int { return 1 }
func (semanticBuildTestEmbedder) Close() error                         { return nil }

func TestCollectSemanticBatchesRestoresSequence(t *testing.T) {
	firstUnits := make([]semanticUnit, semanticModelBatchRows)
	firstEmbeddings := make([]semanticUnitEmbedding, semanticModelBatchRows)
	firstVector := semanticVector{0: 1}
	for row := range firstUnits {
		firstUnits[row] = semanticUnit{
			row:        uint64(row),
			text:       []byte("first"),
			modelInput: []int{1},
		}
		firstEmbeddings[row] = testSemanticUnitEmbedding(firstVector)
	}
	secondVector := semanticVector{1: 1}
	encoded := make(chan semanticEncodedBatch, 2)
	encoded <- semanticEncodedBatch{
		sequence: 1,
		units: []semanticUnit{{
			row:        semanticModelBatchRows,
			text:       []byte("second"),
			modelInput: []int{2},
		}},
		embeddings: []semanticUnitEmbedding{testSemanticUnitEmbedding(secondVector)},
	}
	encoded <- semanticEncodedBatch{
		sequence:   0,
		units:      firstUnits,
		embeddings: firstEmbeddings,
	}
	close(encoded)
	done := make(chan semanticBuildOutput, 1)
	coarseReady := make(chan semanticCoarseBatch, 2)
	vectorsPath := filepath.Join(t.TempDir(), semanticVectorsFile)
	collectSemanticBatches(encoded, vectorsPath, coarseReady, done)
	output := <-done
	if output.err != nil {
		t.Fatal(output.err)
	}
	if got, want := []uint64{output.units[0].row, output.units[semanticModelBatchRows].row}, []uint64{0, semanticModelBatchRows}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v, want %v", got, want)
	}
	if output.units[0].text != nil || output.units[semanticModelBatchRows].text != nil {
		t.Fatal("collected rows kept source text")
	}
	if output.units[0].modelInput != nil || output.units[semanticModelBatchRows].modelInput != nil {
		t.Fatal("collected rows kept transient model input")
	}
	coarseBySequence := make(map[int][]semanticCoarseVectors)
	for batch := range coarseReady {
		coarseBySequence[batch.sequence] = batch.coarse
	}
	first := coarseBySequence[0]
	second := coarseBySequence[1]
	if len(first) != semanticModelBatchRows || len(second) != 1 ||
		first[0][0][0] != 1 || second[0][0][1] != 1 {
		t.Fatal("vectors are out of order")
	}
	wantBytes := uint64((semanticModelBatchRows + 1) * semanticFineVectorBytesPerUnit)
	if output.vectorsArtifact.Bytes != wantBytes {
		t.Fatalf("vector bytes=%d, want %d", output.vectorsArtifact.Bytes, wantBytes)
	}
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	secondAt := semanticModelBatchRows * semanticFineVectorBytesPerUnit
	if math.Float32frombits(binary.LittleEndian.Uint32(raw[:4])) != 1 ||
		math.Float32frombits(binary.LittleEndian.Uint32(raw[secondAt+4:secondAt+8])) != 1 {
		t.Fatal("fine vectors are out of order")
	}
}

func TestEncodeSemanticBatchesAdmitsReadyWorkAfterOneCallCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	batches := make(chan semanticBuildBatch, 4)
	for sequence := range 4 {
		batches <- semanticBuildBatch{
			sequence: sequence,
			units:    []semanticUnit{{row: uint64(sequence)}},
		}
	}
	close(batches)

	slowStarted := make(chan struct{})
	slowRelease := make(chan struct{})
	fourthStarted := make(chan struct{})
	var fourthOnce sync.Once
	var active atomic.Int32
	var peak atomic.Int32
	embedder := semanticBuildTestEmbedder{embed: func(
		_ context.Context,
		units []semanticUnit,
	) ([]semanticUnitEmbedding, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for observed := peak.Load(); current > observed; observed = peak.Load() {
			if peak.CompareAndSwap(observed, current) {
				break
			}
		}
		switch units[0].row {
		case 1:
			close(slowStarted)
			<-slowRelease
		case 2:
			<-slowStarted
		case 3:
			fourthOnce.Do(func() { close(fourthStarted) })
		}
		return make([]semanticUnitEmbedding, len(units)), nil
	}}
	clock := time.Unix(1, 0)
	resources := searchResources{
		now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
		effectiveCPUs:   func() int { return 2 },
		availableMemory: func() int64 { return 1 << 30 },
		callCPUs:        1,
	}
	encoded := make(chan semanticEncodedBatch, 4)
	done := make(chan struct{})
	go func() {
		encodeSemanticBatches(ctx, embedder, batches, encoded, cancel, resources)
		close(done)
	}()

	select {
	case <-fourthStarted:
	case <-time.After(5 * time.Second):
		close(slowRelease)
		t.Fatal("ready work waited for the slow model call")
	}
	close(slowRelease)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("semantic workers did not stop")
	}
	for range encoded {
	}
	if got := peak.Load(); got != 2 {
		t.Fatalf("peak model calls=%d, want the live two-call limit", got)
	}
}

func TestEncodeSemanticBatchesTracksLiveCPULimit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const batchCount = 48
	batches := make(chan semanticBuildBatch, batchCount)
	for sequence := range batchCount {
		batches <- semanticBuildBatch{
			sequence: sequence,
			units:    []semanticUnit{{row: uint64(sequence)}},
		}
	}
	close(batches)

	var effectiveCPUs atomic.Int32
	effectiveCPUs.Store(4)
	var phase atomic.Int32
	var active atomic.Int32
	var initialPeak atomic.Int32
	var constrainedPeak atomic.Int32
	var recoveredPeak atomic.Int32
	var constrainedStarts atomic.Int32
	var shrinkOnce sync.Once
	var recoverOnce sync.Once
	updatePeak := func(target *atomic.Int32, value int32) {
		for observed := target.Load(); value > observed; observed = target.Load() {
			if target.CompareAndSwap(observed, value) {
				return
			}
		}
	}
	embedder := semanticBuildTestEmbedder{embed: func(
		_ context.Context,
		units []semanticUnit,
	) ([]semanticUnitEmbedding, error) {
		currentPhase := phase.Load()
		current := active.Add(1)
		defer active.Add(-1)
		switch currentPhase {
		case 0:
			updatePeak(&initialPeak, current)
			if current >= 4 {
				shrinkOnce.Do(func() {
					effectiveCPUs.Store(1)
					phase.Store(1)
				})
			}
		case 1:
			updatePeak(&constrainedPeak, current)
			if constrainedStarts.Add(1) == 3 {
				recoverOnce.Do(func() {
					effectiveCPUs.Store(4)
					phase.Store(2)
				})
			}
		default:
			updatePeak(&recoveredPeak, current)
		}
		time.Sleep(3 * time.Millisecond)
		return make([]semanticUnitEmbedding, len(units)), nil
	}}
	var clock atomic.Int64
	encoded := make(chan semanticEncodedBatch, batchCount)
	done := make(chan struct{})
	go func() {
		encodeSemanticBatches(ctx, embedder, batches, encoded, cancel, searchResources{
			now: func() time.Time {
				return time.Unix(clock.Add(1), 0)
			},
			effectiveCPUs: func() int { return int(effectiveCPUs.Load()) },
			availableMemory: func() int64 {
				return 1 << 30
			},
			callCPUs: 1,
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("semantic scheduler did not finish")
	}
	for range encoded {
	}
	if phase.Load() != 2 {
		t.Fatalf("scheduler phase=%d, want CPU recovery", phase.Load())
	}
	if initialPeak.Load() < 4 {
		t.Fatalf("initial peak=%d, want four model calls", initialPeak.Load())
	}
	if constrainedPeak.Load() != 1 {
		t.Fatalf("constrained peak=%d, want one model call", constrainedPeak.Load())
	}
	if recoveredPeak.Load() < 2 {
		t.Fatalf("recovered peak=%d, want renewed parallel calls", recoveredPeak.Load())
	}
}

func TestEncodeSemanticBatchesTracksLiveMemoryHeadroom(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const batchCount = 30
	batches := make(chan semanticBuildBatch, batchCount)
	for sequence := range batchCount {
		batches <- semanticBuildBatch{
			sequence: sequence,
			units:    []semanticUnit{{row: uint64(sequence)}},
		}
	}
	close(batches)

	var available atomic.Int64
	available.Store(100)
	var active atomic.Int32
	var initialPeak atomic.Int32
	var constrainedPeak atomic.Int32
	var recoveredPeak atomic.Int32
	var lowOnce sync.Once
	updatePeak := func(target *atomic.Int32, value int32) {
		for observed := target.Load(); value > observed; observed = target.Load() {
			if target.CompareAndSwap(observed, value) {
				return
			}
		}
	}
	embedder := semanticBuildTestEmbedder{embed: func(
		_ context.Context,
		units []semanticUnit,
	) ([]semanticUnitEmbedding, error) {
		row := units[0].row
		current := active.Add(1)
		defer active.Add(-1)
		switch {
		case row < 6:
			updatePeak(&initialPeak, current)
			if current >= 2 {
				lowOnce.Do(func() { available.Store(30) })
			}
		case row <= 8:
			updatePeak(&constrainedPeak, current)
			if row == 8 {
				available.Store(1_000)
			}
		default:
			updatePeak(&recoveredPeak, current)
		}
		time.Sleep(3 * time.Millisecond)
		return make([]semanticUnitEmbedding, len(units)), nil
	}}
	var clock atomic.Int64
	var memoryReads atomic.Int32
	encoded := make(chan semanticEncodedBatch, batchCount)
	done := make(chan struct{})
	go func() {
		encodeSemanticBatches(ctx, embedder, batches, encoded, cancel, searchResources{
			now: func() time.Time {
				return time.Unix(clock.Add(1), 0)
			},
			effectiveCPUs: func() int { return 4 },
			availableMemory: func() int64 {
				memoryReads.Add(1)
				return available.Load()
			},
			callCPUs: 1,
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("semantic scheduler did not finish")
	}
	for range encoded {
	}
	if initialPeak.Load() < 2 {
		t.Fatalf("initial peak=%d, want a memory probe", initialPeak.Load())
	}
	if constrainedPeak.Load() != 1 {
		t.Fatalf("low-memory peak=%d, want one model call", constrainedPeak.Load())
	}
	if recoveredPeak.Load() < 2 {
		t.Fatalf("recovered peak=%d, want renewed parallel calls", recoveredPeak.Load())
	}
	if memoryReads.Load() < 3 {
		t.Fatalf("live memory reads=%d, want repeated readings", memoryReads.Load())
	}
}

func TestEncodeSemanticBatchesCancellationReleasesAllWeights(t *testing.T) {
	checkWeight := withReadSemLock(t)
	defer checkWeight()

	ctx, cancel := context.WithCancel(t.Context())
	const batchCount = 4
	batches := make(chan semanticBuildBatch, batchCount)
	var totalWeight int64
	for sequence := range batchCount {
		totalWeight += int64(sequence + 1)
	}
	if err := readSemaphore.Acquire(ctx, totalWeight); err != nil {
		t.Fatal(err)
	}
	owner := &semanticBuildOwner{weight: totalWeight}
	for sequence := range batchCount {
		owner.retain()
		batches <- semanticBuildBatch{
			sequence: sequence,
			units:    []semanticUnit{{row: uint64(sequence)}},
			owners:   []*semanticBuildOwner{owner},
		}
	}
	owner.seal()
	close(batches)

	wantErr := errors.New("model failed")
	embedder := semanticBuildTestEmbedder{embed: func(
		context.Context,
		[]semanticUnit,
	) ([]semanticUnitEmbedding, error) {
		return nil, wantErr
	}}
	encoded := make(chan semanticEncodedBatch, batchCount)
	encodeSemanticBatches(ctx, embedder, batches, encoded, cancel, searchResources{
		now:             time.Now,
		effectiveCPUs:   func() int { return 2 },
		availableMemory: func() int64 { return 1 << 30 },
		callCPUs:        1,
	})

	var sawFailure bool
	for batch := range encoded {
		sawFailure = sawFailure || errors.Is(batch.err, wantErr)
	}
	if !sawFailure {
		t.Fatal("model failure was not reported")
	}
}

func TestBuildSemanticGenerationReleasesSourceWeightOnModelFailure(t *testing.T) {
	requireTools(t)
	checkWeight := withReadSemLock(t)
	defer checkWeight()

	document := makeFileContent(
		t,
		"app.go",
		[]byte("package sample\nfunc Alpha() {}\n"),
	)
	documents := make(chan fileContent, 1)
	documents <- document
	close(documents)
	wantErr := errors.New("model failed after pipeline ownership transfer")
	embedder := semanticBuildTestEmbedder{embed: func(
		context.Context,
		[]semanticUnit,
	) ([]semanticUnitEmbedding, error) {
		return nil, wantErr
	}}
	_, err := buildSemanticGeneration(
		t.Context(),
		filepath.Join(t.TempDir(), "semantic"),
		"source-1",
		embedder,
		searchResources{},
		func(context.Context) (<-chan fileContent, func() error) {
			return documents, func() error { return nil }
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("semantic build error=%v, want %v", err, wantErr)
	}
}

func TestSemanticConcurrencyControllerProbesSafeWidthsAndRejectsSlowerWidth(t *testing.T) {
	started := time.Unix(1, 0)
	controller := newSemanticConcurrencyController(started, 5_000)
	controller.noteActive(1, started)
	if !controller.noteCompletion(128) {
		t.Fatal("one-wide calibration did not complete")
	}
	controller.evaluate(started.Add(time.Second), 8, 4_000)
	if got := controller.activeLimit(8); got != 2 {
		t.Fatalf("first probe limit=%d, want 2", got)
	}

	controller.noteActive(2, started.Add(time.Second))
	for range 3 {
		controller.noteCompletion(128)
	}
	if !controller.noteCompletion(128) {
		t.Fatal("two-wide calibration did not complete")
	}
	controller.evaluate(started.Add(3*time.Second), 8, 10_000)
	if got := controller.activeLimit(8); got != 4 {
		t.Fatalf("second probe limit=%d, want 4", got)
	}

	controller.noteActive(4, started.Add(3*time.Second))
	for range 7 {
		controller.noteCompletion(128)
	}
	if !controller.noteCompletion(128) {
		t.Fatal("four-wide calibration did not complete")
	}
	controller.evaluate(started.Add(5*time.Second), 8, 10_000)
	if got := controller.activeLimit(8); got != 8 {
		t.Fatalf("third probe limit=%d, want 8", got)
	}

	controller.noteActive(8, started.Add(5*time.Second))
	for range 15 {
		controller.noteCompletion(128)
	}
	if !controller.noteCompletion(128) {
		t.Fatal("eight-wide calibration did not complete")
	}
	controller.evaluate(started.Add(10*time.Second), 8, 9_800)
	if got := controller.activeLimit(8); got != 4 {
		t.Fatalf("slower width left limit=%d, want best width 4", got)
	}
}

func TestSemanticConcurrencyControllerIgnoresSourceLimitedWindow(t *testing.T) {
	started := time.Unix(1, 0)
	controller := newSemanticConcurrencyController(started, 10_000)
	controller.noteActive(1, started)
	controller.noteCompletion(128)
	controller.evaluate(started.Add(time.Second), 8, 10_000)

	controller.noteActive(1, started.Add(2*time.Second))
	if controller.noteCompletion(128) {
		t.Fatal("completion before target saturation ended the window")
	}
	controller.noteActive(2, started.Add(10*time.Second))
	for range 4 {
		controller.noteCompletion(128)
	}
	controller.evaluate(started.Add(11*time.Second), 8, 10_000)
	if got := controller.activeLimit(8); got != 4 {
		t.Fatalf("first measured width selected %d calls, want 4", got)
	}

	controller.noteActive(4, started.Add(11*time.Second))
	controller.noteStarved()
	for range 8 {
		if controller.noteCompletion(128) {
			t.Fatal("source-limited completion ended the invalid window")
		}
	}
	if got := controller.activeLimit(8); got != 4 {
		t.Fatalf("source-limited window selected %d calls, want 4", got)
	}
	if controller.windowSaturated {
		t.Fatal("source-limited window stayed active")
	}

	controller.noteActive(4, started.Add(20*time.Second))
	for range 8 {
		controller.noteCompletion(128)
	}
	controller.evaluate(started.Add(21*time.Second), 8, 10_000)
	if got := controller.activeLimit(8); got != 8 {
		t.Fatalf("later saturated window selected %d calls, want 8", got)
	}
}

func TestSemanticConcurrencyControllerDoesNotLearnMemoryFromSourceGap(t *testing.T) {
	started := time.Unix(1, 0)
	controller := &semanticConcurrencyController{
		limit:             2,
		previousLimit:     1,
		bestLimit:         1,
		performanceCPUs:   8,
		growthCost:        7,
		windowAvailable:   1_000,
		growthSampleValid: true,
		warmupDone:        true,
	}
	controller.noteActive(2, started)
	controller.noteStarved()
	controller.noteActive(2, started.Add(10*time.Second))
	for range 4 {
		controller.noteCompletion(128)
	}
	controller.evaluate(started.Add(11*time.Second), 8, 100)
	if controller.growthCost != 7 {
		t.Fatalf("source gap changed per-call memory cost to %d", controller.growthCost)
	}
}

func TestSemanticConcurrencyControllerReopensCeilingAfterSourceEpoch(t *testing.T) {
	started := time.Unix(1, 0)
	controller := &semanticConcurrencyController{
		limit:              8,
		previousLimit:      8,
		bestLimit:          8,
		performanceCeiling: 8,
		performanceCPUs:    16,
		bestRate:           1_024,
		growthCost:         1,
		windowAvailable:    1_000,
		growthSampleValid:  true,
		warmupDone:         true,
	}
	controller.noteStarved()
	if controller.performanceCeiling != 0 || controller.bestRate != 0 {
		t.Fatal("new source epoch kept the old service curve")
	}

	controller.noteActive(8, started)
	for range 16 {
		controller.noteCompletion(128)
	}
	controller.evaluate(started.Add(time.Second), 16, 1_000)
	if got := controller.activeLimit(16); got != 16 {
		t.Fatalf("new source epoch selected %d calls, want a fresh 16-call probe", got)
	}
}

func TestSemanticConcurrencyControllerDoesNotChargeStableOutputGrowthToCalls(t *testing.T) {
	started := time.Unix(1, 0)
	controller := newSemanticConcurrencyController(started, 1_000)
	controller.noteActive(1, started)
	controller.noteCompletion(128)
	controller.evaluate(started.Add(time.Second), 8, 900)
	if got := controller.activeLimit(8); got != 2 {
		t.Fatalf("first probe limit=%d, want 2", got)
	}

	controller.noteActive(2, started.Add(time.Second))
	for range 3 {
		controller.noteCompletion(128)
	}
	controller.noteCompletion(128)
	controller.evaluate(started.Add(3*time.Second), 8, 900)
	if got := controller.activeLimit(8); got != 4 {
		t.Fatalf("second probe limit=%d, want 4", got)
	}

	controller.noteActive(4, started.Add(3*time.Second))
	for range 7 {
		controller.noteCompletion(128)
	}
	controller.noteCompletion(128)
	controller.evaluate(started.Add(5*time.Second), 8, 900)
	if got := controller.activeLimit(8); got != 8 {
		t.Fatalf("third probe limit=%d, want 8", got)
	}

	controller.noteActive(8, started.Add(5*time.Second))
	for range 15 {
		controller.noteCompletion(128)
	}
	controller.noteCompletion(128)
	controller.evaluate(started.Add(7*time.Second), 8, 800)

	controller.noteActive(8, started.Add(7*time.Second))
	for range 15 {
		controller.noteCompletion(128)
	}
	controller.noteCompletion(128)
	controller.evaluate(started.Add(9*time.Second), 8, 400)
	if got := controller.activeLimit(8); got != 8 {
		t.Fatalf("stable output growth reduced limit to %d", got)
	}
}

func TestSemanticConcurrencyControllerUsesMeasuredMemoryAndLiveCPU(t *testing.T) {
	started := time.Unix(1, 0)
	controller := newSemanticConcurrencyController(started, 100)
	controller.noteActive(1, started)
	controller.noteCompletion(128)
	controller.evaluate(started.Add(time.Second), 8, 40)
	if got := controller.activeLimit(8); got != 1 {
		t.Fatalf("low measured headroom grew limit to %d", got)
	}

	controller.noteActive(1, started.Add(time.Second))
	controller.noteCompletion(256)
	controller.noteCompletion(256)
	controller.evaluate(started.Add(2*time.Second), 8, 600)
	if got := controller.activeLimit(8); got != 2 {
		t.Fatalf("recovered headroom left limit=%d, want 2", got)
	}

	controller.noteActive(2, started.Add(2*time.Second))
	for range 4 {
		controller.noteCompletion(256)
	}
	controller.evaluate(started.Add(3*time.Second), 8, 600)
	if got := controller.activeLimit(8); got != 4 {
		t.Fatalf("continued calibration left limit=%d, want 4", got)
	}

	controller.noteActive(4, started.Add(3*time.Second))
	for range 8 {
		controller.noteCompletion(256)
	}
	controller.evaluate(started.Add(4*time.Second), 8, 600)
	if got := controller.activeLimit(8); got != 8 {
		t.Fatalf("continued calibration left limit=%d, want 8", got)
	}
	if got := controller.activeLimit(1); got != 1 {
		t.Fatalf("lower live CPU limit left concurrency=%d, want 1", got)
	}
}

func TestSemanticConcurrencyControllerRecalibratesAfterCPULimitChange(t *testing.T) {
	started := time.Unix(1, 0)
	controller := &semanticConcurrencyController{
		limit:              2,
		previousLimit:      2,
		bestLimit:          2,
		performanceCeiling: 2,
		performanceCPUs:    8,
		bestRate:           512,
		growthCost:         1,
		windowStarted:      started,
		windowAvailable:    1_000,
		growthSampleValid:  true,
		warmupDone:         true,
	}
	controller.noteActive(2, started)
	if got := controller.activeLimit(16); got != 2 {
		t.Fatalf("changed CPU limit changed active width to %d, want 2", got)
	}
	if controller.performanceCeiling != 0 || controller.bestRate != 0 {
		t.Fatal("changed CPU limit kept the old service curve")
	}
	if controller.windowSaturated || controller.noteCompletion(128) {
		t.Fatal("changed CPU limit kept the old measurement window")
	}

	controller.noteActive(2, started)
	for range 4 {
		controller.noteCompletion(128)
	}
	controller.evaluate(started.Add(time.Second), 16, 1_000)
	if got := controller.activeLimit(16); got != 4 {
		t.Fatalf("fresh service curve selected width %d, want 4", got)
	}
}
