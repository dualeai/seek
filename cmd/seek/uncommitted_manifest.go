package main

import (
	"encoding/json"
	"errors"
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
	uncommittedManifestVersion  = 2
	// maxUncommittedDeltaShards caps stacked delta shards before falling
	// back to a full rebuild. Lower than the committed cap because the
	// uncommitted set churns one file per save in editors; the rebuild
	// floor keeps search-time tombstone lookup bounded. A delta of kept
	// files alone passes the cap, by at most one shard per kept file, so a
	// file that stays unreadable does not rebuild the family on each search.
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
	// Mode and Ctime change on a chmod alone: a file kept by name only for
	// its permissions is read again.
	Mode  uint32 `json:"mode"`
	Ctime int64  `json:"ctime"`
}

// readUncommittedManifest returns the manifest persisted at cacheDir when it
// matches expectedState. Returns ok=false on any mismatch, missing file, or
// unmarshal error — callers fall back to a full rebuild in that case.
// A manifest with no state is pending: a build kept some dirty files by name
// and wrote no state, and the manifest marks those files kept (size -1), so a
// search with no state finds it and reads only them.
func readUncommittedManifest(cacheDir, expectedState string) (uncommittedManifest, bool) {
	var manifest uncommittedManifest
	data, err := os.ReadFile(filepath.Join(cacheDir, uncommittedManifestFileName))
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		return uncommittedManifest{}, false
	}
	return manifest, manifest.Version == uncommittedManifestVersion && manifest.State == expectedState
}

// writeUncommittedManifest writes entries bound to state. It does not sort
// them: the caller gives them sorted by name, as statUncommittedCandidates
// returns them, because the next diff merges two sorted lists.
func writeUncommittedManifest(cacheDir, state string, entries []uncommittedManifestEntry) error {
	data, err := json.Marshal(uncommittedManifest{Version: uncommittedManifestVersion, State: state, Files: entries})
	if err != nil {
		return err
	}
	return writeCacheFile(cacheDir, uncommittedManifestFileName, string(data))
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
	mode  uint32
	ctime int64
	// gone marks a path whose Lstat found no file (ENOENT, ENOTDIR). Any
	// other Lstat error leaves a zero stat that the reader reads.
	gone bool
}

// uncommittedManifestEntries returns the manifest view of the candidates. A
// gone file has no entry, so its return holds nothing to tombstone.
func uncommittedManifestEntries(candidates []uncommittedCandidate) []uncommittedManifestEntry {
	entries := make([]uncommittedManifestEntry, 0, len(candidates))
	for _, c := range candidates {
		if !c.gone {
			entries = append(entries, c.manifestEntry())
		}
	}
	return entries
}

// manifestEntry returns the manifest-shaped view of this candidate.
func (c uncommittedCandidate) manifestEntry() uncommittedManifestEntry {
	return uncommittedManifestEntry{
		Name:  c.name,
		Size:  c.size,
		Mtime: c.mtime,
		Ino:   c.ino,
		Mode:  c.mode,
		Ctime: c.ctime,
	}
}

// statUncommittedCandidates lstats each dirty file under repoDir and returns
// them sorted by name. Missing files are reported as gone entries so the diff
// can emit tombstones. The returned slice is always sorted.
func statUncommittedCandidates(repoDir string, files []string) []uncommittedCandidate {
	out := make([]uncommittedCandidate, 0, len(files))
	for _, f := range files {
		path := filepath.Join(repoDir, f)
		var stat syscall.Stat_t
		if err := syscall.Lstat(path, &stat); err != nil {
			// A deleted file needs no read; an error such as ENAMETOOLONG
			// or EIO leaves a zero stat, so the diff reads it.
			out = append(out, uncommittedCandidate{name: f, gone: errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR)})
			continue
		}
		if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
			// Non-regular files are skipped by streamDirtyFiles too; mirror the
			// exclusion here so they never appear in the manifest.
			continue
		}
		out = append(out, uncommittedCandidate{
			name:  f,
			size:  stat.Size,
			mtime: statMtimeNano(stat),
			ino:   stat.Ino,
			mode:  uint32(stat.Mode),
			ctime: statCtimeNano(stat),
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
				if !c.gone {
					toRead = append(toRead, c)
				}
			}
			i++
			j++
		case c.name < o.Name:
			// A new path with no file (a tracked file deleted) holds
			// nothing to read or tombstone.
			if !c.gone {
				changedPaths = append(changedPaths, c.name)
				toRead = append(toRead, c)
			}
			i++
		default:
			changedPaths = append(changedPaths, o.Name)
			j++
		}
	}
	for ; i < len(candidates); i++ {
		if !candidates[i].gone {
			changedPaths = append(changedPaths, candidates[i].name)
			toRead = append(toRead, candidates[i])
		}
	}
	for ; j < len(old); j++ {
		changedPaths = append(changedPaths, old[j].Name)
	}
	return toRead, changedPaths
}

func uncommittedEntryMatches(old uncommittedManifestEntry, c uncommittedCandidate) bool {
	return old.Size == c.size && old.Mtime == c.mtime && old.Ino == c.ino &&
		old.Mode == c.mode && old.Ctime == c.ctime
}
