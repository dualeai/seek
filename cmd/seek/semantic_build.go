package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

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
	units  []semanticUnit
	owners []*semanticBuildOwner
}

// semanticBuildOwner keeps one source file admitted until every model batch
// that reads a view of its bytes has finished tokenization and inference.
// refs starts at 1 for the extraction loop, which calls retain for each batch
// and then done after its last retain. The call that sets refs to 0 releases
// the weight.
type semanticBuildOwner struct {
	weight int64
	refs   atomic.Int64
}

func newSemanticBuildOwner(weight int64) *semanticBuildOwner {
	owner := &semanticBuildOwner{weight: weight}
	owner.refs.Store(1)
	return owner
}

func (owner *semanticBuildOwner) retain() {
	owner.refs.Add(1)
}

func (owner *semanticBuildOwner) done() {
	if owner.refs.Add(-1) == 0 && owner.weight > 0 {
		readSemaphore.Release(owner.weight)
	}
}

type semanticEncodedBatch struct {
	units      []semanticUnit
	embeddings []semanticUnitEmbedding
	err        error
}

// semanticCopiedRow is a row whose vectors the build copies from a live
// generation or the unit store (semanticReuseIndex).
type semanticCopiedRow struct {
	row uint64
	ref semanticReuseRef
}

type semanticBuildOutput struct {
	rows            uint64
	vectorsArtifact semanticSizedArtifact
	coarseArtifact  semanticSizedArtifact
	err             error
}

type semanticCoarseBatch struct {
	sequence int
	coarse   []semanticCoarseVectors
}

// semanticDocumentSource streams the files of a build. The folder source sends
// them in name order, and the Git source in git ls-tree order. The overlay
// source of the dirty files sends each file when its read ends. The function
// that it also returns gives the read error of the source after the channel
// closes.
type semanticDocumentSource func(context.Context) (<-chan fileContent, func() error)

// folderReadSource gives a document source that streams files with stream.
// stream gets a read context and its cancel function. When a
// *folderReadError ends the read context, the finish function returns it.
func folderReadSource(stream func(context.Context, context.CancelCauseFunc) <-chan fileContent) semanticDocumentSource {
	return func(streamCtx context.Context) (<-chan fileContent, func() error) {
		readCtx, cancel := context.WithCancelCause(streamCtx)
		documents := stream(readCtx, cancel)
		return documents, func() error {
			defer cancel(nil)
			if readErr, ok := errors.AsType[*folderReadError](context.Cause(readCtx)); ok {
				return readErr
			}
			return nil
		}
	}
}

// runSemanticBuild waits for the command's model and runs build with it. A
// build runs it only when the model stages run (searchExecution.buildsVectors
// or buildsOverlay), so model is not nil.
func runSemanticBuild(
	ctx context.Context,
	model *semanticModelFuture,
	build func(semanticModel) error,
) error {
	embedder, err := model.model(ctx)
	if err != nil {
		return err
	}
	return build(embedder)
}

// semanticConcurrencyController changes only the number of model calls that
// run at the same time. It measures one warm-up call. Then it probes wider
// calls while the measured throughput improves and free memory allows more.
// A window that waited for the source does not measure the model, so it does
// not change the width.
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
		// A changed CPU allowance makes the old measurements wrong. Probe the
		// widths that are now possible, and drop the old ceiling.
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

// noteActive starts a measurement window when active calls fill the width. A
// window that already runs at this width keeps its start.
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
		// A gap in the source starts a new measurement. Measure the kept width
		// again, then allow a wider probe when prepared work is available.
		controller.performanceCeiling = 0
		controller.bestRate = 0
		controller.bestLimit = controller.limit
	}
}

func (controller *semanticConcurrencyController) noteInputClosed() {
	controller.invalidateWindow()
}

// noteCompletion counts one finished call of rows rows in the window. It
// reports true when the window holds enough calls to evaluate: the width in
// calls for the warm-up, and twice the width after it.
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

