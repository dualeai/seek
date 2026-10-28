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
    uint64_t row_base;
    uint8_t centroid_shift;
} seek_bitmap_filter_state_t;

typedef struct seek_usearch_hit_t {
    uint64_t key;
    float score;
} seek_usearch_hit_t;

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

// seek_usearch_bitmap_filter reads the bit of a key's generation row: the
// shard key gives the row in the shard, and row_base is the shard's first row.
static int seek_usearch_bitmap_filter(uint64_t key, void* raw_state) {
    seek_bitmap_filter_state_t const* state = (seek_bitmap_filter_state_t const*)raw_state;
    if (state == NULL || state->bits == NULL || state->centroid_shift >= 64) return 0;
    uint64_t local = key >> state->centroid_shift;
    if (state->row_base >= state->rows || local >= state->rows - state->row_base) return 0;
    uint64_t row = state->row_base + local;
    if (row / 8 >= state->bytes) return 0;
    return (state->bits[row / 8] & (uint8_t)(1u << (row & 7u))) != 0;
}

// seek_usearch_search_many searches each query with up to count keys. A NULL
// bits searches every vector; other bits filter keys with the generation row
// bitmap, and row_base is the shard's first row. The cosine metric gives the
// distance 1 - cosine. So each hit score is the cosine of the int8 codes of
// the query and the key.
static void seek_usearch_search_many(
    seek_usearch_index_t index,
    float const* queries,
    size_t query_count,
    size_t dimensions,
    size_t count,
    uint8_t const* bits,
    size_t bytes,
    uint64_t rows,
    uint64_t row_base,
    uint8_t centroid_shift,
    uint64_t* keys,
    float* distances,
    seek_usearch_hit_t* hits,
    size_t* found,
    seek_usearch_error_t* error
) {
    seek_bitmap_filter_state_t state = {bits, bytes, rows, row_base, centroid_shift};
    for (size_t query = 0; query != query_count; ++query) {
        float const* vector = queries + query * dimensions;
        found[query] = bits == NULL
            ? seek_usearch_search_ptr(index, vector, 1, count, keys, distances, error)
            : seek_usearch_filtered_search_ptr(
                index, vector, 1, count, seek_usearch_bitmap_filter, &state, keys, distances, error
            );
        if (*error != NULL) return;
        for (size_t hit = 0; hit != found[query]; ++hit) {
            hits[query * count + hit].key = keys[hit];
            hits[query * count + hit].score = 1.0f - distances[hit];
        }
    }
}
*/
import "C"

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	mathbits "math/bits"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sync/errgroup"
)

const (
	// These values define the stored graph and retrieval-quality contract.
	// semanticUSearchLayout includes all storage-affecting values so a change
	// cannot reuse an incompatible generation.
	//
	// The graphs use USearch's cosine metric on int8 codes. The int8 cast
	// truncates, so cast norms differ by up to 4 %. With the inner product, a
	// key can then be nearer to another key than to itself. Then no search
	// reaches 3.68 % of the keys of a Kubernetes checkout. The cosine divides
	// by both norms: 0.004 % of the keys stay unreachable, and the graphs take
	// the same time to build. USearch 2.26.4 or later is necessary: earlier
	// versions give the distance 0 to a zero int8 dot product.
	semanticUSearchMetricCosine     = 1
	semanticUSearchQuantizationInt8 = 4
	semanticUSearchConnectivity     = 16
	semanticUSearchExpansionAdd     = 128
	semanticUSearchExpansionSearch  = 64
	// Each shard uses one internal USearch thread. The outer Go worker pools
	// run independent shards up to the effective CPU allowance.
	semanticUSearchThreads = 1
	// Each native shard call first requests this many approximate vector keys
	// per query token. A token whose last key still reaches
	// semanticUSearchDeepenSimilarity searches again with twice as many keys.
	// It stops when the shard has no more keys or saturates, so no count cuts
	// the candidates.
	semanticUSearchVectorsPerQuery = 256
	// A token saturates the shard when its last key still reaches
	// semanticUSearchDeepenSimilarity and its next request would pass this
	// many keys. Then the walk of the shard stops, and every allowed row of
	// the shard becomes a candidate for the exact fine scores. The last token
	// of each query window, a special token, is near the first coarse
	// centroid of almost every row. Without this rule, that token walks each
	// graph to all of its keys. On a Kubernetes checkout (20 queries), this
	// rule made the vector search 20 % faster, and 24 % faster with lang:go.
	// The candidates then held every row within the radius that an exact scan
	// finds, also the rows whose keys the graph cannot reach.
	semanticUSearchSaturationKeys = 1_024
	// Revision 3 cuts the shards at file boundaries and gives each shard keys
	// that start at 0 (semantic_usearch_layout.go).
	semanticUSearchLayoutRevision = 3
)

