//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

/*
#cgo LDFLAGS: -ldl

#include <dlfcn.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>

// These declarations mirror the public c/usearch.h ABI from the USearch
// library packaged by the maintainer asset workflow. Review the declarations,
// enum values, and loaded symbols against that header for every asset upgrade.
typedef void* seek_usearch_index_t;
typedef char const* seek_usearch_error_t;
typedef float (*seek_usearch_metric_t)(void const*, void const*);

typedef struct seek_usearch_init_options_t {
    int metric_kind;
    seek_usearch_metric_t metric;
    int quantization;
    size_t dimensions;
    size_t connectivity;
    size_t expansion_add;
    size_t expansion_search;
    bool multi;
} seek_usearch_init_options_t;

typedef char const* (*seek_version_fn)(void);
typedef seek_usearch_index_t (*seek_init_fn)(seek_usearch_init_options_t*, seek_usearch_error_t*);
typedef void (*seek_free_fn)(seek_usearch_index_t, seek_usearch_error_t*);
typedef void (*seek_reserve_fn)(seek_usearch_index_t, size_t, seek_usearch_error_t*);
typedef void (*seek_threads_fn)(seek_usearch_index_t, size_t, seek_usearch_error_t*);
typedef void (*seek_add_fn)(seek_usearch_index_t, uint64_t, void const*, int, seek_usearch_error_t*);
typedef size_t (*seek_size_fn)(seek_usearch_index_t, seek_usearch_error_t*);
typedef void (*seek_path_fn)(seek_usearch_index_t, char const*, seek_usearch_error_t*);
typedef size_t (*seek_search_fn)(seek_usearch_index_t, void const*, int, size_t, uint64_t*, float*, seek_usearch_error_t*);

static void* seek_usearch_library;
static seek_version_fn seek_usearch_version_ptr;
static seek_init_fn seek_usearch_init_ptr;
static seek_free_fn seek_usearch_free_ptr;
static seek_reserve_fn seek_usearch_reserve_ptr;
static seek_threads_fn seek_usearch_threads_add_ptr;
static seek_threads_fn seek_usearch_threads_search_ptr;
static seek_add_fn seek_usearch_add_ptr;
static seek_size_fn seek_usearch_size_ptr;
static seek_path_fn seek_usearch_save_ptr;
static seek_path_fn seek_usearch_view_ptr;
static seek_search_fn seek_usearch_search_ptr;

static char const* seek_usearch_open_library(char const* path) {
    if (seek_usearch_library != NULL) return NULL;
    seek_usearch_library = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    if (seek_usearch_library == NULL) return dlerror();

#define SEEK_LOAD(symbol) do { \
    seek_usearch_##symbol##_ptr = (seek_##symbol##_fn)dlsym(seek_usearch_library, "usearch_" #symbol); \
    if (seek_usearch_##symbol##_ptr == NULL) return dlerror(); \
} while (0)

    SEEK_LOAD(version);
    SEEK_LOAD(init);
    SEEK_LOAD(free);
    SEEK_LOAD(reserve);
    seek_usearch_threads_add_ptr = (seek_threads_fn)dlsym(seek_usearch_library, "usearch_change_threads_add");
    if (seek_usearch_threads_add_ptr == NULL) return dlerror();
    seek_usearch_threads_search_ptr = (seek_threads_fn)dlsym(seek_usearch_library, "usearch_change_threads_search");
    if (seek_usearch_threads_search_ptr == NULL) return dlerror();
    SEEK_LOAD(add);
    SEEK_LOAD(size);
    seek_usearch_save_ptr = (seek_path_fn)dlsym(seek_usearch_library, "usearch_save");
    if (seek_usearch_save_ptr == NULL) return dlerror();
    seek_usearch_view_ptr = (seek_path_fn)dlsym(seek_usearch_library, "usearch_view");
    if (seek_usearch_view_ptr == NULL) return dlerror();
    SEEK_LOAD(search);
#undef SEEK_LOAD
    return NULL;
}

static char const* seek_usearch_version(void) {
    return seek_usearch_version_ptr();
}

static seek_usearch_index_t seek_usearch_init(
    int metric_kind,
    int quantization,
    size_t dimensions,
    size_t connectivity,
    size_t expansion_add,
    size_t expansion_search,
    bool multi,
    seek_usearch_error_t* error
) {
    seek_usearch_init_options_t options;
    options.metric_kind = metric_kind;
    options.metric = NULL;
    options.quantization = quantization;
    options.dimensions = dimensions;
    options.connectivity = connectivity;
    options.expansion_add = expansion_add;
    options.expansion_search = expansion_search;
    options.multi = multi;
    return seek_usearch_init_ptr(&options, error);
}

static void seek_usearch_free(seek_usearch_index_t index, seek_usearch_error_t* error) {
    seek_usearch_free_ptr(index, error);
}

static void seek_usearch_reserve(seek_usearch_index_t index, size_t count, seek_usearch_error_t* error) {
    seek_usearch_reserve_ptr(index, count, error);
}

static void seek_usearch_threads_add(seek_usearch_index_t index, size_t count, seek_usearch_error_t* error) {
    seek_usearch_threads_add_ptr(index, count, error);
}

static void seek_usearch_threads_search(seek_usearch_index_t index, size_t count, seek_usearch_error_t* error) {
    seek_usearch_threads_search_ptr(index, count, error);
}

static void seek_usearch_add(seek_usearch_index_t index, uint64_t key, float const* vector, seek_usearch_error_t* error) {
    seek_usearch_add_ptr(index, key, vector, 1, error); // usearch_scalar_f32_k
}

static size_t seek_usearch_size(seek_usearch_index_t index, seek_usearch_error_t* error) {
    return seek_usearch_size_ptr(index, error);
}

static void seek_usearch_save(seek_usearch_index_t index, char const* path, seek_usearch_error_t* error) {
    seek_usearch_save_ptr(index, path, error);
}

static void seek_usearch_view(seek_usearch_index_t index, char const* path, seek_usearch_error_t* error) {
    seek_usearch_view_ptr(index, path, error);
}

static size_t seek_usearch_search(
    seek_usearch_index_t index,
    float const* query,
    size_t count,
    uint64_t* keys,
    float* distances,
    seek_usearch_error_t* error
) {
    return seek_usearch_search_ptr(index, query, 1, count, keys, distances, error);
}
*/
import "C"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"unsafe"
)

