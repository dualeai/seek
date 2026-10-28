package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// semanticBuildTestEmbedder is a build model whose embed call runs the
// function of the test; its query and score calls fail. The test controls
// when each batch starts, waits and ends, and what it returns. So a test with
// it proves how the build schedules, admits, cancels and releases batches, and
// which units it sends. It cannot prove the time or memory of a real model
// call, or anything about the vectors or the search.
type semanticBuildTestEmbedder struct {
	embed func(context.Context, []semanticUnit) ([]semanticUnitEmbedding, error)
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

// collectTestRows runs the collector over model batches and copied rows and
// returns its output and the coarse blocks that it sent, by sequence.
func collectTestRows(
	t *testing.T,
	dir string,
	batches []semanticEncodedBatch,
	copies [][]semanticCopiedRow,
) (semanticBuildOutput, map[int]int, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	encoded := make(chan semanticEncodedBatch, len(batches))
	for _, batch := range batches {
		encoded <- batch
	}
	close(encoded)
	copied := make(chan []semanticCopiedRow, len(copies))
	for _, rows := range copies {
		copied <- rows
	}
	close(copied)
	coarseReady := make(chan semanticCoarseBatch, 64)
	done := make(chan semanticBuildOutput, 1)
	collectSemanticBatches(encoded, copied, dir, coarseReady, cancel, done)
	blocks := map[int]int{}
	for batch := range coarseReady {
		blocks[batch.sequence] = len(batch.coarse)
	}
	output := <-done
	return output, blocks, ctx.Err()
}

// testEmbeddedBatch returns a model batch with one unit for each of rows, each
// embedded as vector.
func testEmbeddedBatch(rows []uint64, vector semanticVector) semanticEncodedBatch {
	batch := semanticEncodedBatch{}
	for _, row := range rows {
		batch.units = append(batch.units, semanticUnit{row: row, text: []byte("x"), modelInput: []int{1}})
		batch.embeddings = append(batch.embeddings, testSemanticUnitEmbedding(vector))
	}
	return batch
}

// TestCollectSemanticBatchesWritesRowsAtOffsets mixes model rows, out of
// order, with rows copied from a live generation. Every row lands at its
// offset in both vector files. Each 128-row block goes to the USearch stream
// once.
func TestCollectSemanticBatchesWritesRowsAtOffsets(t *testing.T) {
	source := &semanticGeneration{
		vectors: testSemanticFineVectors([]semanticUnitEmbedding{
			testSemanticUnitEmbedding(semanticVector{2: 1}), testSemanticUnitEmbedding(semanticVector{3: 1}),
		}),
		coarse: make([]semanticCoarseSNORM16Vectors, 2),
	}
	for row := range source.coarse {
		for centroid := range source.coarse[row] {
			source.coarse[row][centroid][2+row] = semanticSNORM16Scale
		}
	}
	first := make([]uint64, 0, 128)
	for row := range uint64(128) {
		first = append(first, row)
	}
	dir := t.TempDir()
	output, blocks, ctxErr := collectTestRows(t, dir,
		[]semanticEncodedBatch{
			testEmbeddedBatch([]uint64{130}, semanticVector{1: 1}),
			testEmbeddedBatch(first, semanticVector{0: 1}),
		},
		[][]semanticCopiedRow{{{row: 128, ref: semanticReuseRef{generation: source, row: 0}}, {row: 129, ref: semanticReuseRef{generation: source, row: 1}}}},
	)
	if output.err != nil || ctxErr != nil {
		t.Fatalf("collector err=%v ctx=%v", output.err, ctxErr)
	}
	if output.rows != 131 || output.vectorsArtifact.Bytes != 131*semanticFineVectorBytesPerUnit ||
		output.coarseArtifact.Bytes != 131*semanticCoarseVectorBytesPerUnit {
		t.Fatalf("output=%+v", output)
	}
	if blocks[0] != 128 || blocks[1] != 3 || len(blocks) != 2 {
		t.Fatalf("coarse blocks=%v, want 128 and 3", blocks)
	}
	fine, err := os.ReadFile(filepath.Join(dir, semanticVectorsFile))
	if err != nil {
		t.Fatal(err)
	}
	for row, dimension := range map[int]int{0: 0, 127: 0, 128: 2, 129: 3, 130: 1} {
		at := row*semanticFineVectorBytesPerUnit + dimension*2
		if code := binary.LittleEndian.Uint16(fine[at:]); code != semanticF16One {
			t.Errorf("row %d dimension %d holds %d", row, dimension, code)
		}
	}
	coarse, err := os.ReadFile(filepath.Join(dir, semanticCoarseFile))
	if err != nil {
		t.Fatal(err)
	}
	if at := 129*semanticCoarseVectorBytesPerUnit + 3*2; int16(binary.LittleEndian.Uint16(coarse[at:])) != semanticSNORM16Scale {
		t.Error("the copied coarse row is not at its offset")
	}
}

// TestCollectSemanticBatchesRejectsInvalidInput checks that each of these
// inputs fails the build and cancels it:
//   - a row written twice, or a missing row;
//   - a batch with mismatched embeddings;
//   - a NaN vector.
func TestCollectSemanticBatchesRejectsInvalidInput(t *testing.T) {
	mismatched := testEmbeddedBatch([]uint64{0, 1}, semanticVector{0: 1})
	mismatched.embeddings = mismatched.embeddings[:1]
	invalid := testEmbeddedBatch([]uint64{0}, semanticVector{0: float32(math.NaN())})
	for name, batches := range map[string][]semanticEncodedBatch{
		"twice":      {testEmbeddedBatch([]uint64{0}, semanticVector{0: 1}), testEmbeddedBatch([]uint64{0}, semanticVector{0: 1})},
		"missing":    {testEmbeddedBatch([]uint64{0, 2}, semanticVector{0: 1})},
		"mismatched": {mismatched},
		"invalid":    {invalid},
	} {
		t.Run(name, func(t *testing.T) {
			output, _, ctxErr := collectTestRows(t, t.TempDir(), batches, nil)
			if output.err == nil || !errors.Is(ctxErr, context.Canceled) {
				t.Fatalf("err=%v ctx=%v, want a failed, canceled build", output.err, ctxErr)
			}
		})
	}
}

// TestCollectSemanticBatchesAcceptsNoRows collects no row: the build succeeds
// with two empty vector files and sends no block.
func TestCollectSemanticBatchesAcceptsNoRows(t *testing.T) {
	dir := t.TempDir()
	output, blocks, ctxErr := collectTestRows(t, dir, nil, nil)
	if output.err != nil || ctxErr != nil || output.rows != 0 || len(blocks) != 0 ||
		output.vectorsArtifact.Bytes != 0 || output.coarseArtifact.Bytes != 0 {
		t.Fatalf("empty build output=%+v blocks=%v ctx=%v", output, blocks, ctxErr)
	}
	for _, name := range []string{semanticVectorsFile, semanticCoarseFile} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Size() != 0 {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestCollectSemanticBatchesCancelsAfterVectorCreationFailure finds the vector
// file already there: the build fails with os.ErrExist and cancels.
func TestCollectSemanticBatchesCancelsAfterVectorCreationFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, semanticVectorsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	output, _, ctxErr := collectTestRows(t, dir, nil, nil)
	if !errors.Is(output.err, os.ErrExist) || !errors.Is(ctxErr, context.Canceled) {
		t.Fatalf("err=%v ctx=%v, want an existing-file error and cancellation", output.err, ctxErr)
	}
}

// TestEncodeSemanticBatchesAdmitsReadyWorkAfterOneCallCompletes runs 4 model
// calls with a live limit of 2. The second call is slow: it ends only after
// the fourth call starts. The third call waits until the second starts. So
// the scheduler must start the fourth call when the third ends, beside the
// slow call. The test runs in a synctest bubble with no timer. When every
// goroutine blocks, a scheduler that waits for the slow call has not started
// the fourth call, so the check fails at once.
func TestEncodeSemanticBatchesAdmitsReadyWorkAfterOneCallCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		batches := make(chan semanticBuildBatch, 4)
		for sequence := range 4 {
			batches <- semanticBuildBatch{
				units: []semanticUnit{{row: uint64(sequence)}},
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

		// Wait until each goroutine of the bubble blocks. The slow call then
		// waits for its release, and a correct scheduler has started the
		// fourth call beside it.
		synctest.Wait()
		select {
		case <-fourthStarted:
		default:
			t.Error("the scheduler did not start the fourth call beside the slow call")
		}
		close(slowRelease)
		<-done
		for range encoded {
		}
		if got := peak.Load(); got != 2 {
			t.Fatalf("peak model calls=%d, want the live two-call limit", got)
		}
	})
}

// stepSemanticModelCalls runs in a synctest bubble, with a stub model whose
// calls wait on step. It waits until each goroutine of the bubble blocks. The
// scheduler has then started each call that its live limit admits, and waits
// for one to end. It then ends one call, and does this again until the
// scheduler returns. So the calls that run at each step are the scheduler's
// limit, with no timer and whatever the load of the host. If the scheduler
// waits with no running call, the send blocks too, and synctest stops the
// test as a deadlock.
func stepSemanticModelCalls(step chan<- struct{}, done <-chan struct{}) {
	for {
		synctest.Wait()
		select {
		case <-done:
			return
		default:
		}
		step <- struct{}{}
	}
}

// TestEncodeSemanticBatchesTracksLiveCPULimit gives the scheduler 4 CPUs, then
// 1 when 4 model calls run, then 4 again after 3 calls on 1 CPU. The
// scheduler must reach 4 calls, start each call on 1 CPU alone, and grow
// again after the recovery. Each model call waits for its step, so the calls
// that the scheduler admits overlap on any host.
func TestEncodeSemanticBatchesTracksLiveCPULimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const batchCount = 48
		batches := make(chan semanticBuildBatch, batchCount)
		for sequence := range batchCount {
			batches <- semanticBuildBatch{
				units: []semanticUnit{{row: uint64(sequence)}},
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
		step := make(chan struct{})
		embedder := semanticBuildTestEmbedder{embed: func(
			callCtx context.Context,
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
			select {
			case <-step:
			case <-callCtx.Done():
			}
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

		stepSemanticModelCalls(step, done)
		for range encoded {
		}
		// A peak under 4 calls also keeps the phase at 0. Check the peak
		// first, so that the failure gives the peak.
		if initialPeak.Load() < 4 {
			t.Fatalf("initial peak=%d, want four model calls", initialPeak.Load())
		}
		if phase.Load() != 2 {
			t.Fatalf("scheduler phase=%d, want CPU recovery", phase.Load())
		}
		if constrainedPeak.Load() != 1 {
			t.Fatalf("constrained peak=%d, want one model call", constrainedPeak.Load())
		}
		if recoveredPeak.Load() < 2 {
			t.Fatalf("recovered peak=%d, want renewed parallel calls", recoveredPeak.Load())
		}
	})
}

// TestEncodeSemanticBatchesTracksLiveMemoryHeadroom drops the free memory
// when the scheduler probes 2 model calls, and raises it on row 8. The
// scheduler must probe 2 calls, run rows 6 to 8 one at a time, and grow again
// after the rise. Each model call waits for its step, so the calls that the
// scheduler admits overlap on any host.
func TestEncodeSemanticBatchesTracksLiveMemoryHeadroom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const batchCount = 30
		batches := make(chan semanticBuildBatch, batchCount)
		for sequence := range batchCount {
			batches <- semanticBuildBatch{
				units: []semanticUnit{{row: uint64(sequence)}},
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
		step := make(chan struct{})
		embedder := semanticBuildTestEmbedder{embed: func(
			callCtx context.Context,
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
			select {
			case <-step:
			case <-callCtx.Done():
			}
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
				effectiveCPUs:   func() int { return 4 },
				availableMemory: available.Load,
				callCPUs:        1,
			})
			close(done)
		}()

		stepSemanticModelCalls(step, done)
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
	})
}

