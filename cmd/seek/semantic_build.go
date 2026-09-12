package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	ctags "github.com/sourcegraph/go-ctags"
)

type semanticSourceDocument struct {
	sequence int
	content  fileContent
}

type semanticExtractedFile struct {
	sequence int
	units    []semanticUnit
	weight   int64
	err      error
}

type semanticBuildBatch struct {
	sequence int
	units    []semanticUnit
	owners   []*semanticBuildOwner
}

// semanticBuildOwner keeps one source file admitted until every model batch
// that reads a view of its bytes has finished tokenization and inference.
type semanticBuildOwner struct {
	weight    int64
	remaining atomic.Int64
	sealed    atomic.Bool
	released  atomic.Bool
}

func (owner *semanticBuildOwner) retain() {
	owner.remaining.Add(1)
}

func (owner *semanticBuildOwner) done() {
	owner.remaining.Add(-1)
	owner.releaseIfDone()
}

func (owner *semanticBuildOwner) seal() {
	owner.sealed.Store(true)
	owner.releaseIfDone()
}

func (owner *semanticBuildOwner) releaseIfDone() {
	if owner.weight > 0 && owner.sealed.Load() && owner.remaining.Load() == 0 &&
		owner.released.CompareAndSwap(false, true) {
		readSemaphore.Release(owner.weight)
	}
}

type semanticEncodedBatch struct {
	sequence   int
	units      []semanticUnit
	embeddings []semanticUnitEmbedding
	err        error
}

type semanticBuildOutput struct {
	units           []semanticUnit
	vectorsArtifact semanticSizedArtifact
	err             error
}

type semanticCollectedBatch struct {
	units []semanticUnit
}

type semanticCoarseBatch struct {
	sequence int
	coarse   []semanticCoarseVectors
}

type semanticDocumentSource func(context.Context) (<-chan fileContent, func() error)

func runSemanticBuild(
	ctx context.Context,
	model *semanticModelFuture,
	build func(semanticModel) error,
) error {
	if model == nil || build == nil {
		return errRerankUnavailable
	}
	embedder, err := model.model(ctx)
	if err != nil {
		return err
	}
	return build(embedder)
}

// semanticConcurrencyController changes only the number of model calls in
// flight. It measures one warm-up call, then probes wider calls while complete
// throughput improves and live memory permits growth. Source-limited windows
// do not describe model throughput and cannot reduce or increase the width.
type semanticConcurrencyController struct {
	limit              int
	previousLimit      int
	bestLimit          int
	performanceCeiling int
	performanceCPUs    int
	bestRate           float64
	growthCost         int64
	windowStarted      time.Time
	windowAvailable    int64
	windowBatches      int
	windowRows         int
	windowLimit        int
	windowSaturated    bool
	growthSampleValid  bool
	warmupDone         bool
}

func newSemanticConcurrencyController(
	now time.Time,
	available int64,
) *semanticConcurrencyController {
	return &semanticConcurrencyController{
		limit:             1,
		bestLimit:         1,
		windowStarted:     now,
		windowAvailable:   available,
		growthSampleValid: true,
	}
}

func (controller *semanticConcurrencyController) activeLimit(cpuLimit int) int {
	ceiling := max(1, cpuLimit)
	if controller.performanceCPUs == 0 {
		controller.performanceCPUs = ceiling
	} else if controller.performanceCPUs != ceiling {
		// A changed CPU allowance invalidates the old service curve. Probe the
		// newly available widths instead of keeping a stale ceiling.
		controller.performanceCeiling = 0
		controller.performanceCPUs = ceiling
		controller.bestRate = 0
		controller.bestLimit = min(controller.limit, ceiling)
		controller.invalidateWindow()
		controller.growthSampleValid = false
	}
	if controller.performanceCeiling > 0 {
		ceiling = min(ceiling, controller.performanceCeiling)
	}
	if controller.limit > ceiling {
		controller.limit = ceiling
	}
	return max(1, controller.limit)
}

