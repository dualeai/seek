package main

import (
	"context"
	"errors"
	"math"
)

const (
	// zoektShardFormatBytes is the most content and the largest shard file that
	// Zoekt can address: its offsets are uint32 (zoekt/index/section.go and
	// zoekt/index/shard_builder.go). It is a format limit of the index, not a
	// file-size policy. Seek sets no other per-document size limit: readers send
	// larger content by name only, and guardWrappedShards keeps each shard file
	// within this limit.
	zoektShardFormatBytes = math.MaxUint32

	// maxReadWeightBytes is the largest readSemaphore weight one document
	// takes (readWeight). The smallest read budget, maxInFlightBytes, is sized
	// in these units, not by the document size limit.
	maxReadWeightBytes = 100 * 1024 * 1024 // 100 MiB

	// maxInFlightBytes is the smallest readSemaphore capacity: six
	// maximum-weight documents, room for two build slots (buildSlotLimit).
	// readBudgetBytes grows the capacity with the host's memory.
	maxInFlightBytes = 6 * maxReadWeightBytes

	// minBudgetBuildSlots is buildSlotLimit of the smallest budget.
	minBudgetBuildSlots = (maxInFlightBytes - 2*maxReadWeightBytes) / (2 * maxReadWeightBytes)

	// defaultIndexWindowBytes is the rotation point of the smallest budget
	// when all its build slots run. The compile-time checks below prove the
	// window rules with it; indexWindowFor applies the same formula to the
	// real budget and the builds of a command.
	defaultIndexWindowBytes = (maxInFlightBytes - 2*maxReadWeightBytes) / (2 * minBudgetBuildSlots)
)

// readBudgetBytes is the global readSemaphore capacity for accounted document
// content across all corpus readers and builders; workers do not own separate
// quotas. Readers acquire weight, and consumers release it after the
// Builder.Finish that stops retaining the document. Seek's objective is the
// shortest time, so the budget grows with the host: a quarter of its physical
// memory, and at least maxInFlightBytes. Zoekt needs several times the content
// it builds, so the rest of memory is left for the builds. Other allocations
// are not covered, so this value does not bound process RSS.
var readBudgetBytes = max(int64(maxInFlightBytes), hostPhysicalMemory()/4)

// indexWindowBytes is the document-weight rotation point for
// indexDocumentsWithRepository and the payload limit for native Git delta
// admission when no command set a window (indexWindow). A consumer rotates
// when its pending weight reaches or passes this point, so one document can
// take it past the point. The formula keeps headroom beyond N rotation points:
// N*window + 2*doc is within readBudgetBytes. This is not an exact bound on
// pending weight or process memory. It is a variable so tests can shrink it
// through swapIndexWindowBytesForTest; production callers treat it as
// read-only.
var indexWindowBytes = indexWindowFor(readBudgetBytes, buildSlotLimit(readBudgetBytes))

// buildSlotLimit is the most node builds that run at the same time: the most
// builds whose windows keep the deadlock proof with a window of at least one
// maximum-weight document. The limit protects correctness, not CPU: every
// build's readers and consumers stay within the read budget, so no read
// Acquire waits forever (golang/go#59002). A host with more memory runs more
// builds at once.
func buildSlotLimit(readBudget int64) int {
	return max(1, int((readBudget-2*maxReadWeightBytes)/(2*maxReadWeightBytes)))
}

// indexWindowFor is the rotation point when builds run at once: each build
// holds two windows (the lexical and the semantic consumer), and two
// maximum-weight documents stay free.
func indexWindowFor(readBudget int64, builds int) int64 {
	builds = min(max(builds, 1), buildSlotLimit(readBudget))
	return (readBudget - 2*maxReadWeightBytes) / (2 * int64(builds))
}

type indexWindowKey struct{}

// withIndexWindow sets the window of a command whose graph has nodes nodes,
// before its first build: at most that many builds run, so a search of one
// node gets the whole budget in its window, and a search of many nodes gets
// one window per build slot. Each build takes its two windows from
// buildWindows, so commands that run at the same time share the budget.
func withIndexWindow(ctx context.Context, nodes int) context.Context {
	return context.WithValue(ctx, indexWindowKey{}, indexWindowFor(readBudgetBytes, nodes))
}

// indexWindow is the rotation point of a build under ctx: the window of its
// command, or indexWindowBytes outside a command.
func indexWindow(ctx context.Context) int64 {
	if window, ok := ctx.Value(indexWindowKey{}).(int64); ok {
		return window
	}
	return indexWindowBytes
}

// documentContentLimit is the most content seek gives Zoekt for one document:
// zoektShardFormatBytes, because one shard cannot hold more. Every reader sends
// a larger file by name only (index.SkipReasonTooLarge), so Zoekt stores its
// NOT-INDEXED marker and the name stays searchable. It is a variable so tests
// can swap it; production callers treat it as read-only.
var documentContentLimit int64 = zoektShardFormatBytes

// shardFormatLimit is the largest shard file seek keeps: zoektShardFormatBytes.
// guardWrappedShards treats a larger file as wrapped. It is a variable so tests
// can swap it; production callers treat it as read-only.
var shardFormatLimit int64 = zoektShardFormatBytes

// documentContentFits reports whether a document of size bytes can keep its
// content in the index.
func documentContentFits(size int64) bool {
	return size <= documentContentLimit
}

// These casts underflow at compile time if an invariant is false, for the
// smallest budget with all its build slots. One document window is the minimum
// that keeps indexing useful. A maximum-weight document must fit in the
// in-flight budget to prevent a permanent Acquire wait on a non-cancellable
// context (golang/go#59002). The final check keeps two rotation points per
// slot and two maximum-weight documents within that budget. A larger budget
// or fewer builds give a larger window by the same rule, so the proof holds
// (TestBuildSlotsFromReadBudget).
const (
	_ = uint(minBudgetBuildSlots - 1)
	_ = uint(defaultIndexWindowBytes - maxReadWeightBytes)
	_ = uint(maxInFlightBytes - maxReadWeightBytes)
	_ = uint(maxInFlightBytes - (2*minBudgetBuildSlots*defaultIndexWindowBytes + 2*maxReadWeightBytes))
)

// errDeltaPayloadExceedsWindow fires when a delta (working-tree dirty set OR
// folder-manifest changed set) reaches the build's window (indexWindow). Both
// sites route through a windowed full rebuild via indexDocuments rather than
// holding the whole payload in indexDeltaDocuments's single terminal Finish.
var errDeltaPayloadExceedsWindow = errors.New("delta payload exceeds window threshold")