// evaluate measures a complete window and sets the next width. The warm-up
// window measures only memory. After a wider probe, a memory drop larger than
// the free memory returns to the previous width. A wider width whose rate is
// not above the best rate returns to the best width, which then caps the
// width. Else the width doubles, up to cpuLimit and the cap. The free memory
// also limits the added calls, at the measured cost of one added call.
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
		// Provider setup and specialization can take most of the time of the
		// first call. Use it to measure memory, but do not compare its rate
		// with steady work.
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
		// kept index output must not count as new call memory.
		memoryLimit := limit + int(available/controller.growthCost)
		next = min(next, memoryLimit)
	}
	controller.previousLimit = min(limit, next)
	controller.limit = next
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
// row IDs or artifact bytes. indexDir holds the node's previous generations,
// whose rows the build copies.
func buildSemanticGitGeneration(
	ctx context.Context,
	repoDir string,
	snapshot gitObjectID,
	indexDir string,
	lineage string,
	dir string,
	source string,
	embedder semanticModel,
	resources searchResources,
	missing *[]missingBlob,
) error {
	if err := requireNativeGitVersion(ctx); err != nil {
		return err
	}
	reuse := loadSemanticReuse(indexDir, lineage, "", nil, "")
	defer reuse.Close()
	return buildSemanticGeneration(
		ctx,
		dir,
		source,
		embedder,
		resources,
		func(streamCtx context.Context) (<-chan fileContent, func() error) {
			documents, result := streamNativeGitDocuments(streamCtx, repoDir, snapshot, missing, nil)
			return documents, func() error { return <-result }
		},
		reuse,
	)
}

// buildSemanticFolderGeneration embeds the selected files of a folder node
// whose model path base is base. indexDir holds the node's previous
// generations, whose rows the build copies. A node with none copies the rows
// of the corpora of its lineage (the folders that hold it or that it holds).
// A read error that can be temporary fails the build, so the next search reads
// the file again.
func buildSemanticFolderGeneration(
	ctx context.Context,
	selected []folderCandidate,
	indexDir string,
	lineage string,
	base string,
	dir string,
	source string,
	embedder semanticModel,
	resources searchResources,
) error {
	names := make([]string, len(selected))
	for index, candidate := range selected {
		names[index] = candidate.name
	}
	reuse := loadSemanticReuse(indexDir, lineage, base, names, "")
	defer reuse.Close()
	return buildSemanticGeneration(
		ctx,
		dir,
		source,
		embedder,
		resources,
		folderReadSource(func(readCtx context.Context, cancel context.CancelCauseFunc) <-chan fileContent {
			return streamFolderFilesInOrder(readCtx, selected, resources.cpuLimit(), cancel)
		}),
		reuse,
	)
}