func (controller *semanticConcurrencyController) noteActive(active int, now time.Time) {
	if active < controller.limit ||
		(controller.windowSaturated && controller.windowLimit == controller.limit) {
		return
	}
	controller.windowStarted = now
	controller.windowBatches = 0
	controller.windowRows = 0
	controller.windowLimit = controller.limit
	controller.windowSaturated = true
}

func (controller *semanticConcurrencyController) noteStarved() {
	controller.invalidateWindow()
	controller.growthSampleValid = false
	if controller.performanceCeiling > 0 {
		// A source gap starts a new service epoch. Measure the retained width
		// again, then allow a wider probe if prepared work becomes available.
		controller.performanceCeiling = 0
		controller.bestRate = 0
		controller.bestLimit = controller.limit
	}
}

func (controller *semanticConcurrencyController) noteInputClosed() {
	controller.invalidateWindow()
}

func (controller *semanticConcurrencyController) noteCompletion(rows int) bool {
	if !controller.windowSaturated || controller.windowLimit != controller.limit {
		return false
	}
	controller.windowBatches++
	controller.windowRows += rows
	required := controller.windowLimit
	if controller.warmupDone {
		required *= 2
	}
	return controller.windowBatches >= required
}

func (controller *semanticConcurrencyController) evaluate(
	now time.Time,
	cpuLimit int,
	available int64,
) {
	limit := controller.activeLimit(cpuLimit)
	if !controller.windowSaturated || controller.windowLimit != limit {
		return
	}
	elapsed := now.Sub(controller.windowStarted)
	if elapsed <= 0 || controller.windowRows <= 0 {
		controller.resetWindow(now, available)
		return
	}
	warmup := !controller.warmupDone
	if warmup {
		// Provider setup and specialization can dominate the first call. Use it
		// to measure memory, but do not compare its rate with steady work.
		controller.warmupDone = true
	}

	changedWorkers := limit - controller.previousLimit
	if controller.growthSampleValid && changedWorkers > 0 && available > 0 &&
		controller.windowAvailable > available {
		drop := controller.windowAvailable - available
		controller.growthCost = (drop + int64(changedWorkers) - 1) / int64(changedWorkers)
		if drop > available && limit > 1 {
			controller.limit = max(1, controller.previousLimit)
			controller.previousLimit = controller.limit
			controller.resetWindow(now, available)
			return
		}
	}

	rate := float64(controller.windowRows) / elapsed.Seconds()
	if !warmup {
		if controller.bestRate == 0 || rate > controller.bestRate {
			controller.bestRate = rate
			controller.bestLimit = limit
		} else if limit > controller.bestLimit {
			controller.performanceCeiling = controller.bestLimit
			controller.performanceCPUs = max(1, cpuLimit)
			controller.limit = controller.bestLimit
			controller.previousLimit = controller.limit
			controller.resetWindow(now, available)
			return
		}
	}

	ceiling := max(1, cpuLimit)
	if controller.performanceCeiling > 0 {
		ceiling = min(ceiling, controller.performanceCeiling)
	}
	next := nextSemanticProbeWidth(limit, ceiling)
	if available > 0 && controller.growthCost > 0 {
		// The current width already fits. Free memory limits only added calls;
		// retained index output must not be charged as fresh call memory.
		memoryLimit := limit + int(available/controller.growthCost)
		next = min(next, memoryLimit)
	}
	if next < limit {
		controller.limit = next
		controller.previousLimit = controller.limit
		controller.resetWindow(now, available)
		return
	}
	added := next - limit
	if added > 0 {
		controller.previousLimit = limit
		controller.limit = next
	} else {
		controller.previousLimit = limit
	}
	controller.resetWindow(now, available)
}