// semanticUSearchRuntime loads the USearch library once and returns its
// version. A load error stays for the whole process.
var semanticUSearchRuntime = sync.OnceValues(func() (string, error) {
	path, err := extractSemanticUSearchRuntime()
	if err != nil {
		return "", err
	}
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	if cErr := C.seek_usearch_open_library(cPath); cErr != nil {
		return "", fmt.Errorf("load USearch runtime: %s", C.GoString(cErr))
	}
	version := C.GoString(C.seek_usearch_version())
	if version == "" {
		return "", fmt.Errorf("USearch runtime returned an empty version")
	}
	return version, nil
})

func ensureSemanticUSearchRuntime() error {
	_, err := semanticUSearchRuntime()
	return err
}

// semanticUSearchCompatibility names the graphs that this binary can read and
// reuse. The name holds the loaded runtime version, the digest of the bundled
// library, and the layout. Two builds of one version can differ, so the
// digest is part of the name.
func semanticUSearchCompatibility() (string, error) {
	version, err := semanticUSearchRuntime()
	if err != nil {
		return "", err
	}
	return "usearch-" + version + "-" + semanticUSearchAssetKey() + "-" + semanticUSearchLayout(), nil
}

// semanticUSearchLayout identifies settings that affect stored index
// compatibility. semanticUSearchCompatibility adds the loaded runtime version.
// The host-sized shard size is not here: the manifest records it, and the
// search reads each shard's rows from the manifest.
func semanticUSearchLayout() string {
	return fmt.Sprintf(
		"metric%d-quant%d-m%d-add%d-search%d-threads%d-c%d-maxrows%d-filecut-v%d",
		semanticUSearchMetricCosine,
		semanticUSearchQuantizationInt8,
		semanticUSearchConnectivity,
		semanticUSearchExpansionAdd,
		semanticUSearchExpansionSearch,
		semanticUSearchThreads,
		semanticCoarseCentroidsPerUnit,
		semanticUSearchMaxShardRows,
		semanticUSearchLayoutRevision,
	)
}

// semanticUSearchAssetKey is the digest of the bundled library (70 to 90 KB
// compressed, so the hash costs about 30 us).
func semanticUSearchAssetKey() string {
	return semanticRuntimeAssetKey(semanticUSearchCompressedRuntime)
}

func extractSemanticUSearchRuntime() (string, error) {
	return installSemanticRuntime("USearch", semanticUSearchCompressedRuntime, semanticUSearchRuntimeFileName)
}

// semanticUSearchShardsBuilt counts the graph shards that builds made, not
// reused; tests read it to see that a build reused a shard.
var semanticUSearchShardsBuilt atomic.Int64

// semanticUSearchBuildResult is the outcome of a graph stream. built counts
// the shards that the stream built, and reused the shards that it took from a
// published generation.
type semanticUSearchBuildResult struct {
	shards []semanticUSearchShard
	built  int
	reused int
	err    error
}

type semanticUSearchBuildJob struct {
	shard      int
	plan       semanticUSearchShardPlan
	embeddings semanticCoarseSet
}

