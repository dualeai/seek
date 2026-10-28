//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

// Seek scores vectors with the exact batched dot products of NumKong, the
// kernel library that USearch uses. These kernels use the SIMD instructions of
// the host CPU, so they are several times faster than scalar Go code. The dot
// products of the fine MaxSim use binary16 vectors. The dot products of the
// final model MaxSim use float32 vectors. Seek does not use NumKong's own
// MaxSim kernel. Its int8 screen selects the best centroid of a token
// approximately, and on close centroids it selects a worse one. That score is
// too low, and a row near the radius can be lost.
//
// Seek loads the NumKong release library at run time, as it loads USearch.
// The maintainer asset workflow packages the library of each target. Seek
// installs it in the cache and opens it with dlopen. The release library
// contains the kernels of many CPU families and reads the CPU features itself.
// Seek asks it for the kernels of one family at a time
// (nk_find_kernel_punned), and it uses only these families:
//
//   - serial, on every CPU;
//   - neon and neonfhm on arm64;
//   - haswell and skylake on amd64.
//
// NumKong's own dispatch prefers SME on arm64 CPUs that have it. Seek does not
// use the SME kernels: in NumKong 7.8.4, nk_dots_packed_size_f16_sme enters
// and leaves the streaming mode (smstart, smstop) with no save of d8-d15. The
// mode change clears these registers, but the caller keeps its own values in
// them, so the caller's floating-point values change. Seek does not use the
// AMX kernels: they need a permission from the Linux kernel for each process.
// TODO(https://github.com/ashvardanian/NumKong/issues/416): allow the SME
// family once a NumKong release saves these registers.
//
// NumKong selects the skylake kernels when the CPU has AVX-512F, and the
// haswell kernels when it has AVX2, FMA, and F16C. These kernels also use
// AVX-512BW, AVX-512VL, AVX-512DQ, BMI1, and BMI2. NumKong does not ask the
// operating system whether it saves the YMM and ZMM registers. On some
// CPUs and virtual machines, these kernels can then stop the process with an
// illegal instruction. So Seek also requires each feature of a family from
// golang.org/x/sys/cpu, which includes the operating system check.
// TODO(https://github.com/ashvardanian/NumKong/issues/415): drop this check
// once NumKong checks every feature and the operating system state itself.