func buildSemanticGeneration(
	ctx context.Context,
	dir string,
	source string,
	embedder semanticModel,
	resources searchResources,
	documentSource semanticDocumentSource,
	reuse *semanticReuseIndex,
) error {
	started := time.Now()
	if err := prepareSemanticGenerationDirectory(dir); err != nil {
		return err
	}
	if err := checkCtagsCached(); err != nil {
		return err
	}
	workerCount := resources.cpuLimit()
	parsers, err := startSemanticCtagsParsers(workerCount)
	if err != nil {
		return err
	}
	defer func() {
		for _, parser := range parsers {
			parser.Close()
		}
	}()
	tokenizers, err := takeSemanticWindowTokenizers(ctx, workerCount)
	if err != nil {
		return err
	}
	defer func() {
		for _, tokenizer := range tokenizers {
			lateOnTokenizers.release(tokenizer)
		}
	}()

	buildCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	documents, finishSource := documentSource(buildCtx)
	jobs := make(chan semanticSourceDocument)
	extracted := make(chan semanticExtractedFile)
	dispatchDone := make(chan int, 1)
	go dispatchSemanticDocuments(buildCtx, documents, jobs, dispatchDone)

	var extractWait sync.WaitGroup
	for worker, parser := range parsers {
		extractWait.Go(func() {
			extractSemanticDocuments(buildCtx, parser, tokenizers[worker], reuse, resources, jobs, extracted)
		})
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
		resources.withModelCallCost(embedder.SemanticCallCPUs(buildCtx)),
	)
	// An overlay gets no USearch graph. Its search scores every row with the
	// exact fine vectors, and the graph would be half the cost of a save.
	// The planner cuts the rows into graph shards as the files come in order.
	var coarseReady chan semanticCoarseBatch
	var shardPlans chan semanticUSearchShardPlan
	var planner *semanticUSearchShardPlanner
	usearchBuilt := make(chan semanticUSearchBuildResult, 1)
	if semanticSourceHasGraph(source) {
		coarseReady = make(chan semanticCoarseBatch)
		shardPlans = make(chan semanticUSearchShardPlan)
		planner = newSemanticUSearchShardPlanner(resources.cpuLimit())
		go buildSemanticUSearchStream(
			buildCtx, dir, coarseReady, shardPlans, reuse.shards, cancel, resources, usearchBuilt,
		)
	} else {
		usearchBuilt <- semanticUSearchBuildResult{}
	}
	copied := make(chan []semanticCopiedRow)
	collected := make(chan semanticBuildOutput, 1)
	go collectSemanticBatches(
		encoded,
		copied,
		dir,
		coarseReady,
		cancel,
		collected,
	)

	nextFile := 0
	pending := make(map[int]semanticExtractedFile, workerCount)
	pendingBatch := semanticBuildBatch{}
	var pendingCopy []semanticCopiedRow
	// rows is the stored metadata of every row, in row order. A row's ID is
	// its index.
	var rows []semanticUnit
	failed := false
	var extractionErr error
	sendBatch := func() {
		if len(pendingBatch.units) == 0 {
			return
		}
		select {
		case batches <- pendingBatch:
		case <-buildCtx.Done():
			releaseSemanticBuildBatch(pendingBatch)
			failed = true
		}
		pendingBatch = semanticBuildBatch{}
	}
	sendCopy := func() {
		if len(pendingCopy) == 0 {
			return
		}
		select {
		case copied <- pendingCopy:
		case <-buildCtx.Done():
			failed = true
		}
		pendingCopy = nil
	}
	sendPlans := func(plans []semanticUSearchShardPlan, err error) {
		if err != nil {
			failed = true
			if extractionErr == nil {
				extractionErr = err
				cancel()
			}
			return
		}
		for _, plan := range plans {
			select {
			case shardPlans <- plan:
			case <-buildCtx.Done():
				failed = true
				return
			}
		}
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
				ready.units[index].row = uint64(len(rows))
				stored := ready.units[index]
				stored.text, stored.modelInput, stored.reuse = nil, nil, nil
				rows = append(rows, stored)
			}
			if planner != nil && !failed {
				sendPlans(planner.addFile(rows[len(rows)-len(ready.units):]))
			}
			if len(ready.units) == 0 {
				if ready.weight > 0 {
					readSemaphore.Release(ready.weight)
				}
				continue
			}
			// A copied row needs no model call; the file's bytes stay held
			// only while a model batch reads a view of them.
			owner := newSemanticBuildOwner(ready.weight)
			retained := false
			for _, unit := range ready.units {
				if failed {
					break
				}
				if unit.reuse != nil {
					pendingCopy = append(pendingCopy, semanticCopiedRow{row: unit.row, ref: *unit.reuse})
					if len(pendingCopy) == semanticModelBatchRows {
						sendCopy()
					}
					continue
				}
				if !retained {
					owner.retain()
					pendingBatch.owners = append(pendingBatch.owners, owner)
					retained = true
				}
				pendingBatch.units = append(pendingBatch.units, unit)
				if len(pendingBatch.units) == semanticModelBatchRows {
					sendBatch()
					retained = false
				}
			}
			owner.done()
			// Copied rows of a file go now, so the USearch stream gets full
			// blocks while the model runs.
			sendCopy()
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
	// These values are times since started. Extraction can wait for the model,
	// so the first value is not the CPU time of extraction alone. The second
	// includes extraction, inference, vector collection, and USearch
	// construction.
	elapsedToExtractionDrain := time.Since(started)
	if !failed {
		sendBatch()
		sendCopy()
	} else {
		releaseSemanticBuildBatch(pendingBatch)
	}
	if planner != nil {
		if !failed {
			sendPlans(planner.finish(), nil)
		}
		close(shardPlans)
	}
	close(batches)
	close(copied)
	output := <-collected
	usearchOutput := <-usearchBuilt
	elapsedToModelAndUSearch := time.Since(started)
	dispatched := <-dispatchDone
	streamErr := finishSource()
	if err := ctx.Err(); err != nil {
		return err
	}
	// A failed stage cancels the build, and the other stages can then return
	// that cancellation: the Git source and an extractor do. So an error that
	// is not a cancellation comes first, in this order.
	stageErrs := []error{streamErr, extractionErr, output.err, usearchOutput.err}
	for _, err := range stageErrs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	if err := cmp.Or(stageErrs...); err != nil {
		return err
	}
	if output.rows != uint64(len(rows)) {
		return fmt.Errorf(
			"semantic build completed %d rows, want %d",
			output.rows,
			len(rows),
		)
	}
	if failed || nextFile != dispatched {
		return fmt.Errorf("semantic extraction stopped before all files completed")
	}
	var shardRows uint64
	if planner != nil && len(rows) > 0 {
		shardRows = planner.shardRows()
	}
	writeStarted := time.Now()
	err = writeSemanticGenerationFromParts(
		ctx,
		dir,
		source,
		reuse.base,
		rows,
		output.vectorsArtifact,
		output.coarseArtifact,
		shardRows,
		usearchOutput.shards,
	)
	if err == nil {
		slog.Debug(
			"Built semantic index",
			"files", dispatched,
			"rows", len(rows),
			"graph_shards_built", usearchOutput.built,
			"graph_shards_reused", usearchOutput.reused,
			"graph_shard_rows", shardRows,
			"elapsed_to_extraction_drain", elapsedToExtractionDrain,
			"elapsed_to_model_and_usearch", elapsedToModelAndUSearch,
			"write", time.Since(writeStarted),
			"total", time.Since(started),
		)
	}
	return err
}

