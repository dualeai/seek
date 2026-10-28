package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/sync/errgroup"
)

// nodeRead is one read of a node's published index under the shared read
// lock on cacheDir/.lock. Its first search loads the node's shards, and every
// later search of the read uses that load. The rules of a read:
//
//   - Every open or stat of an index file by path runs under the read lock.
//   - close runs only after every goroutine of the read returns: Close unmaps
//     the shards, and the race detector does not see a use after it.
//   - Every result is a clone (cloneFileMatches), so no result points into a
//     shard after close.
//   - close runs before openNode prepares the node again, and the marked
//     context that openNodeRead returns does not leave the read.
//   - The read of a model query takes the text of its semantic hits from the
//     loaded shards before it closes (readSemanticEvidence).
//   - Every search sees only the current copy of each file (currentCopies).
//
// Only the goroutine that opened the read closes it.
type nodeRead struct {
	plan node
	// dirty holds the dirty files of the node's capture.
	dirty   dirtyFileSet
	lock    *os.File // nil after close
	load    sync.Once
	loaded  nodeShards
	loadErr error
}

// nodeShards is one load of a node's shards.
type nodeShards struct {
	searchers []zoekt.Searcher
	// paths are in the order of searchers; the BM25 rescore reads them.
	paths []string
	// current is the clause that keeps only current copies; nil keeps every
	// document.
	current query.Q
}

// openNodeRead takes the node's shared read lock. dirty holds the dirty files
// of the node's capture (nil for a folder node). It returns the read and ctx
// marked by withReadLockHeld, so a build or publish wait inside the read fails
// as a lock-order defect.
func openNodeRead(ctx context.Context, plan node, dirty dirtyFileSet) (*nodeRead, context.Context, error) {
	lock, err := os.OpenFile(filepath.Join(plan.cacheDir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, ctx, fmt.Errorf("open search lock: %w", err)
	}
	if err := acquireReadLock(ctx, lock); err != nil {
		_ = lock.Close()
		return nil, ctx, fmt.Errorf("acquire search lock: %w", err)
	}
	return &nodeRead{plan: plan, dirty: dirty, lock: lock}, withReadLockHeld(ctx), nil
}

// shards loads the node's shards once per read. The load starts at the first
// search, so a read that returns before any search loads nothing. An index
// with no shard gives an empty load: the search then reports the damage. Each
// load writes one debug line; tests count these lines.
//
// The read holds the read lock, so the shard list cannot interleave with a
// successful swap; the folder delta lists its own build directory.
func (r *nodeRead) shards(ctx context.Context) (nodeShards, error) {
	r.load.Do(func() {
		paths, err := familyShardNames(r.plan.indexDir)
		if err != nil || len(paths) == 0 {
			r.loadErr = err
			return
		}
		searchers, err := loadShardPaths(paths)
		if err != nil {
			r.loadErr = err
			return
		}
		current, err := currentCopies(ctx, searchers, paths, r.dirty)
		if err != nil {
			closeShards(searchers)
			r.loadErr = err
			return
		}
		r.loaded = nodeShards{searchers: searchers, paths: paths, current: current}
		slog.Debug("Loaded the node's shards", "root", r.plan.displayRoot, "shards", len(searchers))
	})
	return r.loaded, r.loadErr
}

// search runs q on the node's shards within the node's scope, on the current
// copy of each file.
func (r *nodeRead) search(ctx context.Context, q query.Q, config searchConfig) ([]zoekt.FileMatch, error) {
	if r.lock == nil {
		return nil, fmt.Errorf("%w: a search of a closed node read", errInvariant)
	}
	shards, err := r.shards(ctx)
	if err != nil {
		return nil, r.damage(fmt.Errorf("load index: %w", err))
	}
	if shards.current != nil {
		q = query.NewAnd(q, shards.current)
	}
	files, err := searchShards(ctx, shards.searchers, shards.paths, q, r.plan.scope, config)
	if err != nil {
		return nil, r.damage(err)
	}
	return files, nil
}

// shardDamage is index damage that a read saw, with the family manifest that
// the read saw under its lock: nil when there was none. The repair removes
// only that manifest (removeDamagedFamilyManifest).
type shardDamage struct {
	err      error
	manifest os.FileInfo
}

func (d *shardDamage) Error() string { return d.err.Error() }
func (d *shardDamage) Unwrap() error { return d.err }

// damage adds the family manifest that the read sees to index damage; it
// returns any other error as it is.
func (r *nodeRead) damage(err error) error {
	if !errors.Is(err, errShardUnloadable) {
		return err
	}
	// os.Stat gives a nil FileInfo with its error: no manifest.
	manifest, _ := os.Stat(filepath.Join(r.plan.indexDir, familyManifestFile))
	return &shardDamage{err: err, manifest: manifest}
}

// close closes the loaded shards, then releases the read lock. A second call
// does nothing.
func (r *nodeRead) close() {
	if r.lock == nil {
		return
	}
	// No search loads the shards after close.
	r.load.Do(func() {})
	closeShards(r.loaded.searchers)
	r.loaded = nodeShards{}
	releaseLock(r.lock)
	r.lock = nil
}

// currentCopies gives the clause that keeps only the current copy of each
// file of a Git node: the uncommitted copy wins. The clause hides the
// committed copy of each dirty file and of each file in the uncommitted
// shards. A stale or peer-built index can hold an uncommitted copy of a file
// that is clean again; that copy is the current one. Zoekt reduces the
// RepoSet per shard, so the clause adds no candidate and no BM25 term. On
// Kubernetes (28 shards), the clause adds no measurable Zoekt time. The
// display then needs no pass over every result to drop stale copies; that
// pass costs about 3 ms for 16,000 files. The result is nil when no file has
// a stale copy, as for a folder node.
func currentCopies(ctx context.Context, searchers []zoekt.Searcher, paths []string, dirty dirtyFileSet) (query.Q, error) {
	var uncommitted []int
	committed := false
	for index, path := range paths {
		if isUncommittedShard(filepath.Base(path)) {
			uncommitted = append(uncommitted, index)
		} else {
			committed = true
		}
	}
	if !committed {
		// No committed copy exists to hide. On a repository with no commit
		// and 40,000 untracked files, the listing took 16 ms of a 255 ms
		// search.
		return nil, nil
	}
	// The uncommitted shards list in parallel. Zoekt skips their tombstoned
	// documents.
	opts := searchOpts
	opts.NumContextLines = 0
	all := &query.Type{Type: query.TypeFileName, Child: &query.Const{Value: true}}
	listed := make([][]zoekt.FileMatch, len(uncommitted))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(runtime.GOMAXPROCS(0))
	for slot, index := range uncommitted {
		group.Go(func() error {
			result, err := zoektSearch(groupCtx, searchers[index], all, &opts)
			if err != nil {
				return fmt.Errorf("list uncommitted files: %w", err)
			}
			listed[slot] = result.Files
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	stale := make(map[string]struct{}, len(dirty))
	for name := range dirty {
		stale[name] = struct{}{}
	}
	for _, files := range listed {
		for _, file := range files {
			stale[file.FileName] = struct{}{}
		}
	}
	if len(stale) == 0 {
		return nil, nil
	}
	return &query.Not{Child: query.NewAnd(
		&query.Not{Child: query.NewRepoSet(repoUncommitted)},
		&query.FileNameSet{Set: stale},
	)}, nil
}