const (
	// These values define the stored graph and retrieval-quality contract.
	// semanticUSearchLayout includes all storage-affecting values so a change
	// cannot reuse an incompatible generation.
	semanticUSearchMetricInnerProduct = 2
	semanticUSearchQuantizationInt8   = 4
	semanticUSearchConnectivity       = 16
	semanticUSearchExpansionAdd       = 128
	semanticUSearchExpansionSearch    = 64
	// Each shard uses one internal USearch thread. The outer Go worker pools run
	// independent shards up to the effective CPU allowance.
	semanticUSearchThreads       = 1
	semanticUSearchUnitsPerShard = 4_096
	// Each query token contributes this many approximate vector candidates per
	// shard before unit collapse and exact scoring.
	semanticUSearchVectorsPerQuery = 256
	semanticUSearchLayoutRevision  = 2
)

var (
	semanticUSearchRuntimeOnce sync.Once
	semanticUSearchRuntimeErr  error
	semanticUSearchRuntimeVer  string
)

func ensureSemanticUSearchRuntime() error {
	semanticUSearchRuntimeOnce.Do(func() {
		path, err := extractSemanticUSearchRuntime()
		if err != nil {
			semanticUSearchRuntimeErr = err
			return
		}
		cPath := C.CString(path)
		defer C.free(unsafe.Pointer(cPath))
		if cErr := C.seek_usearch_open_library(cPath); cErr != nil {
			semanticUSearchRuntimeErr = fmt.Errorf("load USearch runtime: %s", C.GoString(cErr))
			return
		}
		semanticUSearchRuntimeVer = C.GoString(C.seek_usearch_version())
		if semanticUSearchRuntimeVer == "" {
			semanticUSearchRuntimeErr = fmt.Errorf("USearch runtime returned an empty version")
		}
	})
	return semanticUSearchRuntimeErr
}