// buildSemanticUSearchStream starts each graph shard as soon as its rows are
// in, while later model batches are still running. plans gives the shards in
// row order (semanticUSearchShardPlanner). input gives the coarse vectors in
// 128-row batches, in any order. The row writer sends each batch once, and
// only the last batch can be short; the stream fails when a batch follows a
// short one. A shard whose ID is in sources reuses that file: the stream
// copies it, or checks it and links it when every shard is ready. A file that
// fails its check gives a new build of the shard. A link changes the ctime of
// the source file, so the live generation would hash the file again at each
// search until the next bind. The late link keeps that time short, and a
// failed build makes no link. Each shard stays single-threaded inside
// USearch; up to one shard per CPU builds at a time.
func buildSemanticUSearchStream(
	ctx context.Context,
	dir string,
	input <-chan semanticCoarseBatch,
	plans <-chan semanticUSearchShardPlan,
	sources semanticUSearchShardSources,
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
			cancel()
		})
	}
	compatibility, err := semanticUSearchCompatibility()
	fail(err)

	shardsByNumber := make(map[int]semanticUSearchShard)
	reuses := make(map[int]semanticUSearchShardReuse)
	var shardsMu sync.Mutex
	built := 0
	var builds errgroup.Group
	builds.SetLimit(resources.cpuLimit())
	build := func(job semanticUSearchBuildJob) {
		if ctx.Err() != nil {
			return
		}
		shard, reuse, err := prepareSemanticUSearchShard(ctx, dir, compatibility, job, sources, resources)
		if err != nil {
			if ctx.Err() == nil {
				fail(err)
			}
			return
		}
		shardsMu.Lock()
		shardsByNumber[job.shard] = shard
		if reuse != nil {
			reuses[job.shard] = *reuse
		} else {
			built++
		}
		shardsMu.Unlock()
	}

	pending := make(map[int][]semanticCoarseVectors)
	nextSequence := 0
	shortBatch := false
	// window holds the contiguous batches from batch windowFirst on, and
	// ready counts the contiguous rows from row 0. queue holds the shards
	// whose rows are not all in yet; planned counts the rows of all shards.
	var window [][]semanticCoarseVectors
	windowFirst := 0
	ready := uint64(0)
	var queue []semanticUSearchShardPlan
	planned := uint64(0)
	shardCount := 0
	advance := func() {
		for ctx.Err() == nil {
			coarse, ok := pending[nextSequence]
			if !ok {
				return
			}
			delete(pending, nextSequence)
			if shortBatch {
				fail(fmt.Errorf("semantic coarse batch %d follows a short batch", nextSequence))
				return
			}
			window = append(window, coarse)
			ready += uint64(len(coarse))
			shortBatch = len(coarse) < semanticModelBatchRows
			nextSequence++
		}
	}
	submit := func() {
		for len(queue) > 0 && ctx.Err() == nil {
			plan := queue[0]
			end := plan.start + plan.rows
			if end > ready {
				return
			}
			first := int(plan.start/semanticModelBatchRows) - windowFirst
			last := int((end-1)/semanticModelBatchRows) - windowFirst
			job := semanticUSearchBuildJob{shard: shardCount, plan: plan, embeddings: semanticCoarseSet{
				batches: slices.Clone(window[first : last+1]),
				offset:  int(plan.start % semanticModelBatchRows),
				rows:    int(plan.rows),
			}}
			builds.Go(func() error {
				build(job)
				return nil
			})
			shardCount++
			queue = queue[1:]
			// No later shard reads the batches that end at or before end.
			drop := int(end/semanticModelBatchRows) - windowFirst
			clear(window[:drop])
			window = window[drop:]
			windowFirst += drop
		}
	}
	for input != nil || plans != nil {
		select {
		case batch, ok := <-input:
			if !ok {
				input = nil
				continue
			}
			if ctx.Err() != nil {
				continue
			}
			pending[batch.sequence] = batch.coarse
			advance()
		case plan, ok := <-plans:
			if !ok {
				plans = nil
				continue
			}
			if ctx.Err() != nil {
				continue
			}
			if plan.start != planned || plan.rows == 0 {
				fail(fmt.Errorf("USearch shard plan starts at row %d, want %d", plan.start, planned))
				continue
			}
			planned += plan.rows
			queue = append(queue, plan)
		}
		submit()
	}
	if ctx.Err() == nil {
		switch {
		case len(pending) != 0:
			fail(fmt.Errorf("semantic coarse batch %d did not complete", nextSequence))
		case len(queue) != 0 || ready != planned:
			fail(fmt.Errorf("USearch shards cover %d rows, the rows file has %d", planned, ready))
		}
	}
	_ = builds.Wait()
	if firstErr == nil && ctx.Err() == nil {
		fail(placeSemanticUSearchShards(dir, reuses, shardsByNumber, resources.cpuLimit()))
	}
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
	result := semanticUSearchBuildResult{err: firstErr, built: built, reused: len(reuses)}
	if firstErr == nil {
		result.shards = make([]semanticUSearchShard, shardCount)
		for shard := range shardCount {
			placed, ok := shardsByNumber[shard]
			if !ok {
				result.err = fmt.Errorf("USearch shard %d did not complete", shard)
				break
			}
			result.shards[shard] = placed
		}
	}
	done <- result
}