/*
#include <dlfcn.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// These declarations mirror the public C ABI of the NumKong release library
// that the maintainer asset workflow packages (include/numkong/types.h and
// include/numkong/capabilities.h of NumKong 7.8.4). Review the types, enum
// values, capability bits, and loaded symbols against those headers for every
// asset upgrade.
typedef uint64_t seek_nk_capability_t;
typedef uint64_t seek_nk_size_t;
typedef void (*seek_nk_kernel_t)(void*);
typedef seek_nk_capability_t (*seek_nk_capabilities_fn)(void);
typedef void (*seek_nk_find_kernel_fn)(int kind, int dtype, seek_nk_capability_t viable,
                                       seek_nk_kernel_t* kernel, seek_nk_capability_t* capability);
typedef seek_nk_size_t (*seek_nk_packed_size_fn)(seek_nk_size_t columns, seek_nk_size_t depth);
typedef void (*seek_nk_pack_fn)(void const* b, seek_nk_size_t columns, seek_nk_size_t depth,
                                seek_nk_size_t b_stride, void* packed);
typedef void (*seek_nk_packed_fn)(void const* a, void const* packed, void* c, seek_nk_size_t rows,
                                  seek_nk_size_t columns, seek_nk_size_t depth, seek_nk_size_t a_stride,
                                  seek_nk_size_t c_stride);
typedef void (*seek_nk_dot_fn)(void const* a, void const* b, seek_nk_size_t count, void* result);

enum {
    seek_nk_kernel_dot = 'i',
    seek_nk_kernel_dots_packed_size = 'P',
    seek_nk_kernel_dots_pack = 'Q',
    seek_nk_kernel_dots_packed = 'G',
    seek_nk_dtype_f32 = 1 << 11,
    seek_nk_dtype_f16 = 1 << 12,
};

#define SEEK_NK_CAP_SERIAL ((seek_nk_capability_t)1)
#define SEEK_NK_CAP_NEON ((seek_nk_capability_t)1 << 1)
#define SEEK_NK_CAP_HASWELL ((seek_nk_capability_t)1 << 2)
#define SEEK_NK_CAP_SKYLAKE ((seek_nk_capability_t)1 << 3)
#define SEEK_NK_CAP_NEONFHM ((seek_nk_capability_t)1 << 6)

enum {
    seek_nk_centroids = 20,
    seek_nk_dimensions = 48,
    seek_nk_row_values = seek_nk_centroids * seek_nk_dimensions,
};

// A backend holds the kernels of one CPU family. viable is the set of
// families that the lookup can return. The binary16 dots kernels must come
// from the family itself, so that the pack and the scores use one layout.
// The other kernels can come from a slower family of the set.
typedef struct {
    char const* name;
    seek_nk_capability_t capability;
    seek_nk_capability_t viable;
    int loaded;
    seek_nk_packed_size_fn f16_packed_size;
    seek_nk_pack_fn f16_pack;
    seek_nk_packed_fn f16_packed;
    seek_nk_dot_fn f16_dot;
    seek_nk_packed_size_fn f32_packed_size;
    seek_nk_pack_fn f32_pack;
    seek_nk_packed_fn f32_packed;
} seek_nk_backend_t;

// The table is in order of speed: a later backend is faster.
static seek_nk_backend_t seek_nk_backends[] = {
    {"serial", SEEK_NK_CAP_SERIAL, SEEK_NK_CAP_SERIAL},
#if defined(__aarch64__)
    {"neon", SEEK_NK_CAP_NEON, SEEK_NK_CAP_SERIAL | SEEK_NK_CAP_NEON},
    {"neonfhm", SEEK_NK_CAP_NEONFHM, SEEK_NK_CAP_SERIAL | SEEK_NK_CAP_NEON | SEEK_NK_CAP_NEONFHM},
#else
    {"haswell", SEEK_NK_CAP_HASWELL, SEEK_NK_CAP_SERIAL | SEEK_NK_CAP_HASWELL},
    {"skylake", SEEK_NK_CAP_SKYLAKE, SEEK_NK_CAP_SERIAL | SEEK_NK_CAP_HASWELL | SEEK_NK_CAP_SKYLAKE},
#endif
};

static void* seek_nk_library;
static seek_nk_capabilities_fn seek_nk_capabilities_ptr;

static int seek_nk_backend_count(void) { return (int)(sizeof(seek_nk_backends) / sizeof(seek_nk_backends[0])); }
static char const* seek_nk_backend_name(int backend) { return seek_nk_backends[backend].name; }
static uint64_t seek_nk_backend_capability(int backend) { return seek_nk_backends[backend].capability; }
static int seek_nk_backend_loaded(int backend) { return seek_nk_backends[backend].loaded; }
static uint64_t seek_nk_host_capabilities(void) { return seek_nk_capabilities_ptr(); }

static seek_nk_kernel_t seek_nk_find(seek_nk_find_kernel_fn find, int kind, int dtype,
                                     seek_nk_capability_t viable, seek_nk_capability_t* used) {
    seek_nk_kernel_t kernel = NULL;
    *used = 0;
    find(kind, dtype, viable, &kernel, used);
    return kernel;
}

static char const* seek_nk_open_library(char const* path) {
    if (seek_nk_library != NULL) return NULL;
    void* library = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    if (library == NULL) return dlerror();
    seek_nk_capabilities_fn capabilities = (seek_nk_capabilities_fn)dlsym(library, "nk_capabilities");
    seek_nk_find_kernel_fn find = (seek_nk_find_kernel_fn)dlsym(library, "nk_find_kernel_punned");
    if (capabilities == NULL || find == NULL) return "the NumKong runtime has no kernel lookup";
    for (int index = 0; index < seek_nk_backend_count(); ++index) {
        seek_nk_backend_t* backend = &seek_nk_backends[index];
        seek_nk_capability_t size_used, pack_used, packed_used, other;
        backend->f16_packed_size = (seek_nk_packed_size_fn)seek_nk_find(
            find, seek_nk_kernel_dots_packed_size, seek_nk_dtype_f16, backend->viable, &size_used);
        backend->f16_pack = (seek_nk_pack_fn)seek_nk_find(
            find, seek_nk_kernel_dots_pack, seek_nk_dtype_f16, backend->viable, &pack_used);
        backend->f16_packed = (seek_nk_packed_fn)seek_nk_find(
            find, seek_nk_kernel_dots_packed, seek_nk_dtype_f16, backend->viable, &packed_used);
        backend->f16_dot = (seek_nk_dot_fn)seek_nk_find(
            find, seek_nk_kernel_dot, seek_nk_dtype_f16, backend->viable, &other);
        backend->f32_packed_size = (seek_nk_packed_size_fn)seek_nk_find(
            find, seek_nk_kernel_dots_packed_size, seek_nk_dtype_f32, backend->viable, &other);
        backend->f32_pack = (seek_nk_pack_fn)seek_nk_find(
            find, seek_nk_kernel_dots_pack, seek_nk_dtype_f32, backend->viable, &other);
        backend->f32_packed = (seek_nk_packed_fn)seek_nk_find(
            find, seek_nk_kernel_dots_packed, seek_nk_dtype_f32, backend->viable, &other);
        backend->loaded = backend->f16_packed_size != NULL && backend->f16_pack != NULL &&
                          backend->f16_packed != NULL && backend->f16_dot != NULL &&
                          backend->f32_packed_size != NULL && backend->f32_pack != NULL &&
                          backend->f32_packed != NULL && size_used == backend->capability &&
                          pack_used == backend->capability && packed_used == backend->capability;
    }
    seek_nk_capabilities_ptr = capabilities;
    seek_nk_library = library;
    return NULL;
}

static size_t seek_nk_f16_query_bytes(int backend, size_t tokens) {
    return (size_t)seek_nk_backends[backend].f16_packed_size(tokens, seek_nk_dimensions);
}

static void seek_nk_f16_query_pack(int backend, uint16_t const* query, size_t tokens, void* packed) {
    seek_nk_backends[backend].f16_pack(query, tokens, seek_nk_dimensions, seek_nk_dimensions * sizeof(uint16_t),
                                       packed);
}

// seek_nk_fine_maxsim_f16 scores count rows of 20 binary16 centroids each.
// Row i is rows[i], or first + i when rows is NULL. The rows in rows must
// increase. The function writes the summed MaxSim of row i to scores[i].
// products holds count * 20 * tokens floats. gathered holds count * 20 * 48
// binary16 values; it can be NULL when rows is NULL. The function returns -1
// when every row is valid. Otherwise it returns the index i of the first row
// with a bad centroid, and it writes the index of that centroid to
// bad_centroid. A bad centroid has a squared norm that is not finite or is
// more than tolerance from 1.
static int64_t seek_nk_fine_maxsim_f16(int backend, uint16_t const* vectors, uint64_t const* rows,
                                       uint64_t first, size_t count, void const* packed_query, size_t tokens,
                                       float tolerance, float* products, uint16_t* gathered, float* scores,
                                       int32_t* bad_centroid) {
    seek_nk_backend_t const* kernel = &seek_nk_backends[backend];
    if (count == 0) return -1;
    // The kernel reads one block of rows. Sorted rows with no gap are already
    // one block. Other rows are copied into gathered.
    uint16_t const* block = vectors + first * seek_nk_row_values;
    if (rows != NULL && rows[count - 1] - rows[0] == count - 1) {
        block = vectors + rows[0] * seek_nk_row_values;
    } else if (rows != NULL) {
        for (size_t row = 0; row < count; ++row)
            memcpy(gathered + row * seek_nk_row_values, vectors + rows[row] * seek_nk_row_values,
                   seek_nk_row_values * sizeof(uint16_t));
        block = gathered;
    }
    // The file has no checksum. A damaged centroid must not give a score, so
    // each centroid must be finite with a squared norm near 1.
    for (size_t row = 0; row < count; ++row) {
        for (int centroid = 0; centroid < seek_nk_centroids; ++centroid) {
            uint16_t const* vector = block + row * seek_nk_row_values + centroid * seek_nk_dimensions;
            float squared_norm;
            kernel->f16_dot(vector, vector, seek_nk_dimensions, &squared_norm);
            float const difference = squared_norm - 1.0f;
            if (!(difference <= tolerance && difference >= -tolerance)) {
                *bad_centroid = centroid;
                return (int64_t)row;
            }
        }
    }
    // products holds one line of token dot products for each centroid.
    kernel->f16_packed(block, packed_query, products, count * seek_nk_centroids, tokens, seek_nk_dimensions,
                       seek_nk_dimensions * sizeof(uint16_t), tokens * sizeof(float));
    for (size_t row = 0; row < count; ++row) {
        float const* lines = products + row * seek_nk_centroids * tokens;
        float score = 0.0f;
        // Sum the token maxima in token order, as a float, so that the score
        // does not depend on the backend after the dot products.
        for (size_t token = 0; token < tokens; ++token) {
            float maximum = lines[token];
            for (int centroid = 1; centroid < seek_nk_centroids; ++centroid) {
                float const dot = lines[(size_t)centroid * tokens + token];
                maximum = dot > maximum ? dot : maximum;
            }
            score += maximum;
        }
        scores[row] = score;
    }
    return -1;
}

static size_t seek_nk_f32_query_bytes(int backend, size_t tokens) {
    return (size_t)seek_nk_backends[backend].f32_packed_size(tokens, seek_nk_dimensions);
}

static void seek_nk_f32_query_pack(int backend, float const* query, size_t tokens, void* packed) {
    seek_nk_backends[backend].f32_pack(query, tokens, seek_nk_dimensions, seek_nk_dimensions * sizeof(float), packed);
}

// seek_nk_dots_f32 reads count vectors of 48 floats. It writes the dot product
// of vector i and query token j to products[i * tokens + j].
static void seek_nk_dots_f32(int backend, float const* vectors, size_t count, void const* packed_query,
                             size_t tokens, double* products) {
    seek_nk_backends[backend].f32_packed(vectors, packed_query, products, count, tokens, seek_nk_dimensions,
                                         seek_nk_dimensions * sizeof(float), tokens * sizeof(double));
}
*/
import "C"

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/cpu"
)