func semanticUSearchCompatibility() (string, error) {
	if err := ensureSemanticUSearchRuntime(); err != nil {
		return "", err
	}
	return "usearch-" + semanticUSearchRuntimeVer + "-" + semanticUSearchLayout(), nil
}

// semanticUSearchLayout identifies settings that affect stored index
// compatibility. semanticUSearchCompatibility adds the loaded runtime version.
func semanticUSearchLayout() string {
	return fmt.Sprintf(
		"metric%d-quant%d-m%d-add%d-search%d-threads%d-c%d-units%d-v%d",
		semanticUSearchMetricInnerProduct,
		semanticUSearchQuantizationInt8,
		semanticUSearchConnectivity,
		semanticUSearchExpansionAdd,
		semanticUSearchExpansionSearch,
		semanticUSearchThreads,
		semanticCoarseCentroidsPerUnit,
		semanticUSearchUnitsPerShard,
		semanticUSearchLayoutRevision,
	)
}

func semanticUSearchAssetKey() string {
	digest := sha256.Sum256(semanticUSearchCompressedRuntime)
	return hex.EncodeToString(digest[:16])
}

func extractSemanticUSearchRuntime() (string, error) {
	runtimeBytes, err := decodeLateOnAsset(semanticUSearchCompressedRuntime)
	if err != nil {
		return "", fmt.Errorf("load USearch runtime: %w", err)
	}
	cacheRoot, err := seekUserCacheRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheRoot, "semantic", "usearch", semanticUSearchAssetKey())
	digest := sha256.Sum256(runtimeBytes)
	wantSHA256 := hex.EncodeToString(digest[:])
	path, _, err := installPrivateAsset(
		dir,
		semanticUSearchRuntimeFileName,
		".usearch-*.tmp",
		func(path string) bool {
			return validFileDigest(path, int64(len(runtimeBytes)), wantSHA256)
		},
		func(writer io.Writer) error {
			_, err := writer.Write(runtimeBytes)
			return err
		},
	)
	return path, err
}

type semanticUSearchBuildResult struct {
	shards []semanticUSearchShard
	err    error
}

type semanticUSearchBuildJob struct {
	shard      int
	embeddings semanticCoarseSet
}

