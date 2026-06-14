package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

const (
	// uncommittedManifestFileName stores the per-cycle inventory of dirty
	// files that landed in the "uncommitted" Zoekt repo. Each entry pins a
	// path to its working-tree identity so the next cycle can detect
	// add/modify/remove without rebuilding the whole shard set.
	uncommittedManifestFileName = ".uncommitted-manifest-v1"
	uncommittedManifestVersion  = 1
	// maxUncommittedDeltaShards caps stacked delta shards before falling
	// back to a full rebuild. Lower than the committed cap because the
	// uncommitted set churns one file per save in editors; the rebuild
	// floor keeps search-time tombstone lookup bounded.
	maxUncommittedDeltaShards = 32
	// uncommittedDeltaShardMax keeps each delta shard small so a long
	// session of single-file saves does not balloon disk usage. Matches
	// folder delta sizing.
	uncommittedDeltaShardMax = 1024 * 1024
)

type uncommittedManifest struct {
	Version int                        `json:"version"`
	State   string                     `json:"state"`
	Files   []uncommittedManifestEntry `json:"files"`
}

type uncommittedManifestEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Ino   uint64 `json:"ino"`
}

// readUncommittedManifest returns the manifest persisted at cacheDir when it
// matches expectedState. Returns ok=false on any mismatch, missing file, or
// unmarshal error — callers fall back to a full rebuild in that case.
func readUncommittedManifest(cacheDir, expectedState string) (uncommittedManifest, bool) {
	if expectedState == "" {
		return uncommittedManifest{}, false
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, uncommittedManifestFileName))
	if err != nil {
		return uncommittedManifest{}, false
	}
	var manifest uncommittedManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return uncommittedManifest{}, false
	}
	if manifest.Version != uncommittedManifestVersion || manifest.State != expectedState {
		return uncommittedManifest{}, false
	}
	return manifest, true
}

// writeUncommittedManifest persists entries (sorted by name) bound to state.
// Atomic via tmp+rename like the folder manifest.
func writeUncommittedManifest(cacheDir, state string, entries []uncommittedManifestEntry) error {
	sorted := make([]uncommittedManifestEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	manifest := uncommittedManifest{
		Version: uncommittedManifestVersion,
		State:   state,
		Files:   sorted,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	path := filepath.Join(cacheDir, uncommittedManifestFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	// On Rename success the tmp path is consumed; the Remove below is a
	// no-op. On Rename failure (ENOSPC, EBUSY, etc.) we'd otherwise leave a
	// `.tmp` orphan that future readers might trip over.
	defer func() { _ = os.Remove(tmp) }()
	return os.Rename(tmp, path)
}

func deleteUncommittedManifest(cacheDir string) {
	_ = os.Remove(filepath.Join(cacheDir, uncommittedManifestFileName))
	_ = os.Remove(filepath.Join(cacheDir, uncommittedManifestFileName+".tmp"))
}

// uncommittedCandidate describes one dirty file at lstat-time. Used to diff
// against the manifest before reading content.
type uncommittedCandidate struct {
	name  string
	size  int64
	mtime int64
	ino   uint64
}

// uncommittedManifestEntry returns the manifest-shaped view of this candidate.
func (c uncommittedCandidate) manifestEntry() uncommittedManifestEntry {
	return uncommittedManifestEntry{
		Name:  c.name,
		Size:  c.size,
		Mtime: c.mtime,
		Ino:   c.ino,
	}
}

// statUncommittedCandidates lstats each dirty file under repoDir and returns
// them sorted by name. Missing files are reported as zero-stat entries so
// downstream diff can emit tombstones. The returned slice is always sorted.
func statUncommittedCandidates(repoDir string, files []string) []uncommittedCandidate {
	out := make([]uncommittedCandidate, 0, len(files))
	for _, f := range files {
		path := filepath.Join(repoDir, f)
		var stat syscall.Stat_t
		if err := syscall.Lstat(path, &stat); err != nil {
			// Deleted between git status and stat — record as zero so the
			// diff treats it as content change vs any prior manifest entry.
			out = append(out, uncommittedCandidate{name: f})
			continue
		}
		if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
			// Non-regular files are skipped by streamFiles too; mirror the
			// exclusion here so they never appear in the manifest.
			continue
		}
		out = append(out, uncommittedCandidate{
			name:  f,
			size:  stat.Size,
			mtime: statMtimeNano(stat),
			ino:   stat.Ino,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// diffUncommittedAgainstManifest performs a sorted merge between the current
// dirty-file candidates and the prior manifest. Returns the list of paths
// that need a tombstone (renames, content changes, removals) and the subset
// of candidates whose content must be re-read for the delta shard.
//
// Algorithm mirrors changedFolderDocumentsFromManifest in cmd/seek/folder_indexer.go.
// Both slices must already be sorted by name.
func diffUncommittedAgainstManifest(
	candidates []uncommittedCandidate,
	manifest uncommittedManifest,
) (toRead []uncommittedCandidate, changedPaths []string) {
	old := manifest.Files
	i, j := 0, 0
	for i < len(candidates) && j < len(old) {
		c := candidates[i]
		o := old[j]
		switch {
		case c.name == o.Name:
			if !uncommittedEntryMatches(o, c) {
				changedPaths = append(changedPaths, c.name)
				toRead = append(toRead, c)
			}
			i++
			j++
		case c.name < o.Name:
			changedPaths = append(changedPaths, c.name)
			toRead = append(toRead, c)
			i++
		default:
			changedPaths = append(changedPaths, o.Name)
			j++
		}
	}
	for ; i < len(candidates); i++ {
		changedPaths = append(changedPaths, candidates[i].name)
		toRead = append(toRead, candidates[i])
	}
	for ; j < len(old); j++ {
		changedPaths = append(changedPaths, old[j].Name)
	}
	return toRead, changedPaths
}

func uncommittedEntryMatches(old uncommittedManifestEntry, c uncommittedCandidate) bool {
	return old.Size == c.size && old.Mtime == c.mtime && old.Ino == c.ino
}