// TestEncodeSemanticBatchesCancellationReleasesAllWeights fails every model
// call of 4 batches that share one read-weight owner. The scheduler reports
// the failure and cancels, and the deferred check finds all the read weight
// released.
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
	owner := newSemanticBuildOwner(totalWeight)
	for sequence := range batchCount {
		owner.retain()
		batches <- semanticBuildBatch{
			units:  []semanticUnit{{row: uint64(sequence)}},
			owners: []*semanticBuildOwner{owner},
		}
	}
	owner.done()
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

// TestBuildSemanticGenerationReleasesSourceWeightOnModelFailure fails the
// model call after the build owns the read weight of one document. The build
// returns the model error, and the deferred check finds the weight released.
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
	err := buildSemanticGeneration(
		t.Context(),
		filepath.Join(t.TempDir(), "semantic"),
		"source-1",
		embedder,
		searchResources{},
		func(context.Context) (<-chan fileContent, func() error) {
			return documents, func() error { return nil }
		},
		&semanticReuseIndex{},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("semantic build error=%v, want %v", err, wantErr)
	}
}

// TestBuildSemanticGenerationKeepsCollectorErrorAfterInternalCancellation
// puts a vector file in the build directory first. The collector fails with
// os.ErrExist and cancels the build. The build returns that error, not the
// cancellation, and publishes no manifest.
func TestBuildSemanticGenerationKeepsCollectorErrorAfterInternalCancellation(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, semanticVectorsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	embedder := semanticBuildTestEmbedder{embed: func(
		context.Context,
		[]semanticUnit,
	) ([]semanticUnitEmbedding, error) {
		return nil, errors.New("unexpected model call")
	}}
	err := buildSemanticGeneration(
		t.Context(),
		dir,
		"source-collector-error",
		embedder,
		searchResources{},
		func(ctx context.Context) (<-chan fileContent, func() error) {
			documents := make(chan fileContent)
			go func() {
				<-ctx.Done()
				close(documents)
			}()
			return documents, ctx.Err
		},
		&semanticReuseIndex{},
	)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("semantic build error=%v, want existing vector file error", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("collector error was replaced by internal cancellation: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, semanticManifestFile)); !os.IsNotExist(statErr) {
		t.Fatalf("failed build published a manifest: %v", statErr)
	}
}

// TestBuildSemanticGenerationHashesRawContent builds a generation from one
// file whose byte 0xff is not valid UTF-8. Each stored row must hold the
// SHA-256 of the raw bytes, which the search computes from the shard copy
// (checkedSemanticCandidate). The digest comes from "shasum -a 256", as in
// TestSemanticContentIDUsesRawBytes.
func TestBuildSemanticGenerationHashesRawContent(t *testing.T) {
	requireTools(t)
	want, err := hex.DecodeString("01ce0241d2a0e71a4fecd5a8d71157fe2787197732fc15d889cbcf36c38e3c68")
	if err != nil {
		t.Fatal(err)
	}
	documents := make(chan fileContent, 1)
	documents <- fileContent{name: "raw.txt", content: []byte{'a', 0xff, 'b'}}
	close(documents)
	embedder := semanticBuildTestEmbedder{embed: func(_ context.Context, units []semanticUnit) ([]semanticUnitEmbedding, error) {
		embeddings := make([]semanticUnitEmbedding, len(units))
		for index := range embeddings {
			embeddings[index] = testSemanticUnitEmbedding(semanticVector{0: 1})
		}
		return embeddings, nil
	}}
	dir := filepath.Join(t.TempDir(), "semantic")
	if err := buildSemanticGeneration(t.Context(), dir, "source-raw", embedder, searchResources{},
		func(context.Context) (<-chan fileContent, func() error) {
			return documents, func() error { return nil }
		}, &semanticReuseIndex{}); err != nil {
		t.Fatal(err)
	}
	manifest, err := readSemanticManifest(dir, "source-raw")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(dir, semanticRowsFile))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := readSemanticRowsFrom(file, manifest.Rows, false)
	_ = file.Close()
	if err != nil || len(rows) == 0 {
		t.Fatalf("rows=%d err=%v, want the rows of raw.txt", len(rows), err)
	}
	for _, row := range rows {
		if !bytes.Equal(row.contentID[:], want) {
			t.Fatalf("row %d content ID %x, want the SHA-256 of the raw bytes %x", row.row, row.contentID, want)
		}
	}
}