// prepareSemanticUSearchShard gives the graph of one planned shard. It takes
// the file of a published shard with the same ID, copied now or verified for a
// later link. Else it builds a new graph in dir. A source that fails its check
// costs only the build.
func prepareSemanticUSearchShard(
	ctx context.Context,
	dir string,
	compatibility string,
	job semanticUSearchBuildJob,
	sources semanticUSearchShardSources,
	resources searchResources,
) (semanticUSearchShard, *semanticUSearchShardReuse, error) {
	name := semanticUSearchShardName(job.shard)
	path := filepath.Join(dir, name)
	shard := semanticUSearchShard{Name: name, Start: job.plan.start, Rows: job.plan.rows}
	shard.ID = semanticUSearchShardID(compatibility, job.plan, job.embeddings)
	if source, ok := sources[shard.ID]; ok {
		reuse := semanticUSearchShardReuse{source: source}
		var err error
		if source.link {
			reuse, err = verifySemanticUSearchShardSource(source)
		} else {
			shard.Check, err = copySemanticUSearchShard(source, path)
		}
		if err == nil {
			shard.semanticArtifact = source.shard.semanticArtifact
			return shard, &reuse, nil
		}
		slog.Debug("USearch shard not reused", "source", source.path, "error", err)
	}
	cpu, err := resources.acquireCPU(ctx, semanticUSearchThreads)
	if err != nil {
		return shard, nil, err
	}
	err = buildSemanticUSearchShard(ctx, path, job.embeddings)
	var artifact semanticArtifact
	if err == nil {
		artifact, err = inspectSemanticArtifact(path)
	}
	cpu.release()
	var info os.FileInfo
	if err == nil {
		info, err = os.Lstat(path)
	}
	if err != nil {
		return shard, nil, err
	}
	semanticUSearchShardsBuilt.Add(1)
	shard.semanticArtifact, shard.Check = artifact, semanticRowsCheckOf(info)
	return shard, nil, nil
}

// placeSemanticUSearchShards links into dir the verified file of each link
// source of reuses; the build copied the others already. It runs on workers
// goroutines and records the check of each new file in shards.
func placeSemanticUSearchShards(
	dir string,
	reuses map[int]semanticUSearchShardReuse,
	shards map[int]semanticUSearchShard,
	workers int,
) error {
	numbers := make(chan int)
	errs := make([]error, max(1, workers))
	var mu sync.Mutex
	var wait sync.WaitGroup
	for worker := range errs {
		wait.Go(func() {
			for number := range numbers {
				mu.Lock()
				shard, reuse := shards[number], reuses[number]
				mu.Unlock()
				check, err := reuse.place(filepath.Join(dir, shard.Name))
				if err != nil {
					errs[worker] = errors.Join(errs[worker], err)
					continue
				}
				shard.Check = check
				mu.Lock()
				shards[number] = shard
				mu.Unlock()
			}
		})
	}
	for number, reuse := range reuses {
		if reuse.source.link {
			numbers <- number
		}
	}
	close(numbers)
	wait.Wait()
	return errors.Join(errs...)
}

// buildSemanticUSearchShard adds the coarse vectors of the rows of one shard
// to one graph and saves it at path. The keys start at 0: row r of the shard
// has the keys semanticUSearchVectorKey(r, centroid). Keys that share one
// vector can be unreachable in the graph, so a search that does not saturate
// the shard may miss them.
// TODO(https://github.com/unum-cloud/USearch/issues/815): drop this note once
// USearch keeps such keys reachable.
func buildSemanticUSearchShard(
	ctx context.Context,
	path string,
	embeddings semanticCoarseSet,
) error {
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
		coarse := embeddings.At(row)
		for centroid := range coarse {
			vector := &coarse[centroid]
			if err := validateNormalizedSemanticVector(vector); err != nil {
				return fmt.Errorf("USearch shard row %d centroid %d: %w", row, centroid, err)
			}
			key := semanticUSearchVectorKey(uint64(row), centroid)
			if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
				C.seek_usearch_add(index, C.uint64_t(key), (*C.float)(unsafe.Pointer(&vector[0])), cErr)
			}); err != nil {
				return err
			}
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

// semanticUSearchQueryShard defines one shard search. filtered is true only
// when the shard must use the bitmap predicate.
type semanticUSearchQueryShard struct {
	index    int
	filtered bool
}

// semanticUSearchQueryPlan can mix native and exact shard work. jobs contains
// only native shard searches. exactRows contains sorted global row IDs for
// shards assigned to direct scoring.
type semanticUSearchQueryPlan struct {
	jobs      []semanticUSearchQueryShard
	exactRows []uint64
}

// semanticUSearchCandidateSet holds a sorted unique row union. usedUSearch
// means that at least one native job ran, including in a mixed plan.
// saturatedShards counts the native jobs that gave all allowed rows of their
// shard. query carries the checked query vectors to final exact scoring.
type semanticUSearchCandidateSet struct {
	rows            []uint64
	query           []semanticVector
	usedUSearch     bool
	saturatedShards int
}

// semanticFilteredExactShardDivisor sets the measured pass boundary of a
// partial shard: a shard with at most one allowed row in this many goes to
// exact scoring.
const semanticFilteredExactShardDivisor = 4

// semanticUSearchOptionalExactShard reports whether a partial shard of rows
// rows, allowed of them selected, goes to exact scoring at the
// semanticFilteredExactShardDivisor boundary.
func semanticUSearchOptionalExactShard(allowed, rows uint64) bool {
	// Add one row of tolerance so integer shard boundaries do not turn a 25%
	// periodic selection into the other route.
	return allowed*semanticFilteredExactShardDivisor <=
		rows+semanticFilteredExactShardDivisor
}