// buildSemanticUSearchStream starts each complete graph shard while later
// model batches are still running. The input can arrive out of order, but each
// shard inserts stable rows in ascending order. Each shard stays single-threaded
// inside USearch; the Go worker pool supplies host-wide parallelism.
func buildSemanticUSearchStream(
	ctx context.Context,
	dir string,
	input <-chan semanticCoarseBatch,
	cancel context.CancelFunc,
	resources searchResources,
	done chan<- semanticUSearchBuildResult,
) {
	var firstErr error
	var errorOnce sync.Once
	fail := func(err error) {
		if err == nil {
			return
		}
		errorOnce.Do(func() {
			firstErr = err
			if cancel != nil {
				cancel()
			}
		})
	}
	if err := ensureSemanticUSearchRuntime(); err != nil {
		fail(err)
	}

	jobs := make(chan semanticUSearchBuildJob)
	shardsByNumber := make(map[int]semanticUSearchShard)
	var shardsMu sync.Mutex
	var workers sync.WaitGroup
	for range resources.cpuLimit() {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					continue
				}
				cpu, admissionErr := resources.acquireCPU(ctx, semanticUSearchThreads)
				if admissionErr != nil {
					continue
				}
				start := job.shard * semanticUSearchUnitsPerShard
				name := semanticUSearchShardName(job.shard)
				path := filepath.Join(dir, name)
				if err := buildSemanticUSearchShard(ctx, path, start, job.embeddings); err != nil {
					cpu.release()
					fail(err)
					continue
				}
				artifact, err := inspectSemanticArtifact(path)
				cpu.release()
				if err != nil {
					fail(err)
					continue
				}
				shard := semanticUSearchShard{
					Name:             name,
					Start:            uint64(start),
					Rows:             uint64(job.embeddings.Len()),
					semanticArtifact: artifact,
				}
				shardsMu.Lock()
				shardsByNumber[job.shard] = shard
				shardsMu.Unlock()
			}
		}()
	}

	maxBatches := (semanticMaxRows + semanticModelBatchRows - 1) / semanticModelBatchRows
	pending := make(map[int][]semanticCoarseVectors)
	seen := make(map[int]struct{})
	nextSequence := 0
	shardCount := 0
	currentRows := 0
	var currentBatches [][]semanticCoarseVectors
	shortBatch := false
	submit := func() {
		if len(currentBatches) == 0 || ctx.Err() != nil {
			return
		}
		embeddings := semanticCoarseSet{batches: currentBatches, rows: currentRows}
		if err := embeddings.validate(); err != nil {
			fail(err)
			return
		}
		select {
		case jobs <- semanticUSearchBuildJob{shard: shardCount, embeddings: embeddings}:
			shardCount++
			currentBatches = nil
			currentRows = 0
			shortBatch = false
		case <-ctx.Done():
		}
	}
	advance := func() {
		for ctx.Err() == nil {
			coarse, ok := pending[nextSequence]
			if !ok {
				return
			}
			delete(pending, nextSequence)
			if len(coarse) == 0 || len(coarse) > semanticModelBatchRows {
				fail(fmt.Errorf("semantic coarse batch %d has %d rows", nextSequence, len(coarse)))
				return
			}
			if shortBatch {
				fail(fmt.Errorf("semantic coarse batch %d follows a short batch", nextSequence))
				return
			}
			currentBatches = append(currentBatches, coarse)
			currentRows += len(coarse)
			shortBatch = len(coarse) < semanticModelBatchRows
			nextSequence++
			if len(currentBatches) == semanticUSearchUnitsPerShard/semanticModelBatchRows {
				if shortBatch {
					return
				}
				submit()
			}
		}
	}
	for batch := range input {
		if ctx.Err() != nil {
			continue
		}
		if batch.sequence < 0 || batch.sequence >= maxBatches {
			fail(fmt.Errorf("semantic coarse batch %d exceeds limit", batch.sequence))
			continue
		}
		if _, exists := seen[batch.sequence]; exists {
			fail(fmt.Errorf("semantic coarse batch %d was repeated", batch.sequence))
			continue
		}
		seen[batch.sequence] = struct{}{}
		pending[batch.sequence] = batch.coarse
		advance()
	}
	if ctx.Err() == nil {
		advance()
		if len(pending) != 0 {
			fail(fmt.Errorf("semantic coarse batch %d did not complete", nextSequence))
		} else {
			submit()
		}
	}
	close(jobs)
	workers.Wait()
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
	result := semanticUSearchBuildResult{err: firstErr}
	if firstErr == nil {
		result.shards = make([]semanticUSearchShard, shardCount)
		for shard := range shardCount {
			built, ok := shardsByNumber[shard]
			if !ok {
				result.err = fmt.Errorf("USearch shard %d did not complete", shard)
				break
			}
			result.shards[shard] = built
		}
	}
	done <- result
}

func buildSemanticUSearchShard(
	ctx context.Context,
	path string,
	rowBase int,
	embeddings semanticCoarseSet,
) error {
	if rowBase < 0 {
		return fmt.Errorf("USearch row base %d is invalid", rowBase)
	}
	if err := embeddings.validate(); err != nil {
		return err
	}
	index, err := newSemanticUSearchIndex()
	if err != nil {
		return err
	}
	defer func() { _ = closeSemanticUSearchIndex(index) }()
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		C.seek_usearch_threads_add(index, C.size_t(semanticUSearchThreads), cErr)
	}); err != nil {
		return err
	}
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		C.seek_usearch_threads_search(index, C.size_t(semanticUSearchThreads), cErr)
	}); err != nil {
		return err
	}
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		C.seek_usearch_reserve(index, C.size_t(embeddings.Len()*semanticCoarseCentroidsPerUnit), cErr)
	}); err != nil {
		return err
	}
	for row := 0; row < embeddings.Len(); row++ {
		if row&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		coarse, ok := embeddings.At(row)
		if !ok {
			return fmt.Errorf("semantic embedding row %d is missing", row)
		}
		unitRow := uint64(rowBase + row)
		for centroid := range coarse {
			vector := &coarse[centroid]
			if err := validateNormalizedSemanticVector(*vector); err != nil {
				return fmt.Errorf("USearch row %d centroid %d: %w", unitRow, centroid, err)
			}
			key := semanticUSearchVectorKey(unitRow, centroid)
			if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
				C.seek_usearch_add(index, C.uint64_t(key), (*C.float)(unsafe.Pointer(&vector[0])), cErr)
			}); err != nil {
				return err
			}
			runtime.KeepAlive(vector)
		}
	}
	var size C.size_t
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		size = C.seek_usearch_size(index, cErr)
	}); err != nil {
		return err
	}
	wantSize := uint64(embeddings.Len() * semanticCoarseCentroidsPerUnit)
	if uint64(size) != wantSize {
		return fmt.Errorf("USearch shard has %d vectors, want %d", uint64(size), wantSize)
	}
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		C.seek_usearch_save(index, cPath, cErr)
	}); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return nil
}

