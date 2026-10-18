package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/sourcegraph/zoekt/index"
)

// shardFileWrapped reports whether a shard file is larger than
// shardFormatLimit, the most that Zoekt's uint32 offsets can address. Zoekt
// adds every written byte to a uint32 offset with no check
// (zoekt/index/section.go), so such a file holds wrapped offsets and is
// corrupt.
func shardFileWrapped(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > shardFormatLimit
}

// firstBuilderShard returns the number of the first shard that a Builder with
// opts writes. A delta Builder continues after the existing shards
// (index.NewBuilder); a full Builder starts at zero.
func firstBuilderShard(opts index.Options) int {
	if !opts.IsDelta {
		return 0
	}
	return len(opts.FindAllShards())
}

// isolatesShard reports whether a document fills a shard of its own when it is
// added to an empty Builder: Zoekt adds len(Name)+len(Content) to the pending
// size and flushes when that size passes ShardMax (index.Builder.Add). Zoekt
// counts only the name of a document that it skips at Add (a skip reason or a
// DocChecker reason such as a NUL byte), so such a document does not flush and
// is not isolated. Zoekt's SizeMax skip cannot apply: readers never send
// content above documentContentLimit, which is SizeMax, and seek sets no
// LargeFiles.
func isolatesShard(doc index.Document, opts index.Options) bool {
	if doc.SkipReason != index.SkipReasonNone || len(doc.Name)+len(doc.Content) <= opts.ShardMax {
		return false
	}
	var checker index.DocChecker
	return checker.Check(doc.Content, opts.TrigramMax, false) == index.SkipReasonNone
}

// guardWrappedShards keeps the shards of a finished Builder within the Zoekt
// shard format. With documentContentLimit, this is the only size limit seek
// applies to one document: the index cannot address more.
//
// isolated holds the documents that the Builder got first, one shard each, in
// Add order; their shards start at firstShard. later holds the documents added
// after them. When an isolated document's shard wrapped, the function removes
// that shard and every later one, then adds the removed documents again with a
// delta Builder. A wrapped document keeps only its name
// (index.SkipReasonTooLarge), so Zoekt stores its NOT-INDEXED marker and the
// exact-file warning can report it. A shard without an isolated document holds
// at most about twice ShardMax of content, so it cannot wrap, and the function
// does nothing when isolated is empty.
func guardWrappedShards(opts index.Options, firstShard int, isolated, later []index.Document) error {
	if len(isolated) == 0 {
		return nil
	}
	shards := opts.FindAllShards()
	wrapped := make([]bool, len(isolated))
	first := -1
	for i := range isolated {
		n := firstShard + i
		if n < len(shards) && shardFileWrapped(shards[n]) {
			wrapped[i] = true
			if first < 0 {
				first = i
			}
		}
	}
	if first < 0 {
		return nil
	}
	for _, shard := range shards[firstShard+first:] {
		paths, err := index.IndexFilePaths(shard)
		if err != nil {
			return fmt.Errorf("list wrapped shard files: %w", err)
		}
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove wrapped shard file: %w", err)
			}
		}
	}

	opts.IsDelta = true
	builder, err := index.NewBuilder(opts)
	if err != nil {
		return fmt.Errorf("create shard-guard builder: %w", err)
	}
	var addErr error
	add := func(doc index.Document) {
		if addErr == nil {
			addErr = builder.Add(doc)
		}
	}
	for i := first; i < len(isolated); i++ {
		doc := isolated[i]
		if wrapped[i] {
			slog.Debug("Indexing a document by name only: its shard exceeds the Zoekt format", "file", doc.Name)
			doc.Content = nil
			doc.SkipReason = index.SkipReasonTooLarge
		}
		add(doc)
	}
	for _, doc := range later {
		add(doc)
	}
	finishErr := builder.Finish()
	if addErr != nil {
		return fmt.Errorf("add guarded document: %w", addErr)
	}
	return finishErr
}