// The capability bits of the NumKong kernel families that Seek uses. They
// mirror nk_cap_*_k of the NumKong headers.
const (
	semanticNumKongCapSerial  uint64 = 1
	semanticNumKongCapNeon    uint64 = 1 << 1
	semanticNumKongCapHaswell uint64 = 1 << 2
	semanticNumKongCapSkylake uint64 = 1 << 3
	semanticNumKongCapNeonFHM uint64 = 1 << 6
)

// semanticNumKongFeatures holds the CPU features that the kernel families
// need, as golang.org/x/sys/cpu reports them.
type semanticNumKongFeatures struct {
	arm64, fhm                            bool
	avx2, fma, f16c, bmi1, bmi2           bool
	avx512f, avx512bw, avx512vl, avx512dq bool
}

func semanticNumKongHostFeatures() semanticNumKongFeatures {
	return semanticNumKongFeatures{
		arm64:    runtime.GOARCH == "arm64",
		fhm:      cpu.ARM64.HasASIMDFHM,
		avx2:     cpu.X86.HasAVX2,
		fma:      cpu.X86.HasFMA,
		f16c:     cpu.X86.HasF16C,
		bmi1:     cpu.X86.HasBMI1,
		bmi2:     cpu.X86.HasBMI2,
		avx512f:  cpu.X86.HasAVX512F,
		avx512bw: cpu.X86.HasAVX512BW,
		avx512vl: cpu.X86.HasAVX512VL,
		avx512dq: cpu.X86.HasAVX512DQ,
	}
}