// planSemanticUSearchShards assigns one route to each shard in a partial mask:
//   - no allowed rows: skip the shard;
//   - all rows allowed: use unfiltered native search;
//   - at most 256 allowed coarse keys (64 rows at 4 keys a row): use exact
//     scoring;
//   - at or below the measured 25% pass boundary: use exact scoring;
//   - otherwise: use native search with the bitmap predicate.
//
// When the native request count equals all allowed keys, native search would
// have to return every allowed key to fill the request. Exact scoring avoids
// that sparse graph case. No work budget sends exact shards to native search.
// A native search that saturates its shard also gives every allowed row of
// the shard to the exact scores, after a graph walk. A measurement used a
// Kubernetes checkout with a _test.go filter (18 % of the rows). There, a
// budget of one full shard at 128 query tokens saved no time and found the
// same rows.
func planSemanticUSearchShards(
	ctx context.Context,
	generation *semanticGeneration,
	mask *semanticFilterMask,
) (semanticUSearchQueryPlan, error) {
	if err := ctx.Err(); err != nil {
		return semanticUSearchQueryPlan{}, err
	}
	if generation == nil || len(generation.manifest.USearchFiles) == 0 {
		return semanticUSearchQueryPlan{}, fmt.Errorf("USearch semantic generation is unavailable")
	}
	shards := generation.manifest.USearchFiles
	plan := semanticUSearchQueryPlan{jobs: make([]semanticUSearchQueryShard, 0, len(shards))}
	if mask != nil {
		wantBytes := (generation.manifest.Rows + 7) / 8
		if !mask.partial() || mask.rows != generation.manifest.Rows ||
			uint64(len(mask.bits)) != wantBytes || len(mask.shardAllowed) != len(shards) {
			return semanticUSearchQueryPlan{}, fmt.Errorf("partial semantic filter mask does not match generation")
		}
	}
	var exactShards []int
	var exactCount uint64
	for shardIndex, shard := range shards {
		if err := ctx.Err(); err != nil {
			return semanticUSearchQueryPlan{}, err
		}
		job := semanticUSearchQueryShard{index: shardIndex}
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
		if allowedVectors <= semanticUSearchVectorsPerQuery ||
			semanticUSearchOptionalExactShard(allowed, shard.Rows) {
			exactShards = append(exactShards, shardIndex)
			exactCount += allowed
			continue
		}
		job.filtered = true
		plan.jobs = append(plan.jobs, job)
	}
	if len(exactShards) != 0 {
		plan.exactRows = make([]uint64, 0, exactCount)
		for _, shardIndex := range exactShards {
			shard := shards[shardIndex]
			before := len(plan.exactRows)
			plan.exactRows = mask.appendSelectedRowsInRange(
				plan.exactRows, shard.Start, shard.Start+shard.Rows,
			)
			if uint64(len(plan.exactRows)-before) != mask.shardAllowed[shardIndex] {
				return semanticUSearchQueryPlan{}, fmt.Errorf(
					"semantic filter bitmap count does not match shard %d", shardIndex,
				)
			}
		}
	}
	if len(plan.jobs) == 0 && len(plan.exactRows) == 0 {
		return semanticUSearchQueryPlan{}, fmt.Errorf("semantic filter selected no searchable shard")
	}
	return plan, nil
}

// searchSemanticUSearchCandidatesWithMask executes the shard plan. A nil mask
// sends every shard to unfiltered native search. A partial mask uses the route
// table in planSemanticUSearchShards. The merger takes the row of every vector
// key that a shard returns, and every allowed row of a saturated shard. It
// adds the exact rows and returns one sorted unique row set.
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
	plan, err := planSemanticUSearchShards(ctx, generation, mask)
	if err != nil {
		return semanticUSearchCandidateSet{}, err
	}
	return executeSemanticUSearchPlan(ctx, generation, queryVectors, mask, plan)
}