func nextSemanticProbeWidth(current, ceiling int) int {
	current = max(1, current)
	ceiling = max(1, ceiling)
	if current >= ceiling {
		return ceiling
	}
	return min(ceiling, max(current+1, current*2))
}

func (controller *semanticConcurrencyController) resetWindow(now time.Time, available int64) {
	controller.windowStarted = now
	controller.windowAvailable = available
	controller.growthSampleValid = true
	controller.invalidateWindow()
}

func (controller *semanticConcurrencyController) invalidateWindow() {
	controller.windowBatches = 0
	controller.windowRows = 0
	controller.windowLimit = 0
	controller.windowSaturated = false
}

// buildSemanticGitGeneration reads the same captured commit as the Zoekt
// branch. Extraction keeps source order. Model completion order cannot change
// row IDs or artifact bytes.
func buildSemanticGitGeneration(
	ctx context.Context,
	repoDir string,
	snapshot gitSnapshot,
	scope *gitDirtyScope,
	dir string,
	source string,
	embedder semanticModel,
	resources searchResources,
) (semanticManifest, error) {
	if err := requireNativeGitVersion(ctx, repoDir); err != nil {
		return semanticManifest{}, err
	}
	return buildSemanticGeneration(
		ctx,
		dir,
		source,
		embedder,
		resources,
		func(streamCtx context.Context) (<-chan fileContent, func() error) {
			documents, result := streamNativeGitDocuments(streamCtx, repoDir, snapshot, scope)
			return documents, func() error { return (<-result).err }
		},
	)
}

func buildSemanticFolderGeneration(
	ctx context.Context,
	selected []folderCandidate,
	dir string,
	source string,
	embedder semanticModel,
	resources searchResources,
) (semanticManifest, error) {
	return buildSemanticGeneration(
		ctx,
		dir,
		source,
		embedder,
		resources,
		func(streamCtx context.Context) (<-chan fileContent, func() error) {
			documents := streamFolderFiles(streamCtx, selected, resources.cpuLimit())
			return documents, func() error { return nil }
		},
	)
}