// semanticNumKongAllowedCapabilities gives the kernel families whose compile
// targets are all in features. golang.org/x/sys/cpu reports AVX2 and
// AVX-512 only when the operating system saves their registers. Every arm64
// CPU that Seek supports has NEON.
func semanticNumKongAllowedCapabilities(features semanticNumKongFeatures) uint64 {
	allowed := semanticNumKongCapSerial
	if features.arm64 {
		allowed |= semanticNumKongCapNeon
		if features.fhm {
			allowed |= semanticNumKongCapNeonFHM
		}
	}
	haswell := features.avx2 && features.fma && features.f16c && features.bmi1 && features.bmi2
	if haswell {
		allowed |= semanticNumKongCapHaswell
		if features.avx512f && features.avx512bw && features.avx512vl && features.avx512dq {
			allowed |= semanticNumKongCapSkylake
		}
	}
	return allowed
}

// semanticNumKongRuntime loads the NumKong library once and gives the kernel
// families that Seek can use on the host, slowest first. The list always
// starts with the serial family. A load error stays for the whole process.
var semanticNumKongRuntime = sync.OnceValues(func() ([]int, error) {
	path, err := extractSemanticNumKongRuntime()
	if err != nil {
		return nil, err
	}
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	if cErr := C.seek_nk_open_library(cPath); cErr != nil {
		return nil, fmt.Errorf("load NumKong runtime: %s", C.GoString(cErr))
	}
	usable := uint64(C.seek_nk_host_capabilities()) &
		semanticNumKongAllowedCapabilities(semanticNumKongHostFeatures())
	var backends []int
	for backend := range int(C.seek_nk_backend_count()) {
		if C.seek_nk_backend_loaded(C.int(backend)) != 0 &&
			usable&uint64(C.seek_nk_backend_capability(C.int(backend))) != 0 {
			backends = append(backends, backend)
		}
	}
	if len(backends) == 0 || backends[0] != 0 {
		return nil, fmt.Errorf("load NumKong runtime: the serial kernels are missing")
	}
	// The family decides the fine-scorer speed: --verbose names it.
	slog.Debug("Selected the NumKong kernel family", "backend", semanticNumKongBackendName(backends[len(backends)-1]))
	return backends, nil
})