// executeSemanticUSearchPlan runs up to GOMAXPROCS native jobs of plan at a
// time. It merges their rows with plan.exactRows into one sorted unique row
// set. The first failed job stops the jobs that have not started.
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
	if err := ensureSemanticUSearchRuntime(); err != nil {
		return semanticUSearchCandidateSet{}, err
	}
	// Only the row set reaches the exact scores, so each shard's keys go to a
	// bitmap of the generation's rows. A merge in score order costs time
	// quadratic in the shard count.
	rows := generation.manifest.Rows
	seen := make([]uint64, (rows+63)/64)
	mark := func(row uint64) error {
		if row >= rows {
			return fmt.Errorf("USearch returned unit row %d outside generation", row)
		}
		seen[row/64] |= 1 << (row % 64)
		return nil
	}
	// mu guards seen and saturatedShards: each search marks its rows when it
	// ends.
	var mu sync.Mutex
	saturatedShards := 0
	searches, searchCtx := errgroup.WithContext(ctx)
	searches.SetLimit(runtime.GOMAXPROCS(0))
	for _, job := range plan.jobs {
		if searchCtx.Err() != nil {
			break
		}
		searches.Go(func() error {
			if err := generation.validateUSearchShard(job.index); err != nil {
				return err
			}
			hits, saturated, err := searchSemanticUSearchShardTokens(
				searchCtx,
				generation.usearch[job.index],
				generation.manifest.USearchFiles[job.index],
				queryVectors,
				semanticUSearchVectorsPerQuery,
				mask,
				job.filtered,
			)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			if saturated {
				saturatedShards++
				// An unfiltered job's shard allows all of its rows.
				shard := generation.manifest.USearchFiles[job.index]
				rowMask := mask
				if !job.filtered {
					rowMask = nil
				}
				return markSemanticRowRange(seen, rows, shard.Start, shard.Start+shard.Rows, rowMask)
			}
			for _, tokenHits := range hits {
				for _, hit := range tokenHits {
					row, _ := semanticUSearchUnitAndCentroid(hit.row)
					if err := mark(row); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}
	searchErr := searches.Wait()
	if err := ctx.Err(); err != nil {
		return semanticUSearchCandidateSet{}, err
	}
	if searchErr != nil {
		return semanticUSearchCandidateSet{}, searchErr
	}
	for _, row := range plan.exactRows {
		if err := mark(row); err != nil {
			return semanticUSearchCandidateSet{}, err
		}
	}
	count := 0
	for _, word := range seen {
		count += mathbits.OnesCount64(word)
	}
	ordered := make([]uint64, 0, count)
	for index, word := range seen {
		for word != 0 {
			ordered = append(ordered, uint64(index)*64+uint64(mathbits.TrailingZeros64(word)))
			word &= word - 1
		}
	}
	return semanticUSearchCandidateSet{
		rows: ordered, query: queryVectors, usedUSearch: true, saturatedShards: saturatedShards,
	}, nil
}

// markSemanticRowRange sets the bits of seen for the rows in [start, end)
// that mask allows. A nil mask allows every row. Whole 64-row words copy the
// mask bytes in one step.
func markSemanticRowRange(seen []uint64, rows, start, end uint64, mask *semanticFilterMask) error {
	if start > end || end > rows {
		return fmt.Errorf("USearch shard rows %d-%d are outside generation", start, end)
	}
	if mask != nil && (mask.rows != rows || uint64(len(mask.bits)) != (rows+7)/8) {
		return fmt.Errorf("partial semantic filter mask does not match generation")
	}
	for row := start; row < end; {
		if row%64 == 0 && end-row >= 64 {
			word := ^uint64(0)
			if mask != nil {
				word = binary.LittleEndian.Uint64(mask.bits[row/8:])
			}
			seen[row/64] |= word
			row += 64
			continue
		}
		if mask == nil || mask.allows(row) {
			seen[row/64] |= 1 << (row % 64)
		}
		row++
	}
	return nil
}

// searchSemanticUSearchShardTokens views one graph shard and sends all query
// tokens through one synchronous C batch of up to limit keys each. The batch
// loops over normal USearch calls and cannot stop in its middle. A token whose
// last key still reaches semanticUSearchDeepenSimilarity searches again alone
// with twice the keys, until the shard has no more. Each token keeps only the
// keys within that bound. When such a token's next request would pass
// semanticUSearchSaturationKeys, the search stops and reports a saturated
// shard with no hits. The caller then takes every allowed row of the shard.
// A filtered batch requires a partial global-row bitmap. The shard's keys
// start at 0. Each returned semanticHit.row holds the generation vector key:
// semanticUSearchVectorKey(shard.Start, 0) plus the shard key. The caller
// later converts it to a semantic row. This boundary rejects keys outside the
// shard, filtered-out rows, duplicate keys, and non-finite distances.
func searchSemanticUSearchShardTokens(
	ctx context.Context,
	path string,
	shard semanticUSearchShard,
	queries []semanticVector,
	limit int,
	mask *semanticFilterMask,
	filtered bool,
) ([][]semanticHit, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	index, err := newSemanticUSearchIndex()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = closeSemanticUSearchIndex(index) }()
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	// TODO(https://github.com/unum-cloud/USearch/issues/818): turn off key
	// lookups once the C API can; each view builds a key table that Seek
	// never reads.
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		C.seek_usearch_view(index, cPath, cErr)
	}); err != nil {
		return nil, false, err
	}
	if err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
		C.seek_usearch_threads_search(index, C.size_t(semanticUSearchThreads), cErr)
	}); err != nil {
		return nil, false, err
	}
	vectorCount := int(shard.Rows) * semanticCoarseCentroidsPerUnit
	want := min(limit, vectorCount)
	if want <= 0 {
		return nil, false, fmt.Errorf("USearch shard is empty")
	}
	if unsafe.Sizeof(semanticHit{}) != unsafe.Sizeof(C.seek_usearch_hit_t{}) ||
		unsafe.Offsetof(semanticHit{}.row) != 0 ||
		unsafe.Offsetof(semanticHit{}.score) != 8 {
		return nil, false, fmt.Errorf("USearch batch hit layout is incompatible")
	}
	baseKey := semanticUSearchVectorKey(shard.Start, 0)
	seenKeys := make([]byte, (vectorCount+7)/8)
	allHits := make([][]semanticHit, len(queries))
	// bits stays nil for an unfiltered search.
	var bits *C.uint8_t
	var bitBytes C.size_t
	var bitRows C.uint64_t
	if filtered {
		if mask == nil || len(mask.bits) == 0 {
			return nil, false, fmt.Errorf("filtered USearch call has no bitmap")
		}
		if shard.Start >= mask.rows {
			return nil, false, fmt.Errorf("USearch shard starts at row %d outside the filter", shard.Start)
		}
		bits = (*C.uint8_t)(unsafe.Pointer(&mask.bits[0]))
		bitBytes, bitRows = C.size_t(len(mask.bits)), C.uint64_t(mask.rows)
	}
	// search runs USearch for the tokens of queries with count keys each.
	search := func(queries []semanticVector, count int) ([]C.seek_usearch_hit_t, []C.size_t, error) {
		nativeHits := make([]C.seek_usearch_hit_t, len(queries)*count)
		found := make([]C.size_t, len(queries))
		keys := make([]C.uint64_t, count)
		distances := make([]C.float, count)
		err := callSemanticUSearch(func(cErr *C.seek_usearch_error_t) {
			C.seek_usearch_search_many(
				index,
				(*C.float)(unsafe.Pointer(&queries[0][0])),
				C.size_t(len(queries)),
				C.size_t(semanticEmbeddingDimensions),
				C.size_t(count),
				bits, bitBytes, bitRows, C.uint64_t(shard.Start), C.uint8_t(semanticUSearchCentroidShift),
				&keys[0], &distances[0],
				(*C.seek_usearch_hit_t)(unsafe.Pointer(&nativeHits[0])),
				(*C.size_t)(unsafe.Pointer(&found[0])),
				cErr,
			)
		})
		return nativeHits, found, err
	}
	nativeHits, found, err := search(queries, want)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	for token := range queries {
		count := int(found[token])
		if count > want {
			return nil, false, fmt.Errorf("USearch returned too many vectors")
		}
		tokenHits, tokenCount, tokenWant := nativeHits[token*want:], count, want
		// A token whose last key still reaches the bound searches again with
		// twice the keys, until the shard has no more or saturates.
		for tokenCount == tokenWant && tokenWant < vectorCount &&
			float32(tokenHits[tokenCount-1].score) >= semanticUSearchDeepenSimilarity {
			if 2*tokenWant > semanticUSearchSaturationKeys {
				return nil, true, nil
			}
			tokenWant = min(2*tokenWant, vectorCount)
			deeper, deeperFound, err := search(queries[token:token+1], tokenWant)
			if err != nil {
				return nil, false, err
			}
			tokenHits, tokenCount = deeper, int(deeperFound[0])
			if tokenCount > tokenWant {
				return nil, false, fmt.Errorf("USearch returned too many vectors")
			}
		}
		hits := unsafe.Slice((*semanticHit)(unsafe.Pointer(&tokenHits[0])), tokenCount)
		clear(seenKeys)
		// Only keys within the bound are candidates: the candidate set
		// follows the similarity, not the shard count. The caller reads the
		// set, not its order.
		kept := hits[:0]
		for hit := range hits {
			localKey := hits[hit].row
			score := hits[hit].score
			if localKey >= uint64(vectorCount) {
				return nil, false, fmt.Errorf("USearch returned vector %d outside shard", localKey)
			}
			key := baseKey + localKey
			row, _ := semanticUSearchUnitAndCentroid(key)
			if mask != nil && !mask.allows(row) {
				return nil, false, fmt.Errorf("USearch returned filtered-out unit row %d", row)
			}
			seenByte, seenBit := localKey/8, byte(1<<(localKey&7))
			if seenKeys[seenByte]&seenBit != 0 {
				return nil, false, fmt.Errorf("USearch returned duplicate vector %d", key)
			}
			if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
				return nil, false, fmt.Errorf("USearch returned a non-finite distance")
			}
			seenKeys[seenByte] |= seenBit
			if score >= semanticUSearchDeepenSimilarity {
				kept = append(kept, semanticHit{row: key, score: score})
			}
		}
		allHits[token] = kept
	}
	return allHits, false, nil
}

