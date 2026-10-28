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
	// uncommittedManifestFileName stores the list of dirty files that the
	// last build put in the "uncommitted" Zoekt repo. Each entry binds a path
	// to its working-tree identity. So the next build finds the added,
	// changed, and removed files without a full rebuild of the shard family.
	uncommittedManifestFileName = ".uncommitted-manifest-v1"
	uncommittedManifestVersion  = 4
	// maxUncommittedDeltaShards is the most shards that the uncommitted family
	// can hold before a delta; above it, the build does a full rebuild. It is
	// lower than the committed cap (maxCommittedDeltaShards) for two reasons.
	// An editor changes one dirty file per save, and each shard costs every
	// search an open and a lookup. A delta of removals only adds no shard:
	// deltaNeedsCompaction bounds the dead content instead. A delta of kept
	// files alone passes the cap, by at most one shard per kept file. So a file
	// that stays unreadable does not rebuild the family on each search.
	maxUncommittedDeltaShards = 32
)

type uncommittedManifest struct {
	Version int    `json:"version"`
	State   string `json:"state"`
	// DeadBytes is the content that deltas tombstoned since the last full
	// build (deltaNeedsCompaction).
	DeadBytes int64                      `json:"dead_bytes"`
	Files     []uncommittedManifestEntry `json:"files"`
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
	// Marker is the size of the NOT-INDEXED marker that the shard holds
	// instead of the content, as the read found it (nameOnlyMarkerBytes).
	// The marker comes from a skip reason of the reader, or from content with
	// a NUL byte or of one or two bytes. Zero means the read found no marker.
	// heldBytes then still counts a marker for a kept file (size -1) or a
	// file too large for the index.
	Marker int64 `json:"marker,omitempty"`
}

// readUncommittedManifest returns the manifest persisted at cacheDir when it
// matches expectedState. ok is false on any mismatch, a missing file, or an
// unmarshal error; callers then do a full rebuild. A manifest with no state is
// pending: a build kept some dirty files by name and wrote no state. The
// manifest marks those files kept (size -1). So a search with no state finds
// the manifest and reads only those files.
func readUncommittedManifest(cacheDir, expectedState string) (uncommittedManifest, bool) {
	var manifest uncommittedManifest
	data, err := os.ReadFile(filepath.Join(cacheDir, uncommittedManifestFileName))
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		return uncommittedManifest{}, false
	}
	return manifest, manifest.Version == uncommittedManifestVersion && manifest.State == expectedState
}

// writeUncommittedManifest writes entries bound to state, with deadBytes, the
// content that deltas tombstoned since the last full build. It does not sort
// them: the caller gives them sorted by name, as statUncommittedCandidates
// returns them, because the next diff merges two sorted lists. It stores an
// unreadFileMarker as zero, because a manifest never stores that value.
func writeUncommittedManifest(cacheDir, state string, entries []uncommittedManifestEntry, deadBytes int64) error {
	for i := range entries {
		entries[i].Marker = max(entries[i].Marker, 0)
	}
	data, err := json.Marshal(uncommittedManifest{Version: uncommittedManifestVersion, State: state, DeadBytes: deadBytes, Files: entries})
	if err != nil {
		return err
	}
	return writeCacheFile(cacheDir, uncommittedManifestFileName, string(data))
}

func deleteUncommittedManifest(cacheDir string) {
	removeCacheFile(cacheDir, uncommittedManifestFileName)
}

// uncommittedCandidate describes one dirty file at lstat time. The diff
// against the manifest reads it before any content read. Its entry has no
// Marker: only the content read finds one.
type uncommittedCandidate struct {
	uncommittedManifestEntry
	// gone marks a path whose Lstat found no file (ENOENT, ENOTDIR). Any
	// other Lstat error leaves a zero stat that the reader reads.
	gone bool
}

// uncommittedManifestEntries returns the manifest view of the candidates. A
// gone file has no entry: when the file comes back, the diff finds no old copy
// to tombstone.
func uncommittedManifestEntries(candidates []uncommittedCandidate) []uncommittedManifestEntry {
	entries := make([]uncommittedManifestEntry, 0, len(candidates))
	for _, c := range candidates {
		if !c.gone {
			entries = append(entries, c.uncommittedManifestEntry)
		}
	}
	return entries
}