// searchSemanticUSearchCandidates searches each query token in every shard in
// parallel, validates returned vector keys, and returns the union of unit rows.
// It does not compute the final LateOn MaxSim score.
func searchSemanticUSearchCandidates(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
) ([]uint64, error) {
	if generation == nil || len(generation.manifest.USearchFiles) == 0 ||
		len(generation.usearch) != len(generation.manifest.USearchFiles) {
		return nil, fmt.Errorf("USearch semantic generation is unavailable")
	}
	if generation.usearchErr != nil {
		return nil, generation.usearchErr
	}
	if err := ensureSemanticUSearchRuntime(); err != nil {
		return nil, err
	}
	queryVectors, err := semanticQueryTokenVectors(query)
	if err != nil {
		return nil, err
	}
	type result struct {
		hits [][]semanticHit
		err  error
	}
	jobs := make(chan int)
	results := make(chan result, len(generation.usearch))
	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := min(runtime.GOMAXPROCS(0), len(generation.usearch))
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for shard := range jobs {
				hits, err := searchSemanticUSearchShardTokens(
					searchCtx,
					generation.usearch[shard],
					generation.manifest.USearchFiles[shard],
					queryVectors,
					semanticUSearchVectorsPerQuery,
				)
				results <- result{hits: hits, err: err}
				if err != nil {
					cancel()
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for shard := range generation.usearch {
			select {
			case jobs <- shard:
			case <-searchCtx.Done():
				return
			}
		}
	}()
	go func() {
		wait.Wait()
		close(results)
	}()
	merged := make([][]semanticHit, len(queryVectors))
	var firstErr error
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		for token, hits := range result.hits {
			merged[token] = append(merged[token], hits...)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if firstErr != nil {
		return nil, firstErr
	}
	rows := make(map[uint64]struct{}, len(queryVectors)*semanticUSearchVectorsPerQuery)
	for _, hits := range merged {
		sortSemanticHits(hits)
		if len(hits) > semanticUSearchVectorsPerQuery {
			hits = hits[:semanticUSearchVectorsPerQuery]
		}
		for _, hit := range hits {
			row, _ := semanticUSearchUnitAndCentroid(hit.row)
			if row >= generation.manifest.Rows {
				return nil, fmt.Errorf("USearch returned unit row %d outside generation", row)
			}
			rows[row] = struct{}{}
		}
	}
	ordered := make([]uint64, 0, len(rows))
	for row := range rows {
		ordered = append(ordered, row)
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	return ordered, nil
}

func searchSemanticUSearchShardTokens(
	ctx context.Context,
	path string,
	shard semanticUSearchShard,
	queries []semanticVector,
	limit int,
) ([][]semanticHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index, err := newSemanticUSearchIndex()
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeSemanticUSearchIndex(index) }()
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		C.seek_usearch_view(index, cPath, cErr)
	}); err != nil {
		return nil, err
	}
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		C.seek_usearch_threads_search(index, C.size_t(semanticUSearchThreads), cErr)
	}); err != nil {
		return nil, err
	}
	vectorCount := int(shard.Rows) * semanticCoarseCentroidsPerUnit
	want := min(limit, vectorCount)
	if want <= 0 {
		return nil, fmt.Errorf("USearch shard is empty")
	}
	keys := make([]C.uint64_t, want)
	distances := make([]C.float, want)
	startKey := semanticUSearchVectorKey(shard.Start, 0)
	endKey := semanticUSearchVectorKey(shard.Start+shard.Rows, 0)
	allHits := make([][]semanticHit, len(queries))
	for token, query := range queries {
		var found C.size_t
		if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
			found = C.seek_usearch_search(index, (*C.float)(unsafe.Pointer(&query[0])), C.size_t(want),
				(*C.uint64_t)(unsafe.Pointer(&keys[0])), (*C.float)(unsafe.Pointer(&distances[0])), cErr)
		}); err != nil {
			return nil, err
		}
		runtime.KeepAlive(query)
		if uint64(found) > uint64(want) {
			return nil, fmt.Errorf("USearch returned too many vectors")
		}
		hits := make([]semanticHit, int(found))
		seen := make(map[uint64]struct{}, int(found))
		for hit := range hits {
			key := uint64(keys[hit])
			distance := float32(distances[hit])
			if key < startKey || key >= endKey {
				return nil, fmt.Errorf("USearch returned vector %d outside shard", key)
			}
			if _, exists := seen[key]; exists {
				return nil, fmt.Errorf("USearch returned duplicate vector %d", key)
			}
			if math.IsNaN(float64(distance)) || math.IsInf(float64(distance), 0) {
				return nil, fmt.Errorf("USearch returned a non-finite distance")
			}
			seen[key] = struct{}{}
			hits[hit] = semanticHit{row: key, score: -distance}
		}
		sortSemanticHits(hits)
		allHits[token] = hits
	}
	return allHits, nil
}

