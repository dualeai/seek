package main

import (
	"path/filepath"
	"slices"

	"github.com/sourcegraph/zoekt"
)

// committedShardHead returns the commit that the published committed shard 0
// records for its HEAD branch.
//
// The shards are the authority on which commit they hold. The .head sidecar
// can disagree with them after a crash between the shard swap and the receipt
// writes, so a freshness check that reads the shard cannot be fooled by a stale
// .head file. Zoekt keeps this record inside the shard for a full build and in
// a ".meta" file beside it for a delta build; its reader prefers the ".meta"
// file, so one call covers both shapes.
//
// ok is false when no committed shard 0 exists, when the shard does not hold
// one live repository with one HEAD branch, or when the metadata cannot be
// read. The caller must then keep the answer it already has. A read failure
// means "unknown", never "stale": this runs outside the publish lock, so a
// peer can replace the file while it is read, and a rebuild on every read
// error would livelock.
func committedShardHead(scan familyScan) (string, bool) {
	path, ok := committedShardZeroPath(scan)
	if !ok {
		return "", false
	}
	repos, err := zoektReadMetadata(path)
	repos = slices.DeleteFunc(repos, func(repo *zoekt.Repository) bool { return repo.Tombstone })
	if err != nil || len(repos) != 1 {
		return "", false
	}
	return recordedHeadVersion(repos[0])
}

// recordedHeadVersion returns the commit a shard's repository record holds for
// its HEAD branch. Seek indexes one branch named HEAD, so any other shape
// belongs to a shard this build did not write, and the caller must not trust
// it. The delta path applies the same rule before it reuses a base.
func recordedHeadVersion(repository *zoekt.Repository) (string, bool) {
	if repository == nil ||
		len(repository.Branches) != 1 ||
		repository.Branches[0].Name != "HEAD" ||
		repository.Branches[0].Version == "" {
		return "", false
	}
	return repository.Branches[0].Version, true
}

// committedShardZeroPath returns the path of the committed family's shard 0.
// Zoekt keeps the repository metadata in every committed shard, and a delta
// build updates the record of each old shard too, so one read of shard 0
// answers the commit question.
func committedShardZeroPath(scan familyScan) (string, bool) {
	for _, m := range scan.members {
		if m.shard && !m.uncommitted && m.numbered && m.num == 0 {
			return filepath.Join(scan.dir, m.name), true
		}
	}
	return "", false
}