// TestSemanticConcurrencyControllerProbesSafeWidthsAndRejectsSlowerWidth
// doubles the width from 1 to 8 while each width is faster and the free
// memory allows it. Width 8 is slower than width 4, so the controller goes
// back to 4.
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

// TestSemanticConcurrencyControllerIgnoresSourceLimitedWindow checks that a
// window starts only when the width is full. A window with a source gap
// (noteStarved) ends with no measure and does not change the width.
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

	controller.noteActive(4, started.Add(20*time.Second))
	for range 8 {
		controller.noteCompletion(128)
	}
	controller.evaluate(started.Add(21*time.Second), 8, 10_000)
	if got := controller.activeLimit(8); got != 8 {
		t.Fatalf("later saturated window selected %d calls, want 8", got)
	}
}

// TestSemanticConcurrencyControllerDoesNotLearnMemoryFromSourceGap measures a
// memory drop over a window with a source gap. The controller learns no call
// cost from that drop, so the width grows.
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
	// The memory drop of 900 spans the source gap. Charged to the one added
	// call, the drop is more than the 100 still free and would cut the width
	// to 1. With the known cost of 7 per call, the width grows from 2 to 4.
	controller.evaluate(started.Add(11*time.Second), 8, 100)
	if got := controller.activeLimit(8); got != 4 {
		t.Fatalf("source gap changed the width to %d, want 4", got)
	}
}