func buildSemanticGeneration(
	ctx context.Context,
	dir string,
	source string,
	embedder semanticModel,
	resources searchResources,
	documentSource semanticDocumentSource,
) (semanticManifest, error) {
	started := time.Now()
	if embedder == nil || documentSource == nil {
		return semanticManifest{}, errRerankUnavailable
	}
	if err := prepareSemanticGenerationDirectory(dir); err != nil {
		return semanticManifest{}, err
	}
	if err := checkCtagsCached(); err != nil {
		return semanticManifest{}, err
	}
	workerCount := resources.cpuLimit()
	parsers := make([]semanticTagParser, workerCount)
	for index := range parsers {
		parser, err := ctags.New(ctags.Options{Bin: os.Getenv("CTAGS_COMMAND")})
		if err != nil {
			for _, opened := range parsers[:index] {
				opened.Close()
			}
			return semanticManifest{}, fmt.Errorf("start semantic ctags worker: %w", err)
		}
		parsers[index] = parser
	}
	defer func() {
		for _, parser := range parsers {
			parser.Close()
		}
	}()

	buildCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	documents, finishSource := documentSource(buildCtx)
	if documents == nil || finishSource == nil {
		return semanticManifest{}, fmt.Errorf("semantic document source is unavailable")
	}
	jobs := make(chan semanticSourceDocument)
	extracted := make(chan semanticExtractedFile)
	dispatchDone := make(chan int, 1)
	go dispatchSemanticDocuments(buildCtx, documents, jobs, dispatchDone)

	var extractWait sync.WaitGroup
	for _, parser := range parsers {
		extractWait.Add(1)
		go func(parser semanticTagParser) {
			defer extractWait.Done()
			extractSemanticDocuments(buildCtx, parser, embedder, resources, jobs, extracted, cancel)
		}(parser)
	}
	go func() {
		extractWait.Wait()
		close(extracted)
	}()

	batches := make(chan semanticBuildBatch)
	encoded := make(chan semanticEncodedBatch)
	go encodeSemanticBatches(
		buildCtx,
		embedder,
		batches,
		encoded,
		cancel,
		resources.withModelCallCost(semanticModelCallCPUs(buildCtx, embedder)),
	)
	coarseReady := make(chan semanticCoarseBatch)
	usearchBuilt := make(chan semanticUSearchBuildResult, 1)
	go buildSemanticUSearchStream(buildCtx, dir, coarseReady, cancel, resources, usearchBuilt)
	collected := make(chan semanticBuildOutput, 1)
	go collectSemanticBatches(
		encoded,
		filepath.Join(dir, semanticVectorsFile),
		coarseReady,
		collected,
	)

	nextFile := 0
	nextRow := uint64(0)
	batchSequence := 0
	pending := make(map[int]semanticExtractedFile, workerCount)
	pendingBatch := semanticBuildBatch{}
	failed := false
	var extractionErr error
	sendBatch := func() {
		if len(pendingBatch.units) == 0 {
			return
		}
		pendingBatch.sequence = batchSequence
		batchSequence++
		select {
		case batches <- pendingBatch:
		case <-buildCtx.Done():
			releaseSemanticBuildBatch(pendingBatch)
			failed = true
		}
		pendingBatch = semanticBuildBatch{}
	}

	for result := range extracted {
		if result.err != nil {
			if extractionErr == nil {
				extractionErr = result.err
				cancel()
			}
			if result.weight > 0 {
				readSemaphore.Release(result.weight)
			}
			failed = true
			continue
		}
		if failed || buildCtx.Err() != nil {
			if result.weight > 0 {
				readSemaphore.Release(result.weight)
			}
			failed = true
			continue
		}
		pending[result.sequence] = result
		for {
			ready, ok := pending[nextFile]
			if !ok {
				break
			}
			delete(pending, nextFile)
			nextFile++
			for index := range ready.units {
				ready.units[index].row = nextRow
				nextRow++
			}
			if len(ready.units) == 0 {
				if ready.weight > 0 {
					readSemaphore.Release(ready.weight)
				}
				continue
			}
			owner := &semanticBuildOwner{weight: ready.weight}
			for len(ready.units) > 0 && !failed {
				count := min(len(ready.units), semanticModelBatchRows-len(pendingBatch.units))
				owner.retain()
				pendingBatch.owners = append(pendingBatch.owners, owner)
				pendingBatch.units = append(pendingBatch.units, ready.units[:count]...)
				ready.units = ready.units[count:]
				if len(pendingBatch.units) == semanticModelBatchRows {
					sendBatch()
				}
			}
			owner.seal()
			if failed {
				break
			}
		}
	}
	for _, result := range pending {
		if result.weight > 0 {
			readSemaphore.Release(result.weight)
		}
	}
	// These values are cumulative milestones from started. Extraction can block
	// behind inference backpressure, so the first value is not isolated extractor
	// CPU time. The second includes extraction, inference, vector collection, and
	// USearch construction.
	elapsedToExtractionDrain := time.Since(started)
	if !failed {
		sendBatch()
	} else {
		releaseSemanticBuildBatch(pendingBatch)
	}
	close(batches)
	output := <-collected
	usearchOutput := <-usearchBuilt
	elapsedToModelAndUSearch := time.Since(started)
	dispatched := <-dispatchDone
	streamErr := finishSource()
	if err := ctx.Err(); err != nil {
		return semanticManifest{}, err
	}
	if streamErr != nil {
		return semanticManifest{}, streamErr
	}
	if extractionErr != nil {
		return semanticManifest{}, extractionErr
	}
	if usearchOutput.err != nil && (output.err == nil || errors.Is(output.err, context.Canceled)) {
		return semanticManifest{}, usearchOutput.err
	}
	if output.err != nil {
		return semanticManifest{}, output.err
	}
	if usearchOutput.err != nil {
		return semanticManifest{}, usearchOutput.err
	}
	if uint64(len(output.units)) != nextRow {
		return semanticManifest{}, fmt.Errorf(
			"semantic model completed %d rows, want %d",
			len(output.units),
			nextRow,
		)
	}
	if failed || nextFile != dispatched {
		return semanticManifest{}, fmt.Errorf("semantic extraction stopped before all files completed")
	}
	writeStarted := time.Now()
	manifest, err := writeSemanticGenerationFromParts(
		ctx,
		dir,
		source,
		output.units,
		output.vectorsArtifact,
		usearchOutput.shards,
	)
	if err == nil {
		slog.Debug(
			"Built semantic index",
			"files", dispatched,
			"rows", len(output.units),
			"elapsed_to_extraction_drain", elapsedToExtractionDrain,
			"elapsed_to_model_and_usearch", elapsedToModelAndUSearch,
			"write", time.Since(writeStarted),
			"total", time.Since(started),
		)
	}
	return manifest, err
}

