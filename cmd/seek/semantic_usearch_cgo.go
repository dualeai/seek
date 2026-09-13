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
typedef int (*seek_filter_fn)(uint64_t, void*);
typedef size_t (*seek_filtered_search_fn)(seek_usearch_index_t, void const*, int, size_t, seek_filter_fn, void*, uint64_t*, float*, seek_usearch_error_t*);

typedef struct seek_bitmap_filter_state_t {
    uint8_t const* bits;
    size_t bytes;
    uint64_t rows;
    uint8_t centroid_shift;
} seek_bitmap_filter_state_t;

typedef struct seek_usearch_hit_t {
    uint64_t key;
    float score;
} seek_usearch_hit_t;

#define SEEK_USEARCH_MAX_RESULTS 256

_Static_assert(sizeof(seek_usearch_hit_t) == 16, "unexpected batch hit size");
_Static_assert(offsetof(seek_usearch_hit_t, key) == 0, "unexpected batch key offset");
_Static_assert(offsetof(seek_usearch_hit_t, score) == 8, "unexpected batch score offset");

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
static seek_filtered_search_fn seek_usearch_filtered_search_ptr;

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
    SEEK_LOAD(filtered_search);
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

static int seek_usearch_bitmap_filter(uint64_t key, void* raw_state) {
    seek_bitmap_filter_state_t const* state = (seek_bitmap_filter_state_t const*)raw_state;
    if (state == NULL || state->bits == NULL || state->centroid_shift >= 64) return 0;
    uint64_t row = key >> state->centroid_shift;
    if (row >= state->rows || row / 8 >= state->bytes) return 0;
    return (state->bits[row / 8] & (uint8_t)(1u << (row & 7u))) != 0;
}

static void seek_usearch_search_many(
    seek_usearch_index_t index,
    float const* queries,
    size_t query_count,
    size_t dimensions,
    size_t count,
    seek_usearch_hit_t* hits,
    size_t* found,
    seek_usearch_error_t* error
) {
    if (count > SEEK_USEARCH_MAX_RESULTS) {
        *error = "USearch batch result limit is too large";
        return;
    }
    uint64_t keys[SEEK_USEARCH_MAX_RESULTS];
    float distances[SEEK_USEARCH_MAX_RESULTS];
    for (size_t query = 0; query != query_count; ++query) {
        found[query] = seek_usearch_search_ptr(
            index,
            queries + query * dimensions,
            1,
            count,
            keys,
            distances,
            error
        );
        if (*error != NULL) return;
        for (size_t hit = 0; hit != found[query]; ++hit) {
            hits[query * count + hit].key = keys[hit];
            hits[query * count + hit].score = -distances[hit];
        }
    }
}

static void seek_usearch_filtered_search_many(
    seek_usearch_index_t index,
    float const* queries,
    size_t query_count,
    size_t dimensions,
    size_t count,
    uint8_t const* bits,
    size_t bytes,
    uint64_t rows,
    uint8_t centroid_shift,
    seek_usearch_hit_t* hits,
    size_t* found,
    seek_usearch_error_t* error
) {
    if (count > SEEK_USEARCH_MAX_RESULTS) {
        *error = "USearch batch result limit is too large";
        return;
    }
    seek_bitmap_filter_state_t state = {bits, bytes, rows, centroid_shift};
    uint64_t keys[SEEK_USEARCH_MAX_RESULTS];
    float distances[SEEK_USEARCH_MAX_RESULTS];
    for (size_t query = 0; query != query_count; ++query) {
        found[query] = seek_usearch_filtered_search_ptr(
            index,
            queries + query * dimensions,
            1,
            count,
            seek_usearch_bitmap_filter,
            &state,
            keys,
            distances,
            error
        );
        if (*error != NULL) return;
        for (size_t hit = 0; hit != found[query]; ++hit) {
            hits[query * count + hit].key = keys[hit];
            hits[query * count + hit].score = -distances[hit];
        }
    }
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
	mathbits "math/bits"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	// Each native shard call requests at most this many approximate vector keys
	// per query token. The merger applies the same global per-token cap before
	// unit collapse and exact scoring.
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
	result, err := searchSemanticUSearchCandidatesWithMask(ctx, generation, query, nil)
	return result.rows, err
}

