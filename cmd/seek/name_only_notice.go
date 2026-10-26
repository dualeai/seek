package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"

	"github.com/sourcegraph/zoekt/query"
)

// The name-only notice is one line per search. It gives the count of the
// name-only files that could have matched. These are binary files and text
// files too large for the shard format. They are also files with no bytes in
// Seek: cloud-only files, missing Git blobs, and unreadable files. Zoekt stores
// each of them with its NOT-INDEXED marker as content, so one marker query per
// node counts them. Zoekt's own too-small files (1 or 2 bytes) are not counted.

// countedSkipReasons are the marker explanations that the notice counts, as
// Zoekt writes them (index/document.go; TestCountedSkipReasonsMatchZoekt).
var countedSkipReasons = []string{
	"exceeds the maximum size limit",
	"contains binary content",
	"contains too many trigrams", // seekNoBytesReason
}

type nameOnlyCountKey struct{}

// withNameOnlyCount returns ctx with a new command count of the name-only
// files that could match, and that count.
func withNameOnlyCount(ctx context.Context) (context.Context, *atomic.Int64) {
	count := new(atomic.Int64)
	return context.WithValue(ctx, nameOnlyCountKey{}, count), count
}

// addNameOnlyFiles adds a node's count once its read succeeded, so a read
// that openNode repeats counts once.
func addNameOnlyFiles(ctx context.Context, files int64) {
	if count, ok := ctx.Value(nameOnlyCountKey{}).(*atomic.Int64); ok {
		count.Add(files)
	}
}

// nameOnlyFilter returns the filter subtree of q, and whether q has a content
// atom. The atoms that content cannot change (file names, languages, sets)
// keep their meaning. A content atom takes the value that lets a file match:
// true where the query asks for it, and false under a negation. Content that
// is not indexed could hold it or lack it.
func nameOnlyFilter(q query.Q) (query.Q, bool) {
	content := false
	var walk func(q query.Q, positive bool) query.Q
	walk = func(q query.Q, positive bool) query.Q {
		switch typed := q.(type) {
		case *query.And:
			children := make([]query.Q, len(typed.Children))
			for index, child := range typed.Children {
				children[index] = walk(child, positive)
			}
			return query.NewAnd(children...)
		case *query.Or:
			children := make([]query.Q, len(typed.Children))
			for index, child := range typed.Children {
				children[index] = walk(child, positive)
			}
			return query.NewOr(children...)
		case *query.Not:
			return &query.Not{Child: walk(typed.Child, !positive)}
		case *query.Type:
			return &query.Type{Type: typed.Type, Child: walk(typed.Child, positive)}
		case *query.Substring:
			if typed.FileName && !typed.Content {
				return typed
			}
		case *query.Regexp:
			if typed.FileName && !typed.Content {
				return typed
			}
		case *query.Symbol:
		default:
			return q
		}
		content = true
		return &query.Const{Value: positive}
	}
	return query.Simplify(walk(q, true)), content
}

// countNameOnlyFiles counts the node's name-only files that pass the filter
// subtree of q and that q does not already match. It searches through read,
// which holds the node's read lock and searches only the current copy of each
// file. Only a complete marker document counts. A failed count loses only the
// notice.
func countNameOnlyFiles(
	ctx context.Context,
	read *nodeRead,
	q query.Q,
) int64 {
	if _, ok := ctx.Value(nameOnlyCountKey{}).(*atomic.Int64); !ok {
		return 0
	}
	filter, content := nameOnlyFilter(q)
	if !content {
		// A file-name query matches name-only files by name.
		return 0
	}
	marker := &query.Substring{Pattern: zoektNotIndexedMarker, Content: true, CaseSensitive: true}
	opts := searchOpts
	opts.NumContextLines = 0
	files, err := read.search(ctx, query.NewAnd(filter, marker, &query.Not{Child: q}), searchConfig{opts: &opts})
	if err != nil {
		slog.Debug("Name-only count failed", "root", read.plan.displayRoot, "error", err)
		return 0
	}
	var found int64
	for _, file := range files {
		line, ok := onlyMarkerLine(file)
		if ok && slices.Contains(countedSkipReasons, string(bytes.TrimPrefix(line, []byte(zoektNotIndexedMarker)))) {
			found++
		}
	}
	return found
}

// nameOnlyNoticeLine is the notice for that many name-only files, after a
// blank line, with words that differ from those of the truncation line.
func nameOnlyNoticeLine(files int64) string {
	if files == 1 {
		return "\n1 name-only file could also match: binary, too large, not on disk, or unreadable\n"
	}
	return fmt.Sprintf("\n%d name-only files could also match: binary, too large, not on disk, or unreadable\n", files)
}