func semanticUSearchVectorKey(row uint64, centroid int) uint64 {
	return row*semanticCoarseCentroidsPerUnit + uint64(centroid)
}

func semanticUSearchUnitAndCentroid(key uint64) (uint64, int) {
	return key / semanticCoarseCentroidsPerUnit, int(key % semanticCoarseCentroidsPerUnit)
}

func newSemanticUSearchIndex() (C.seek_usearch_index_t, error) {
	var cErr C.seek_usearch_error_t
	index := C.seek_usearch_init(
		C.int(semanticUSearchMetricInnerProduct),
		C.int(semanticUSearchQuantizationInt8),
		C.size_t(semanticEmbeddingDimensions),
		C.size_t(semanticUSearchConnectivity),
		C.size_t(semanticUSearchExpansionAdd),
		C.size_t(semanticUSearchExpansionSearch),
		C.bool(false),
		&cErr,
	)
	if cErr != nil {
		return nil, fmt.Errorf("USearch: %s", C.GoString(cErr))
	}
	if index == nil {
		return nil, fmt.Errorf("USearch returned a nil index")
	}
	return index, nil
}

func closeSemanticUSearchIndex(index C.seek_usearch_index_t) error {
	if index == nil {
		return nil
	}
	return callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		C.seek_usearch_free(index, cErr)
	})
}

func callSemanticUSearch(call func(*C.seek_usearch_error_t)) error {
	var cErr C.seek_usearch_error_t
	call(&cErr)
	if cErr != nil {
		return fmt.Errorf("USearch: %s", C.GoString(cErr))
	}
	return nil
}

// semanticUSearchExactFallback uses a full exact scan for a small generation.
// For a larger generation it gets approximate USearch candidates and then
// exact-scores their mapped vectors. Any non-cancellation USearch error falls
// back to a full exact scan so damaged approximate data cannot disable semantic
// retrieval.
func semanticUSearchExactFallback(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	limit int,
) ([]semanticHit, error) {
	if generation != nil && len(generation.vectors) <= semanticUSearchVectorsPerQuery {
		slog.Debug("Searched semantic index", "backend", "exact", "rows", len(generation.vectors))
		return exactSemanticSearch(ctx, generation.vectors, query, limit)
	}
	rows, err := searchSemanticUSearchCandidates(ctx, generation, query)
	if err == nil {
		slog.Debug("Searched semantic index", "backend", "usearch", "candidates", len(rows))
		return exactSemanticCandidates(ctx, generation.vectors, query, rows, limit)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	slog.Debug("USearch semantic search failed; using exact vectors", "error", err)
	return exactSemanticSearch(ctx, generation.vectors, query, limit)
}