// semanticNumKongBackend gives the fastest kernel family that Seek can use on
// the host.
func semanticNumKongBackend() (int, error) {
	backends, err := semanticNumKongRuntime()
	if err != nil {
		return 0, err
	}
	return backends[len(backends)-1], nil
}

func semanticNumKongBackendName(backend int) string {
	return C.GoString(C.seek_nk_backend_name(C.int(backend)))
}

// semanticNumKongAssetKey is the digest of the bundled library.
func semanticNumKongAssetKey() string {
	return semanticRuntimeAssetKey(semanticNumKongCompressedRuntime)
}

// extractSemanticNumKongRuntime installs the bundled NumKong library in the
// cache, as extractSemanticUSearchRuntime installs USearch.
func extractSemanticNumKongRuntime() (string, error) {
	return installSemanticRuntime("NumKong", semanticNumKongCompressedRuntime, semanticNumKongRuntimeFileName)
}

// semanticNumKongPack holds query tokens in the layout of one kernel family.
// The pack is 8-byte aligned and holds no Go pointer, so C can read it.
type semanticNumKongPack struct {
	backend int
	tokens  int
	words   []uint64
}

func (pack *semanticNumKongPack) pointer() unsafe.Pointer {
	return unsafe.Pointer(&pack.words[0])
}

func newSemanticNumKongPack(backend, tokens int, bytes C.size_t) semanticNumKongPack {
	return semanticNumKongPack{backend: backend, tokens: tokens, words: make([]uint64, (int(bytes)+7)/8)}
}

// packSemanticF16Query packs binary16 query tokens of 48 components each for
// a loaded backend. query must hold at least one token.
func packSemanticF16Query(backend int, query []semanticF16Vector) semanticNumKongPack {
	pack := newSemanticNumKongPack(backend, len(query), C.seek_nk_f16_query_bytes(C.int(backend), C.size_t(len(query))))
	C.seek_nk_f16_query_pack(
		C.int(backend),
		(*C.uint16_t)(unsafe.Pointer(&query[0][0])),
		C.size_t(len(query)),
		pack.pointer(),
	)
	return pack
}