// TestSemanticConcurrencyControllerReopensCeilingAfterSourceEpoch starts a new
// epoch with a source gap. The controller drops the old ceiling and best rate,
// and measures the widths again.
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

	// The old curve stopped at width 8 with 1,024 rows/s. The new epoch
	// measures 512 rows/s at width 8, then 768 at width 16. Each width beats
	// the best rate of its own epoch, so the width stays 16. The old ceiling
	// would hold the width at 8; the old best rate would cut 16 back to 8.
	controller.noteActive(8, started)
	for range 16 {
		controller.noteCompletion(32)
	}
	controller.evaluate(started.Add(time.Second), 16, 1_000)
	if got := controller.activeLimit(16); got != 16 {
		t.Fatalf("new source epoch selected %d calls, want a fresh 16-call probe", got)
	}
	controller.noteActive(16, started.Add(time.Second))
	for range 32 {
		controller.noteCompletion(24)
	}
	controller.evaluate(started.Add(2*time.Second), 16, 1_000)
	if got := controller.activeLimit(16); got != 16 {
		t.Fatalf("the old best rate cut the new epoch to %d calls, want 16", got)
	}
}

// TestSemanticConcurrencyControllerDoesNotChargeStableOutputGrowthToCalls
// lowers the free memory while the width stays at 8. The controller charges
// memory only to added calls, so the drop does not cut the width.
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