func dispatchSemanticDocuments(
	ctx context.Context,
	documents <-chan fileContent,
	jobs chan<- semanticSourceDocument,
	done chan<- int,
) {
	defer close(jobs)
	sequence := 0
	for document := range documents {
		job := semanticSourceDocument{sequence: sequence, content: document}
		select {
		case jobs <- job:
			sequence++
		case <-ctx.Done():
			if document.weight > 0 {
				readSemaphore.Release(document.weight)
			}
			for document = range documents {
				if document.weight > 0 {
					readSemaphore.Release(document.weight)
				}
			}
			done <- sequence
			return
		}
	}
	done <- sequence
}

func extractSemanticDocuments(
	ctx context.Context,
	parser semanticTagParser,
	packer semanticModel,
	resources searchResources,
	jobs <-chan semanticSourceDocument,
	results chan<- semanticExtractedFile,
	cancel context.CancelFunc,
) {
	for job := range jobs {
		document := job.content
		cpu, admissionErr := resources.acquireCPU(ctx, 1)
		if admissionErr != nil {
			if document.weight > 0 {
				readSemaphore.Release(document.weight)
			}
			return
		}
		var entries []*ctags.Entry
		var parseErr error
		if len(document.content) > 0 && !semanticContentIsBinary(document.content) &&
			!semanticContentIsGenerated(document.content) {
			entries, parseErr = parser.Parse(document.name, document.content)
		}
		units := extractSemanticUnits(document.name, document.content, entries, parseErr)
		var packErr error
		if len(units) > 0 {
			units, packErr = packer.PackSemanticUnits(ctx, units)
			if packErr != nil {
				cancel()
			}
		}
		cpu.release()
		result := semanticExtractedFile{
			sequence: job.sequence,
			units:    units,
			weight:   document.weight,
			err:      packErr,
		}
		select {
		case results <- result:
		case <-ctx.Done():
			if result.weight > 0 {
				readSemaphore.Release(result.weight)
			}
			return
		}
	}
}