// statUncommittedCandidates lstats each dirty file under repoDir and returns
// them sorted by name. A missing file gives a gone entry, so the diff can
// tombstone its old copy. A file that is not a regular file has no entry.
func statUncommittedCandidates(repoDir string, files []string) []uncommittedCandidate {
	out := make([]uncommittedCandidate, 0, len(files))
	for _, f := range files {
		path := filepath.Join(repoDir, f)
		var stat syscall.Stat_t
		if err := syscall.Lstat(path, &stat); err != nil {
			// A deleted file needs no read; an error such as ENAMETOOLONG
			// or EIO leaves a zero stat, so the diff reads it.
			out = append(out, uncommittedCandidate{
				uncommittedManifestEntry: uncommittedManifestEntry{Name: f},
				gone:                     errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR),
			})
			continue
		}
		if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
			// streamDirtyFiles also skips a file that is not regular. The
			// same rule here keeps it out of the manifest.
			continue
		}
		out = append(out, uncommittedCandidate{uncommittedManifestEntry: uncommittedManifestEntry{
			Name:  f,
			Size:  stat.Size,
			Mtime: statMtimeNano(stat),
			Ino:   stat.Ino,
			Mode:  uint32(stat.Mode),
			Ctime: statCtimeNano(stat),
		}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// diffUncommittedAgainstManifest merges the sorted current dirty-file
// candidates with the prior manifest. It returns three values:
//   - toRead: the candidates whose content the delta shard must read again.
//   - changedPaths: the paths that the delta marks as changed or removed
//     (additions, renames, content changes, removals).
//   - tombstoned: the bytes of the prior copies that the tombstones hide.
//
// The merge follows changedFolderDocumentsFromManifest in folder_indexer.go.
// Both slices must already be sorted by name.
func diffUncommittedAgainstManifest(
	candidates []uncommittedCandidate,
	manifest uncommittedManifest,
) (toRead []uncommittedCandidate, changedPaths []string, tombstoned int64) {
	old := manifest.Files
	i, j := 0, 0
	for i < len(candidates) && j < len(old) {
		c := candidates[i]
		o := old[j]
		switch {
		case c.Name == o.Name:
			if !o.sameStat(c.uncommittedManifestEntry) {
				changedPaths = append(changedPaths, c.Name)
				tombstoned += o.heldBytes()
				if !c.gone {
					toRead = append(toRead, c)
				}
			}
			i++
			j++
		case c.Name < o.Name:
			// A new path with no file (a tracked file deleted) holds
			// nothing to read or tombstone.
			if !c.gone {
				changedPaths = append(changedPaths, c.Name)
				toRead = append(toRead, c)
			}
			i++
		default:
			changedPaths = append(changedPaths, o.Name)
			tombstoned += o.heldBytes()
			j++
		}
	}
	for ; i < len(candidates); i++ {
		if !candidates[i].gone {
			changedPaths = append(changedPaths, candidates[i].Name)
			toRead = append(toRead, candidates[i])
		}
	}
	for ; j < len(old); j++ {
		changedPaths = append(changedPaths, old[j].Name)
		tombstoned += old[j].heldBytes()
	}
	return toRead, changedPaths, tombstoned
}

// heldBytes is the content bytes that the shard document of the entry holds
// (shardHeldBytes). A file kept by name (size -1) has no bytes.
func (e uncommittedManifestEntry) heldBytes() int64 {
	return shardHeldBytes(e.Marker, e.Size, e.Size < 0)
}

// uncommittedLiveBytes returns the content bytes that the shard documents of
// entries hold (uncommittedManifestEntry.heldBytes).
func uncommittedLiveBytes(entries []uncommittedManifestEntry) int64 {
	var live int64
	for _, e := range entries {
		live += e.heldBytes()
	}
	return live
}

// copyUncommittedMarkers sets the Marker of each unchanged entry from the
// manifest entry of the same name. Every other entry gets unreadFileMarker
// until the delta reads it. Both slices are sorted by name.
func copyUncommittedMarkers(entries []uncommittedManifestEntry, manifest uncommittedManifest) {
	for i := range entries {
		entries[i].Marker = unreadFileMarker
	}
	i, j := 0, 0
	for i < len(entries) && j < len(manifest.Files) {
		switch entry, old := entries[i], manifest.Files[j]; {
		case entry.Name == old.Name:
			if entry.sameStat(old) {
				entries[i].Marker = old.Marker
			}
			i++
			j++
		case entry.Name < old.Name:
			i++
		default:
			j++
		}
	}
}

// setUncommittedMarkers sets the Marker of each entry that docs read, as the
// read found it (nameOnlyMarkerBytes).
func setUncommittedMarkers(entries []uncommittedManifestEntry, docs []fileContent) {
	if len(docs) == 0 {
		return
	}
	read := make(map[string]int64, len(docs))
	for _, doc := range docs {
		read[doc.name] = nameOnlyMarkerBytes(doc)
	}
	for i := range entries {
		if marker, ok := read[entries[i].Name]; ok {
			entries[i].Marker = marker
		}
	}
}

// sameStat reports whether two entries hold the same lstat fields. It does not
// compare Name, which the callers match first, or Marker, which the content
// read sets.
func (e uncommittedManifestEntry) sameStat(other uncommittedManifestEntry) bool {
	return e.Size == other.Size && e.Mtime == other.Mtime && e.Ino == other.Ino &&
		e.Mode == other.Mode && e.Ctime == other.Ctime
}