// startSemanticCtagsParsers starts one ctags parser per extraction worker, all
// at the same time. Each start waits for the first reply of its process: 18
// starts in series took 170 ms, and at the same time 37 ms. A failed start
// closes the parsers that started and returns the error of the first worker.
func startSemanticCtagsParsers(count int) ([]ctags.Parser, error) {
	parsers := make([]ctags.Parser, count)
	errs := make([]error, count)
	var wait sync.WaitGroup
	for index := range parsers {
		wait.Go(func() {
			// TODO(https://github.com/sourcegraph/go-ctags/issues/27): go-ctags
			// drops a tag whose reply line has 4,096 bytes or more, with no
			// error. Such a symbol then gets no unit of its own.
			parsers[index], errs[index] = ctags.New(ctags.Options{Bin: os.Getenv(envCtagsCommand)})
		})
	}
	wait.Wait()
	for _, err := range errs {
		if err == nil {
			continue
		}
		for _, parser := range parsers {
			if parser != nil {
				parser.Close()
			}
		}
		return nil, fmt.Errorf("start semantic ctags worker: %w", err)
	}
	return parsers, nil
}

// takeSemanticWindowTokenizers takes one tokenizer per extraction worker for
// the window split, building the missing ones at the same time.
func takeSemanticWindowTokenizers(ctx context.Context, count int) ([]*lateOnTokenizer, error) {
	tokenizers := make([]*lateOnTokenizer, count)
	errs := make([]error, count)
	var wait sync.WaitGroup
	for index := range tokenizers {
		wait.Go(func() {
			tokenizers[index], errs[index] = lateOnTokenizers.take(ctx)
		})
	}
	wait.Wait()
	if err := errors.Join(errs...); err != nil {
		for _, tokenizer := range tokenizers {
			lateOnTokenizers.release(tokenizer)
		}
		return nil, fmt.Errorf("start semantic window tokenizer: %w", err)
	}
	return tokenizers, nil
}

// dispatchSemanticDocuments numbers the documents in source order and sends
// them to jobs. It sends to done the number of documents that it sent. After
// a cancel, it drains documents and releases their read weight.
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