func encodeSemanticBatches(
	ctx context.Context,
	embedder semanticModel,
	batches <-chan semanticBuildBatch,
	encoded chan<- semanticEncodedBatch,
	cancel context.CancelFunc,
	resources searchResources,
) {
	defer close(encoded)
	completed := make(chan semanticEncodedBatch)
	// Measure free memory after the fixed model session exists. The controller
	// must estimate per-call growth, not charge every call for session setup.
	initialAvailable := resources.freeMemory()
	controller := newSemanticConcurrencyController(resources.currentTime(), initialAvailable)
	slog.Debug(
		"Calibrating semantic inference concurrency",
		"cpu_limit", resources.cpuLimit(),
		"cpu_per_call", resources.callCPUs,
		"call_limit", resources.callLimit(),
		"available_memory", initialAvailable,
	)
	active := 0
	maxActive := 0
	startedBatches := 0
	completedBatches := 0
	completedRows := 0
	// inputEmptyPolls counts non-blocking input checks that found no prepared
	// batch while the controller had capacity. It is an event count, not time.
	inputEmptyPolls := 0
	input := batches

	start := func(batch semanticBuildBatch) {
		if ctx.Err() != nil {
			releaseSemanticBuildBatch(batch)
			return
		}
		cpu, admissionErr := resources.acquireCPU(ctx, resources.callCPUs)
		if admissionErr != nil {
			releaseSemanticBuildBatch(batch)
			return
		}
		active++
		startedBatches++
		maxActive = max(maxActive, active)
		controller.noteActive(active, resources.currentTime())
		go func() {
			defer cpu.release()
			embeddings, err := embedder.EmbedSemanticUnits(ctx, batch.units)
			releaseSemanticBuildBatch(batch)
			completed <- semanticEncodedBatch{
				sequence:   batch.sequence,
				units:      batch.units,
				embeddings: embeddings,
				err:        err,
			}
		}()
	}
	finish := func(result semanticEncodedBatch) {
		active--
		completedBatches++
		completedRows += len(result.units)
		if result.err != nil {
			cancel()
		}
		encoded <- result
		if controller.noteCompletion(len(result.units)) {
			callLimit := resources.callLimit()
			before := controller.activeLimit(callLimit)
			available := resources.freeMemory()
			controller.evaluate(
				resources.currentTime(),
				callLimit,
				available,
			)
			after := controller.activeLimit(resources.callLimit())
			if after != before {
				slog.Debug(
					"Adjusted semantic inference concurrency",
					"from", before,
					"to", after,
					"available_memory", available,
				)
			}
		}
	}

	for input != nil || active > 0 {
		limit := controller.activeLimit(resources.callLimit())
		if input != nil && active < limit {
			select {
			case batch, ok := <-input:
				if !ok {
					input = nil
					controller.noteInputClosed()
					continue
				}
				start(batch)
				continue
			default:
			}
			inputEmptyPolls++
			controller.noteStarved()
			if active == 0 {
				batch, ok := <-input
				if !ok {
					input = nil
					controller.noteInputClosed()
					continue
				}
				start(batch)
				continue
			}
			select {
			case batch, ok := <-input:
				if !ok {
					input = nil
					controller.noteInputClosed()
					continue
				}
				start(batch)
			case result := <-completed:
				finish(result)
			}
			continue
		}
		if active > 0 {
			finish(<-completed)
			continue
		}
		batch, ok := <-input
		if !ok {
			input = nil
			controller.noteInputClosed()
			continue
		}
		start(batch)
	}
	cpuLimit := resources.cpuLimit()
	callLimit := resources.callLimit()
	slog.Debug(
		"Semantic inference scheduler finished",
		"batches", completedBatches,
		"rows", completedRows,
		"started_batches", startedBatches,
		"max_in_flight", maxActive,
		"final_limit", controller.activeLimit(callLimit),
		"call_limit", callLimit,
		"cpu_limit", cpuLimit,
		"input_empty_polls", inputEmptyPolls,
	)
}