// packSemanticF32Query packs float32 query tokens of 48 components each for a
// loaded backend. query must hold at least one token.
func packSemanticF32Query(backend int, query []float32) semanticNumKongPack {
	tokens := len(query) / semanticEmbeddingDimensions
	pack := newSemanticNumKongPack(backend, tokens, C.seek_nk_f32_query_bytes(C.int(backend), C.size_t(tokens)))
	C.seek_nk_f32_query_pack(C.int(backend), (*C.float)(unsafe.Pointer(&query[0])), C.size_t(tokens), pack.pointer())
	return pack
}

// semanticFineScratch holds the buffers of one worker. They hold the dot
// products of one block of rows, the rows of a block with gaps, and the block
// scores. A pool keeps the buffers, so a search does not clear new memory for
// each worker.
type semanticFineScratch struct {
	products []float32
	gathered []semanticFineF16Vectors
	scores   []float32
}

var semanticFineScratchPool = sync.Pool{New: func() any { return new(semanticFineScratch) }}

// getSemanticFineScratch gives buffers for blocks of blockRows rows. gather
// also gives the buffer for rows with gaps. putSemanticFineScratch returns
// the buffers to the pool.
func getSemanticFineScratch(blockRows, tokens int, gather bool) *semanticFineScratch {
	scratch := semanticFineScratchPool.Get().(*semanticFineScratch)
	if need := blockRows * semanticFineCentroidsPerUnit * tokens; cap(scratch.products) < need {
		scratch.products = make([]float32, need)
	}
	if cap(scratch.scores) < blockRows {
		scratch.scores = make([]float32, blockRows)
	}
	if gather && cap(scratch.gathered) < blockRows {
		scratch.gathered = make([]semanticFineF16Vectors, blockRows)
	}
	scratch.products = scratch.products[:cap(scratch.products)]
	scratch.scores = scratch.scores[:cap(scratch.scores)]
	scratch.gathered = scratch.gathered[:cap(scratch.gathered)]
	return scratch
}

func putSemanticFineScratch(scratch *semanticFineScratch) {
	semanticFineScratchPool.Put(scratch)
}

// scoreSemanticFineBlock writes the summed fine MaxSim of count rows to
// scratch.scores. Row i is rows[i], or first+i when rows is nil. The rows must
// increase and must be in vectors. With rows, scratch must hold the buffer for
// rows with gaps (getSemanticFineScratch with gather). The function returns -1
// when every row is valid. Otherwise it returns the index of the first row
// with an invalid centroid, and the index of that centroid.
func scoreSemanticFineBlock(
	vectors []semanticFineF16Vectors,
	rows []uint64,
	first uint64,
	count int,
	query *semanticNumKongPack,
	scratch *semanticFineScratch,
) (int, int) {
	if count == 0 {
		return -1, 0
	}
	var rowIDs *C.uint64_t
	var gathered *C.uint16_t
	if rows != nil {
		rowIDs = (*C.uint64_t)(unsafe.Pointer(&rows[0]))
		gathered = (*C.uint16_t)(unsafe.Pointer(&scratch.gathered[0][0][0]))
	}
	var centroid C.int32_t
	bad := C.seek_nk_fine_maxsim_f16(
		C.int(query.backend),
		(*C.uint16_t)(unsafe.Pointer(&vectors[0][0][0])),
		rowIDs,
		C.uint64_t(first),
		C.size_t(count),
		query.pointer(),
		C.size_t(query.tokens),
		C.float(semanticF16SquaredNormTolerance),
		(*C.float)(unsafe.Pointer(&scratch.products[0])),
		gathered,
		(*C.float)(unsafe.Pointer(&scratch.scores[0])),
		&centroid,
	)
	return int(bad), int(centroid)
}

// semanticNumKongDotsF32 writes the dot product of vector i of vectors and
// query token j to products[i*query.tokens+j]. vectors holds whole vectors of
// 48 components, and products holds one float64 for each pair.
func semanticNumKongDotsF32(vectors []float32, query *semanticNumKongPack, products []float64) {
	count := len(vectors) / semanticEmbeddingDimensions
	if count == 0 {
		return
	}
	C.seek_nk_dots_f32(
		C.int(query.backend),
		(*C.float)(unsafe.Pointer(&vectors[0])),
		C.size_t(count),
		query.pointer(),
		C.size_t(query.tokens),
		(*C.double)(unsafe.Pointer(&products[0])),
	)
}