func searchSemanticUSearchFilteredCandidates(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	mask *semanticFilterMask,
) ([]uint64, error) {
	if mask == nil || mask.mode != semanticFilterPartial {
		return nil, fmt.Errorf("partial semantic filter mask is missing")
	}
	result, err := searchSemanticUSearchCandidatesWithMask(ctx, generation, query, mask)
	return result.rows, err
}

// semanticUSearchQueryShard defines one shard search. limit is the maximum
// number of vector keys returned for each query token, not a semantic row
// count. filtered is true only when the shard must use the bitmap predicate.
type semanticUSearchQueryShard struct {
	index    int
	limit    int
	filtered bool
}

// semanticUSearchQueryPlan can mix native and exact shard work. jobs contains
// only native shard searches. exactRows contains sorted global row IDs for
// shards assigned to direct scoring.
type semanticUSearchQueryPlan struct {
	jobs      []semanticUSearchQueryShard
	exactRows []uint64
}

// semanticUSearchCandidateSet returns a sorted unique row union. usedUSearch
// means that at least one native job ran, including in a mixed plan. query
// carries the checked query vectors to final exact scoring.
type semanticUSearchCandidateSet struct {
	rows        []uint64
	query       []semanticVector
	usedUSearch bool
}

const (
	// Direct filtered scoring and native-error recovery share a work limit equal
	// to one full shard at the model's maximum query length. Counting one extra
	// token covers stored-vector validation, whose cost does not fall with query
	// length.
	semanticExactWorkLimit            = semanticUSearchUnitsPerShard * (lateOnSequenceLength + 1)
	semanticFilteredExactShardDivisor = 4
)

func semanticUSearchOptionalExactShard(allowed, rows uint64) bool {
	// Add one row of tolerance so integer shard boundaries do not turn a 25%
	// periodic selection into the other route.
	return allowed*semanticFilteredExactShardDivisor <=
		rows+semanticFilteredExactShardDivisor
}

func semanticExactWork(rows uint64, queryTokens int) uint64 {
	return rows * uint64(queryTokens+1)
}