func collectSemanticBatches(
	encoded <-chan semanticEncodedBatch,
	vectorsPath string,
	coarseReady chan<- semanticCoarseBatch,
	done chan<- semanticBuildOutput,
) {
	if coarseReady != nil {
		defer close(coarseReady)
	}
	var output semanticBuildOutput
	batches := make(map[int]semanticCollectedBatch)
	maxSequence := -1
	totalRows := 0
	maxBatches := (semanticMaxRows + semanticModelBatchRows - 1) / semanticModelBatchRows
	vectorFile, err := os.OpenFile(vectorsPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		output.err = err
	}
	vectorBuffer := make([]byte, semanticModelBatchRows*semanticFineVectorBytesPerUnit)
	for batch := range encoded {
		for index := range batch.units {
			batch.units[index].text = nil
			batch.units[index].modelInput = nil
		}
		if output.err != nil {
			continue
		}
		if batch.err != nil {
			output.err = batch.err
			continue
		}
		if batch.sequence < 0 || batch.sequence >= maxBatches {
			output.err = fmt.Errorf("semantic model batch %d exceeds limit", batch.sequence)
			continue
		}
		if _, exists := batches[batch.sequence]; exists {
			output.err = fmt.Errorf("semantic model batch %d was repeated", batch.sequence)
			continue
		}
		if len(batch.units) == 0 || len(batch.units) != len(batch.embeddings) ||
			len(batch.units) > semanticModelBatchRows {
			output.err = fmt.Errorf("semantic model batch %d has mismatched rows", batch.sequence)
			continue
		}
		start := batch.sequence * semanticModelBatchRows
		for index, unit := range batch.units {
			wantRow := start + index
			if unit.row != uint64(wantRow) || wantRow >= semanticMaxRows {
				output.err = fmt.Errorf(
					"semantic model batch %d row %d is %d, want %d",
					batch.sequence,
					index,
					unit.row,
					wantRow,
				)
				break
			}
		}
		if output.err != nil {
			continue
		}
		encodedVectors, err := encodeSemanticFineVectors(vectorBuffer, batch.embeddings, start)
		if err != nil {
			output.err = err
			continue
		}
		offset := int64(start * semanticFineVectorBytesPerUnit)
		written, err := vectorFile.WriteAt(encodedVectors, offset)
		if err != nil || written != len(encodedVectors) {
			if err == nil {
				err = fmt.Errorf("semantic vector write was short")
			}
			output.err = err
			continue
		}
		coarse := make([]semanticCoarseVectors, len(batch.embeddings))
		for row := range batch.embeddings {
			coarse[row] = batch.embeddings[row].coarse
		}
		batches[batch.sequence] = semanticCollectedBatch{units: batch.units}
		if coarseReady != nil {
			coarseReady <- semanticCoarseBatch{sequence: batch.sequence, coarse: coarse}
		}
		maxSequence = max(maxSequence, batch.sequence)
		totalRows += len(batch.units)
	}
	if output.err == nil && len(batches) > 0 {
		if len(batches) != maxSequence+1 {
			for sequence := range maxSequence + 1 {
				if _, exists := batches[sequence]; !exists {
					output.err = fmt.Errorf("semantic model batch %d did not complete", sequence)
					break
				}
			}
		}
	}
	if output.err == nil {
		output.units = make([]semanticUnit, 0, totalRows)
		for sequence := range maxSequence + 1 {
			batch := batches[sequence]
			if sequence < maxSequence && len(batch.units) != semanticModelBatchRows {
				output.err = fmt.Errorf(
					"semantic model batch %d has %d rows, want %d",
					sequence,
					len(batch.units),
					semanticModelBatchRows,
				)
				break
			}
			output.units = append(output.units, batch.units...)
		}
	}
	if vectorFile != nil {
		if output.err == nil {
			wantBytes, sizeOK := checkedSemanticVectorBytes(uint64(totalRows))
			if !sizeOK {
				output.err = fmt.Errorf("semantic vector size is invalid")
			} else if truncateErr := vectorFile.Truncate(int64(wantBytes)); truncateErr != nil {
				output.err = truncateErr
			} else {
				output.vectorsArtifact = semanticSizedArtifact{Bytes: wantBytes}
			}
		}
		if closeErr := vectorFile.Close(); output.err == nil && closeErr != nil {
			output.err = closeErr
		}
	}
	done <- output
}

func releaseSemanticBuildBatch(batch semanticBuildBatch) {
	for _, owner := range batch.owners {
		owner.done()
	}
}