// semanticUSearchVectorKey gives the key of one coarse centroid of a row. In a
// shard graph, row is the row in the shard; in a search hit, it is the row in
// the generation.
func semanticUSearchVectorKey(row uint64, centroid int) uint64 {
	return row*semanticCoarseCentroidsPerUnit + uint64(centroid)
}

func semanticUSearchUnitAndCentroid(key uint64) (uint64, int) {
	return key / semanticCoarseCentroidsPerUnit, int(key % semanticCoarseCentroidsPerUnit)
}

// semanticUSearchCentroidShift is the key-to-row shift of the C bitmap
// predicate. The coarse-centroid count is a power of two, so key >> shift has
// the same result as division by that count. A test checks that 1 << shift is
// that count.
const semanticUSearchCentroidShift = 2

func newSemanticUSearchIndex() (C.seek_usearch_index_t, error) {
	var cErr C.seek_usearch_error_t
	index := C.seek_usearch_init(
		C.int(semanticUSearchMetricCosine),
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

// semanticUSearchMasked searches the rows that mask allows (nil: every row).
// USearch or a direct exact scan finds the candidates. The exact fine scores
// keep the candidates that reach floor, best first.
func semanticUSearchMasked(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	mask *semanticFilterMask,
	floor float32,
) ([]semanticHit, error) {
	if mask == nil {
		return semanticUSearchExactFallback(ctx, generation, query, floor)
	}
	switch mask.allowed {
	case 0:
		return nil, nil
	case mask.rows:
		return semanticUSearchExactFallback(ctx, generation, query, floor)
	}

	if mask.allowed <= semanticFilteredExactRows {
		rows := mask.selectedRows()
		if uint64(len(rows)) != mask.allowed {
			return nil, fmt.Errorf("semantic filter bitmap count does not match selected rows")
		}
		slog.Debug("Searched semantic index", "backend", "filtered-exact", "rows", len(rows))
		return exactSemanticSortedCandidates(ctx, generation.vectors, query, rows, floor)
	}
	candidates, err := searchSemanticUSearchCandidatesWithMask(ctx, generation, query, mask)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Recovery is exact over the allowed rows.
		slog.Debug("Filtered USearch search failed; using exact vectors", "error", err)
		return exactSemanticSortedCandidates(ctx, generation.vectors, query, mask.selectedRows(), floor)
	}
	backend := "filtered-shard-exact"
	if candidates.usedUSearch {
		backend = "filtered-usearch"
	}
	slog.Debug("Searched semantic index", "backend", backend, "candidates", len(candidates.rows),
		"saturated_shards", candidates.saturatedShards)
	return exactSemanticSortedRows(ctx, generation.vectors, candidates.query, candidates.rows, floor)
}

// semanticUSearchExactFallback uses a full exact scan for a generation of at
// most semanticUSearchVectorsPerQuery rows. For a larger generation it gets
// approximate USearch candidates and then exact-scores their mapped vectors.
// A native error other than cancellation falls back to a full exact scan.
func semanticUSearchExactFallback(
	ctx context.Context,
	generation *semanticGeneration,
	query *semanticQueryEmbedding,
	floor float32,
) ([]semanticHit, error) {
	if len(generation.vectors) <= semanticUSearchVectorsPerQuery {
		slog.Debug("Searched semantic index", "backend", "exact", "rows", len(generation.vectors))
		return exactSemanticSearch(ctx, generation.vectors, query, floor)
	}
	candidates, err := searchSemanticUSearchCandidatesWithMask(ctx, generation, query, nil)
	if err == nil {
		slog.Debug("Searched semantic index", "backend", "usearch", "candidates", len(candidates.rows),
			"saturated_shards", candidates.saturatedShards)
		return exactSemanticSortedRows(
			ctx, generation.vectors, candidates.query, candidates.rows, floor,
		)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Recovery is always exact, with no work limit: a node keeps its
	// semantic hits when its graph fails.
	slog.Debug("USearch semantic search failed; using exact vectors", "error", err)
	return exactSemanticSearch(ctx, generation.vectors, query, floor)
}