// planSemanticUSearchShards assigns one route to each shard in a partial mask:
//   - no allowed rows: skip the shard;
//   - all rows allowed: use unfiltered native search;
//   - at most 256 allowed coarse keys, currently 64 rows: prefer exact scoring;
//   - at or below the measured 25% pass boundary: prefer exact scoring;
//   - otherwise: use native search with the bitmap predicate.
//
// Exact preferences consume one shared row-token work budget in shard order.
// Overflow shards use the native predicate, so exact work cannot grow with the
// generation's shard count. When the native request count equals all allowed
// keys, native search would have to return every allowed key to fill the
// request. Exact scoring avoids that sparse graph case while budget remains.
func planSemanticUSearchShards(
	ctx context.Context,
	generation *semanticGeneration,
	mask *semanticFilterMask,
	queryTokens int,
) (semanticUSearchQueryPlan, error) {
	if err := ctx.Err(); err != nil {
		return semanticUSearchQueryPlan{}, err
	}
	if generation == nil || len(generation.manifest.USearchFiles) == 0 {
		return semanticUSearchQueryPlan{}, fmt.Errorf("USearch semantic generation is unavailable")
	}
	if queryTokens <= 0 || queryTokens > lateOnSequenceLength {
		return semanticUSearchQueryPlan{}, fmt.Errorf("semantic query token count is invalid")
	}
	shards := generation.manifest.USearchFiles
	plan := semanticUSearchQueryPlan{jobs: make([]semanticUSearchQueryShard, 0, len(shards))}
	if mask != nil {
		wantBytes := (generation.manifest.Rows + 7) / 8
		if mask.mode != semanticFilterPartial || mask.rows != generation.manifest.Rows ||
			uint64(len(mask.bits)) != wantBytes || len(mask.shardAllowed) != len(shards) {
			return semanticUSearchQueryPlan{}, fmt.Errorf("partial semantic filter mask does not match generation")
		}
	}
	if mask != nil {
		perRowWork := uint64(queryTokens + 1)
		capacity := min(mask.allowed, uint64(semanticExactWorkLimit)/perRowWork)
		plan.exactRows = make([]uint64, 0, int(capacity))
	}
	remainingExactWork := uint64(semanticExactWorkLimit)
	for shardIndex, shard := range shards {
		if err := ctx.Err(); err != nil {
			return semanticUSearchQueryPlan{}, err
		}
		job := semanticUSearchQueryShard{index: shardIndex, limit: semanticUSearchVectorsPerQuery}
		if mask == nil {
			plan.jobs = append(plan.jobs, job)
			continue
		}
		allowed := mask.shardAllowed[shardIndex]
		if allowed == 0 {
			continue
		}
		if allowed == shard.Rows {
			plan.jobs = append(plan.jobs, job)
			continue
		}
		allowedVectors := allowed * semanticCoarseCentroidsPerUnit
		preferExact := allowedVectors <= semanticUSearchVectorsPerQuery ||
			semanticUSearchOptionalExactShard(allowed, shard.Rows)
		exactWork := semanticExactWork(allowed, queryTokens)
		if preferExact && exactWork <= remainingExactWork {
			before := len(plan.exactRows)
			plan.exactRows = mask.appendSelectedRowsInRange(
				plan.exactRows, shard.Start, shard.Start+shard.Rows,
			)
			if uint64(len(plan.exactRows)-before) != allowed {
				return semanticUSearchQueryPlan{}, fmt.Errorf(
					"semantic filter bitmap count does not match shard %d", shardIndex,
				)
			}
			remainingExactWork -= exactWork
			continue
		}
		job.filtered = true
		job.limit = int(min(uint64(semanticUSearchVectorsPerQuery), allowedVectors))
		plan.jobs = append(plan.jobs, job)
	}
	if len(plan.jobs) == 0 && len(plan.exactRows) == 0 {
		return semanticUSearchQueryPlan{}, fmt.Errorf("semantic filter selected no searchable shard")
	}
	return plan, nil
}

// searchSemanticUSearchCandidatesWithMask executes the shard plan. A nil mask
// sends every shard to unfiltered native search. A partial mask uses the route
// table in planSemanticUSearchShards. The merger keeps the best 256 returned
// vector keys across all shards for each query token before key-to-row collapse,
// then joins the sorted exact rows and returns one sorted unique row set.
func searchSemanticUSearchCandidatesWithMask(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	mask *semanticFilterMask,
) (semanticUSearchCandidateSet, error) {
	queryVectors, err := semanticQueryTokenVectors(query)
	if err != nil {
		return semanticUSearchCandidateSet{}, err
	}
	plan, err := planSemanticUSearchShards(ctx, generation, mask, len(queryVectors))
	if err != nil {
		return semanticUSearchCandidateSet{}, err
	}
	return executeSemanticUSearchPlan(ctx, generation, queryVectors, mask, plan)
}