// extractSemanticDocuments parses and windows the files of jobs and sends
// each to results. It does not cancel ctx on an error: the build loop gets
// the error first and cancels the build. A cancel here would race the send,
// and the build would lose its first error.
func extractSemanticDocuments(
	ctx context.Context,
	parser ctags.Parser,
	tokenizer *lateOnTokenizer,
	reuse *semanticReuseIndex,
	resources searchResources,
	jobs <-chan semanticSourceDocument,
	results chan<- semanticExtractedFile,
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
		var units []semanticUnit
		var packErr error
		if semanticContentIsEligible(document.content) {
			contentID := semanticContentID(sha256.Sum256(document.content))
			var reused bool
			// A file that a live generation holds under the same path tail
			// and content copies its rows with no parse and no tokenization.
			if units, reused = reuse.fileRows(document.name, contentID); !reused {
				entries, parseErr := parser.Parse(document.name, document.content)
				units = extractSemanticUnitsWithID(document.name, document.content, contentID, entries, parseErr)
				if len(units) > 0 {
					units, packErr = windowSemanticUnits(ctx, tokenizer, units, reuse.base)
				}
				if packErr == nil {
					reuse.markReused(units)
				}
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

// encodeSemanticBatches runs one model call for each batch, as many at the
// same time as the concurrency controller and the CPU leases allow. It sends
// each result to encoded in completion order. A failed call cancels the build.
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
		"provider", semanticProviderName(),
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
			embeddings, err := embedder.EmbedSemanticUnits(ctx, batch.units)
			// Give the lease back before the send. The loop of another build of
			// the command can wait for CPU while this result waits for this
			// loop.
			cpu.release()
			releaseSemanticBuildBatch(batch)
			completed <- semanticEncodedBatch{
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

	receive := func(batch semanticBuildBatch, ok bool) {
		if !ok {
			input = nil
			controller.noteInputClosed()
			return
		}
		start(batch)
	}
	for input != nil || active > 0 {
		limit := controller.activeLimit(resources.callLimit())
		// In both cases, at least one call runs: the loop condition covers a
		// closed input, and activeLimit is at least 1.
		if input == nil || active >= limit {
			finish(<-completed)
			continue
		}
		select {
		case batch, ok := <-input:
			receive(batch, ok)
			continue
		default:
		}
		inputEmptyPolls++
		controller.noteStarved()
		// With no running call, nothing sends on completed, so this select
		// waits for input.
		select {
		case batch, ok := <-input:
			receive(batch, ok)
		case result := <-completed:
			finish(result)
		}
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

// collectSemanticBatches writes each row's fine and coarse vectors at the
// row's offset. The rows are those that the model embedded, out of order, and
// those copied from a live generation or the unit store. It sends the coarse
// vectors of each complete 128-row block to the USearch stream after its
// writes succeed. Both paths store the coarse vectors as SNORM16 and send
// their decoded values, so a copied row gives the same bytes as an embedded
// one. On the first collector error, it keeps that error, cancels shared build
// work, and drains both inputs until their producers close them.
func collectSemanticBatches(
	encoded <-chan semanticEncodedBatch,
	copied <-chan []semanticCopiedRow,
	dir string,
	coarseReady chan<- semanticCoarseBatch,
	cancel context.CancelFunc,
	done chan<- semanticBuildOutput,
) {
	if coarseReady != nil {
		defer close(coarseReady)
	}
	var output semanticBuildOutput
	fail := func(err error) {
		if err != nil && output.err == nil {
			output.err = err
			cancel()
		}
	}
	writer, err := newSemanticRowWriter(dir, coarseReady)
	fail(err)
	for encoded != nil || copied != nil {
		select {
		case batch, ok := <-encoded:
			if !ok {
				encoded = nil
				continue
			}
			if output.err != nil {
				continue
			}
			if batch.err != nil {
				fail(batch.err)
				continue
			}
			// The build cuts each batch at 1 to semanticModelBatchRows rows;
			// the model must give one embedding for each.
			if len(batch.units) != len(batch.embeddings) {
				fail(fmt.Errorf("semantic model batch has mismatched rows"))
				continue
			}
			fail(writer.writeEmbedded(batch.units, batch.embeddings))
		case rows, ok := <-copied:
			if !ok {
				copied = nil
				continue
			}
			if output.err == nil {
				fail(writer.writeCopied(rows))
			}
		}
	}
	if writer != nil {
		var err error
		output.rows, output.vectorsArtifact, output.coarseArtifact, err = writer.finish(output.err == nil)
		fail(err)
	}
	done <- output
}

// semanticRowWriter places vectors at row offsets and tracks which rows and
// 128-row coarse blocks are complete. It sends each complete block to
// coarseReady, the USearch stream; nil sends none.
type semanticRowWriter struct {
	fine        *os.File
	coarse      *os.File
	coarseReady chan<- semanticCoarseBatch
	seen        []bool
	rows        uint64
	blocks      map[uint64]*semanticCoarseBlock
	buffer      []byte
}

type semanticCoarseBlock struct {
	count  int
	coarse [semanticModelBatchRows]semanticCoarseVectors
}

// newSemanticRowWriter creates the fine and coarse vector files of a new
// generation in dir. It fails when either file exists.
func newSemanticRowWriter(dir string, coarseReady chan<- semanticCoarseBatch) (*semanticRowWriter, error) {
	fine, err := os.OpenFile(filepath.Join(dir, semanticVectorsFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	coarse, err := os.OpenFile(filepath.Join(dir, semanticCoarseFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = fine.Close()
		return nil, err
	}
	return &semanticRowWriter{
		fine: fine, coarse: coarse, coarseReady: coarseReady, blocks: make(map[uint64]*semanticCoarseBlock),
	}, nil
}

// writeEmbedded encodes and writes the vectors that the model gave units.
func (w *semanticRowWriter) writeEmbedded(units []semanticUnit, embeddings []semanticUnitEmbedding) error {
	return w.writeRuns(len(units), func(i int) uint64 { return units[i].row }, func(i int, fine, coarse []byte) (semanticCoarseSNORM16Vectors, error) {
		err := encodeSemanticFineVectors(fine, &embeddings[i].fine, units[i].row)
		if err != nil {
			return semanticCoarseSNORM16Vectors{}, errors.Join(err, fmt.Errorf("semantic row %d fine vectors", units[i].row))
		}
		var stored semanticCoarseSNORM16Vectors
		for centroid := range embeddings[i].coarse {
			if stored[centroid], err = encodeSemanticSNORM16Vector(&embeddings[i].coarse[centroid]); err != nil {
				return stored, fmt.Errorf("semantic row %d coarse centroid %d: %w", units[i].row, centroid, err)
			}
		}
		putSemanticCoarse(coarse, &stored)
		return stored, nil
	})
}

// writeCopied writes the stored vectors that each copied row refers to. The
// reuse index made each reference only for a row in range with valid codes
// (semanticRowVectorsValid, semanticVectorsValid).
func (w *semanticRowWriter) writeCopied(rows []semanticCopiedRow) error {
	return w.writeRuns(len(rows), func(i int) uint64 { return rows[i].row }, func(i int, fine, coarse []byte) (semanticCoarseSNORM16Vectors, error) {
		// The mapped rows and store records are little-endian bytes
		// (mapSemanticRows): copy them as they are.
		sourceFine, sourceCoarse := rows[i].ref.vectors()
		copy(fine, unsafe.Slice((*byte)(unsafe.Pointer(sourceFine)), semanticFineVectorBytesPerUnit))
		stored := *sourceCoarse
		copy(coarse, unsafe.Slice((*byte)(unsafe.Pointer(&stored)), semanticCoarseVectorBytesPerUnit))
		return stored, nil
	})
}

// writeRuns encodes count rows and writes each run of consecutive rows with
// one write per file.
func (w *semanticRowWriter) writeRuns(
	count int,
	rowOf func(int) uint64,
	encode func(i int, fine, coarse []byte) (semanticCoarseSNORM16Vectors, error),
) error {
	const fineBytes, coarseBytes = semanticFineVectorBytesPerUnit, semanticCoarseVectorBytesPerUnit
	if need := count * (fineBytes + coarseBytes); cap(w.buffer) < need {
		w.buffer = make([]byte, need)
	}
	fine := w.buffer[:count*fineBytes]
	coarse := w.buffer[count*fineBytes : count*(fineBytes+coarseBytes)]
	stored := make([]semanticCoarseSNORM16Vectors, count)
	for i := range count {
		var err error
		if stored[i], err = encode(i, fine[i*fineBytes:(i+1)*fineBytes], coarse[i*coarseBytes:(i+1)*coarseBytes]); err != nil {
			return err
		}
	}
	for first := 0; first < count; {
		last := first + 1
		for last < count && rowOf(last) == rowOf(last-1)+1 {
			last++
		}
		row := rowOf(first)
		if err := writeSemanticAt(w.fine, fine[first*fineBytes:last*fineBytes], row*fineBytes); err != nil {
			return err
		}
		if err := writeSemanticAt(w.coarse, coarse[first*coarseBytes:last*coarseBytes], row*coarseBytes); err != nil {
			return err
		}
		first = last
	}
	for i := range count {
		if err := w.complete(rowOf(i), &stored[i]); err != nil {
			return err
		}
	}
	return nil
}

func writeSemanticAt(file *os.File, data []byte, offset uint64) error {
	written, err := file.WriteAt(data, int64(offset))
	if err == nil && written != len(data) {
		err = fmt.Errorf("semantic vector write was short")
	}
	return err
}

func putSemanticCoarse(dst []byte, stored *semanticCoarseSNORM16Vectors) {
	at := 0
	for centroid := range stored {
		for dimension := range stored[centroid] {
			binary.LittleEndian.PutUint16(dst[at:at+2], uint16(stored[centroid][dimension]))
			at += 2
		}
	}
}

// complete records one written row and sends its 128-row block to the
// USearch stream when the block is full.
func (w *semanticRowWriter) complete(row uint64, stored *semanticCoarseSNORM16Vectors) error {
	for uint64(len(w.seen)) <= row {
		w.seen = append(w.seen, false)
	}
	if w.seen[row] {
		return fmt.Errorf("semantic row %d was written twice", row)
	}
	w.seen[row] = true
	w.rows++
	sequence := row / semanticModelBatchRows
	block := w.blocks[sequence]
	if block == nil {
		block = &semanticCoarseBlock{}
		w.blocks[sequence] = block
	}
	for centroid := range stored {
		block.coarse[row%semanticModelBatchRows][centroid] = decodeSemanticSNORM16Vector(stored[centroid])
	}
	block.count++
	if block.count == semanticModelBatchRows {
		delete(w.blocks, sequence)
		if w.coarseReady != nil {
			w.coarseReady <- semanticCoarseBatch{sequence: int(sequence), coarse: block.coarse[:]}
		}
	}
	return nil
}

// finish checks that the writer wrote every row from 0, sends the last short
// block, sizes both files, and closes them.
func (w *semanticRowWriter) finish(ok bool) (uint64, semanticSizedArtifact, semanticSizedArtifact, error) {
	var err error
	if ok {
		if uint64(len(w.seen)) != w.rows {
			err = fmt.Errorf("semantic build wrote %d of %d rows", w.rows, len(w.seen))
		}
		// When the writer wrote every row from 0 once, it sent every full
		// block. Only the last block can remain, and it is short.
		for sequence, block := range w.blocks {
			if err == nil && w.coarseReady != nil {
				w.coarseReady <- semanticCoarseBatch{sequence: int(sequence), coarse: block.coarse[:block.count]}
			}
		}
	}
	fineBytes, fineOK := checkedSemanticVectorBytes(w.rows)
	coarseBytes, coarseOK := checkedSemanticCoarseBytes(w.rows)
	if ok && err == nil && (!fineOK || !coarseOK) {
		err = fmt.Errorf("semantic vector size is invalid")
	}
	if ok && err == nil {
		err = errors.Join(w.fine.Truncate(int64(fineBytes)), w.coarse.Truncate(int64(coarseBytes)))
	}
	err = errors.Join(err, w.fine.Close(), w.coarse.Close())
	return w.rows, semanticSizedArtifact{Bytes: fineBytes}, semanticSizedArtifact{Bytes: coarseBytes}, err
}

func releaseSemanticBuildBatch(batch semanticBuildBatch) {
	for _, owner := range batch.owners {
		owner.done()
	}
}

// buildSemanticOverlayGeneration embeds the work-tree bytes of a Git node's
// dirty files. It reads each file once and keys its windows by the bytes that
// it read. It copies a window that a live generation or the unit store holds
// and does not embed it again. Deleted files have no rows; the read masks the
// base rows of every dirty path.
func buildSemanticOverlayGeneration(
	ctx context.Context,
	repoDir string,
	indexDir string,
	lineage string,
	dir string,
	source string,
	files []string,
	embedder semanticModel,
	resources searchResources,
	base string,
) error {
	// base is the staged base generation of the same build, or "": the
	// overlay reuses its windows, so a modified file embeds only its changed
	// windows.
	reuse := loadSemanticReuse(indexDir, lineage, "", nil, base)
	defer reuse.Close()
	return buildSemanticGeneration(
		ctx,
		dir,
		source,
		embedder,
		resources,
		folderReadSource(func(readCtx context.Context, cancel context.CancelCauseFunc) <-chan fileContent {
			return streamDirtyFiles(readCtx, repoDir, files, resources.cpuLimit(), &dirtyFailure{stop: cancel})
		}),
		reuse,
	)
}