// TestSemanticConcurrencyControllerUsesMeasuredMemoryAndLiveCPU keeps the
// width at 1 while the measured call cost does not fit the free memory. It
// grows the width to 8 when the memory recovers, and cuts it to the live CPU
// limit.
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

// TestSemanticConcurrencyControllerRecalibratesAfterCPULimitChange changes the
// CPU limit from 8 to 16. The controller drops the old window, ceiling and
// best rate, and measures the widths again.
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
	// The old window needed four completions at width 2.
	for range 4 {
		if controller.noteCompletion(128) {
			t.Fatal("changed CPU limit kept the old measurement window")
		}
	}

	// The old curve stopped at width 2 with 512 rows/s. The new curve
	// measures 256 rows/s at width 2, then 384 at width 4. Each width beats
	// the best rate of the new curve, so the width grows to 8. The old
	// ceiling would hold the width at 2; the old best rate would cut 4 back
	// to 2.
	controller.noteActive(2, started)
	for range 4 {
		controller.noteCompletion(64)
	}
	controller.evaluate(started.Add(time.Second), 16, 1_000)
	if got := controller.activeLimit(16); got != 4 {
		t.Fatalf("fresh service curve selected width %d, want 4", got)
	}
	controller.noteActive(4, started.Add(time.Second))
	for range 8 {
		controller.noteCompletion(48)
	}
	controller.evaluate(started.Add(2*time.Second), 16, 1_000)
	if got := controller.activeLimit(16); got != 8 {
		t.Fatalf("the old best rate held the width at %d, want 8", got)
	}
}

// TestBuildSemanticFolderGenerationFailsOnTransientRead makes one open fail
// with EMFILE: the vector build returns the error and publishes no manifest,
// so the next search reads the file again.
func TestBuildSemanticFolderGenerationFailsOnTransientRead(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a.go", "package sample\n// alpha\n")
	writeFileAt(t, folder, "b.go", "package sample\n// beta\n")
	var selected []folderCandidate
	for _, name := range []string{"a.go", "b.go"} {
		path := filepath.Join(folder, name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		selected = append(selected, newFolderCandidate(name, path, info))
	}
	realOpen := folderOpenFile
	folderOpenFile = func(path string) (*os.File, error) {
		if filepath.Base(path) == "b.go" {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EMFILE}
		}
		return realOpen(path)
	}
	t.Cleanup(func() { folderOpenFile = realOpen })
	embedder := semanticBuildTestEmbedder{embed: func(_ context.Context, units []semanticUnit) ([]semanticUnitEmbedding, error) {
		embeddings := make([]semanticUnitEmbedding, len(units))
		for index := range embeddings {
			embeddings[index] = testSemanticUnitEmbedding(semanticVector{0: 1})
		}
		return embeddings, nil
	}}
	dir := filepath.Join(t.TempDir(), "semantic")
	err := buildSemanticFolderGeneration(t.Context(), selected, t.TempDir(), "", "", dir, "source", embedder, liveSearchResources())
	if !errors.Is(err, syscall.EMFILE) {
		t.Fatalf("err=%v, want EMFILE", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, semanticManifestFile)); !os.IsNotExist(statErr) {
		t.Fatalf("the failed build wrote a manifest: %v", statErr)
	}
}