func executeSemanticUSearchPlan(
	ctx context.Context,
	generation *semanticGeneration,
	queryVectors []semanticVector,
	mask *semanticFilterMask,
	plan semanticUSearchQueryPlan,
) (semanticUSearchCandidateSet, error) {
	if len(plan.jobs) == 0 {
		return semanticUSearchCandidateSet{rows: plan.exactRows, query: queryVectors}, nil
	}
	if len(generation.usearch) != len(generation.manifest.USearchFiles) {
		return semanticUSearchCandidateSet{}, fmt.Errorf("USearch semantic generation is unavailable")
	}
	if generation.usearchErr != nil {
		return semanticUSearchCandidateSet{}, generation.usearchErr
	}
	if err := ensureSemanticUSearchRuntime(); err != nil {
		return semanticUSearchCandidateSet{}, err
	}
	type result struct {
		hits [][]semanticHit
		err  error
	}
	jobs := make(chan semanticUSearchQueryShard)
	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := min(runtime.GOMAXPROCS(0), len(plan.jobs))
	results := make(chan result)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for job := range jobs {
				err := generation.validateUSearchShard(job.index)
				var hits [][]semanticHit
				if err == nil {
					hits, err = searchSemanticUSearchShardTokens(
						searchCtx,
						generation.usearch[job.index],
						generation.manifest.USearchFiles[job.index],
						queryVectors,
						job.limit,
						mask,
						job.filtered,
					)
				}
				results <- result{hits: hits, err: err}
				if err != nil {
					cancel()
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, job := range plan.jobs {
			select {
			case jobs <- job:
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
	mergeBuffers := make([][]semanticHit, len(queryVectors))
	var firstErr error
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		for token, hits := range result.hits {
			if len(merged[token]) == 0 {
				// The worker no longer uses this slice after the receive.
				merged[token] = hits
				continue
			}
			mergeBuffers[token] = mergeSemanticHits(
				mergeBuffers[token][:0],
				merged[token],
				hits,
				semanticUSearchVectorsPerQuery,
			)
			merged[token], mergeBuffers[token] = mergeBuffers[token], merged[token]
		}
	}
	if err := ctx.Err(); err != nil {
		return semanticUSearchCandidateSet{}, err
	}
	if firstErr != nil {
		return semanticUSearchCandidateSet{}, firstErr
	}
	rowCapacity := len(plan.exactRows)
	for _, hits := range merged {
		rowCapacity += len(hits)
	}
	ordered := make([]uint64, 0, rowCapacity)
	ordered = append(ordered, plan.exactRows...)
	for _, hits := range merged {
		for _, hit := range hits {
			row, _ := semanticUSearchUnitAndCentroid(hit.row)
			if row >= generation.manifest.Rows {
				return semanticUSearchCandidateSet{}, fmt.Errorf(
					"USearch returned unit row %d outside generation", row,
				)
			}
			ordered = append(ordered, row)
		}
	}
	slices.Sort(ordered)
	ordered = slices.Compact(ordered)
	return semanticUSearchCandidateSet{
		rows: ordered, query: queryVectors, usedUSearch: true,
	}, nil
}

// mergeSemanticHits merges two ordered hit lists and retains their best limit
// entries. dst must not share its backing array with left or right.
func mergeSemanticHits(dst, left, right []semanticHit, limit int) []semanticHit {
	want := min(limit, len(left)+len(right))
	if want <= 0 {
		return dst[:0]
	}
	if cap(dst) < want {
		dst = make([]semanticHit, 0, want)
	}
	dst = dst[:0]
	for len(dst) < want && len(left) > 0 && len(right) > 0 {
		if semanticHitLess(left[0], right[0]) {
			dst = append(dst, left[0])
			left = left[1:]
		} else {
			dst = append(dst, right[0])
			right = right[1:]
		}
	}
	for len(dst) < want && len(left) > 0 {
		dst = append(dst, left[0])
		left = left[1:]
	}
	for len(dst) < want && len(right) > 0 {
		dst = append(dst, right[0])
		right = right[1:]
	}
	return dst
}

// searchSemanticUSearchShardTokens views one graph shard and sends all query
// tokens through one synchronous C batch. The batch loops over normal USearch
// calls and cannot stop in its middle. A filtered batch requires a partial
// global-row bitmap. In this function, semanticHit.row holds a USearch vector
// key; the caller later converts it to a semantic row. This boundary rejects
// keys outside the shard, filtered-out rows, duplicate keys, and non-finite
// distances.
func searchSemanticUSearchShardTokens(
	ctx context.Context,
	path string,
	shard semanticUSearchShard,
	queries []semanticVector,
	limit int,
	mask *semanticFilterMask,
	filtered bool,
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
	resultSlots := len(queries) * want
	if unsafe.Sizeof(semanticHit{}) != unsafe.Sizeof(C.seek_usearch_hit_t{}) ||
		unsafe.Offsetof(semanticHit{}.row) != 0 ||
		unsafe.Offsetof(semanticHit{}.score) != 8 {
		return nil, fmt.Errorf("USearch batch hit layout is incompatible")
	}
	nativeHits := make([]C.seek_usearch_hit_t, resultSlots)
	found := make([]C.size_t, len(queries))
	startKey := semanticUSearchVectorKey(shard.Start, 0)
	endKey := semanticUSearchVectorKey(shard.Start+shard.Rows, 0)
	seenKeys := make([]byte, (vectorCount+7)/8)
	allHits := make([][]semanticHit, len(queries))
	var centroidShift uint8
	if filtered {
		if mask == nil || len(mask.bits) == 0 {
			return nil, fmt.Errorf("filtered USearch call has no bitmap")
		}
		centroidShift, err = semanticUSearchCentroidShift()
		if err != nil {
			return nil, err
		}
	}
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		if filtered {
			C.seek_usearch_filtered_search_many(
				index,
				(*C.float)(unsafe.Pointer(&queries[0][0])),
				C.size_t(len(queries)),
				C.size_t(semanticEmbeddingDimensions),
				C.size_t(want),
				(*C.uint8_t)(unsafe.Pointer(&mask.bits[0])),
				C.size_t(len(mask.bits)),
				C.uint64_t(mask.rows),
				C.uint8_t(centroidShift),
				(*C.seek_usearch_hit_t)(unsafe.Pointer(&nativeHits[0])),
				(*C.size_t)(unsafe.Pointer(&found[0])),
				cErr,
			)
			return
		}
		C.seek_usearch_search_many(
			index,
			(*C.float)(unsafe.Pointer(&queries[0][0])),
			C.size_t(len(queries)),
			C.size_t(semanticEmbeddingDimensions),
			C.size_t(want),
			(*C.seek_usearch_hit_t)(unsafe.Pointer(&nativeHits[0])),
			(*C.size_t)(unsafe.Pointer(&found[0])),
			cErr,
		)
	}); err != nil {
		return nil, err
	}
	runtime.KeepAlive(queries)
	runtime.KeepAlive(nativeHits)
	runtime.KeepAlive(found)
	if mask != nil {
		runtime.KeepAlive(mask.bits)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for token := range queries {
		count := int(found[token])
		if count > want {
			return nil, fmt.Errorf("USearch returned too many vectors")
		}
		resultStart := token * want
		hits := unsafe.Slice(
			(*semanticHit)(unsafe.Pointer(&nativeHits[resultStart])),
			count,
		)
		clear(seenKeys)
		for hit := range hits {
			key := hits[hit].row
			score := hits[hit].score
			if key < startKey || key >= endKey {
				return nil, fmt.Errorf("USearch returned vector %d outside shard", key)
			}
			row, _ := semanticUSearchUnitAndCentroid(key)
			if mask != nil && !mask.allows(row) {
				return nil, fmt.Errorf("USearch returned filtered-out unit row %d", row)
			}
			localKey := key - startKey
			seenByte, seenBit := localKey/8, byte(1<<(localKey&7))
			if seenKeys[seenByte]&seenBit != 0 {
				return nil, fmt.Errorf("USearch returned duplicate vector %d", key)
			}
			if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
				return nil, fmt.Errorf("USearch returned a non-finite distance")
			}
			seenKeys[seenByte] |= seenBit
			hits[hit] = semanticHit{row: key, score: score}
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

// semanticUSearchCentroidShift returns the key-to-row shift used by the C
// bitmap predicate. The coarse-centroid count must be a power of two so
// key >> shift has the same result as division by that count.
func semanticUSearchCentroidShift() (uint8, error) {
	count := uint(semanticCoarseCentroidsPerUnit)
	if count == 0 || count&(count-1) != 0 {
		return 0, fmt.Errorf("USearch centroid count must be a power of two")
	}
	return uint8(mathbits.TrailingZeros(count)), nil
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

// semanticUSearchFiltered applies file and language filters before final exact
// scoring. No filter or an all-row mask uses the normal semantic route. No
// allowed rows returns no hits. A small partial selection uses direct scoring;
// a larger one can use an exact-only, native-only, or mixed shard plan.
func semanticUSearchFiltered(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	filter *semanticFilterPlan,
	limit int,
) ([]semanticHit, error) {
	if filter == nil {
		return semanticUSearchExactFallback(ctx, generation, query, limit)
	}
	mask, err := buildSemanticFilterMask(ctx, generation, filter)
	if err != nil {
		return nil, err
	}
	switch mask.mode {
	case semanticFilterNone:
		return nil, nil
	case semanticFilterAll:
		return semanticUSearchExactFallback(ctx, generation, query, limit)
	case semanticFilterPartial:
	default:
		return nil, fmt.Errorf("semantic filter mask has an invalid mode")
	}

	var rows []uint64
	if mask.allowed <= semanticFilteredExactRows {
		rows = mask.selectedRows()
		if uint64(len(rows)) != mask.allowed {
			return nil, fmt.Errorf("semantic filter bitmap count does not match selected rows")
		}
		slog.Debug("Searched semantic index", "backend", "filtered-exact", "rows", len(rows))
	} else {
		var candidates semanticUSearchCandidateSet
		candidates, err = searchSemanticUSearchCandidatesWithMask(ctx, generation, query, &mask)
		if err != nil {
			return nil, err
		}
		rows = candidates.rows
		backend := "filtered-shard-exact"
		if candidates.usedUSearch {
			backend = "filtered-usearch"
		}
		slog.Debug("Searched semantic index", "backend", backend, "candidates", len(rows))
		if candidates.query != nil {
			return exactSemanticSortedRows(ctx, generation.vectors, candidates.query, rows, limit)
		}
	}
	return exactSemanticSortedCandidates(ctx, generation.vectors, query, rows, limit)
}

// semanticUSearchExactFallback uses a full exact scan for a small generation.
// For a larger generation it gets approximate USearch candidates and then
// exact-scores their mapped vectors. A non-cancellation native error uses exact
// recovery only while the row-token work stays within the shared exact-work
// budget. Larger failures return an error so the caller can use its normal
// lexical/model fallback without starting a long full scan.
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
	candidates, err := searchSemanticUSearchCandidatesWithMask(ctx, generation, query, nil)
	if err == nil {
		slog.Debug("Searched semantic index", "backend", "usearch", "candidates", len(candidates.rows))
		return exactSemanticSortedRows(
			ctx, generation.vectors, candidates.query, candidates.rows, limit,
		)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if generation == nil {
		return nil, err
	}
	queryTokens, queryErr := semanticQueryTokenCount(query)
	if queryErr != nil {
		return nil, queryErr
	}
	exactWork := semanticExactWork(uint64(len(generation.vectors)), queryTokens)
	if exactWork > uint64(semanticExactWorkLimit) {
		return nil, fmt.Errorf(
			"USearch semantic search failed and exact recovery work %d exceeds limit %d: %w",
			exactWork,
			semanticExactWorkLimit,
			err,
		)
	}
	slog.Debug("USearch semantic search failed; using exact vectors", "error", err)
	return exactSemanticSearch(ctx, generation.vectors, query, limit)
}
