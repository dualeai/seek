package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/sourcegraph/zoekt/index"
)

const (
	// uncommittedManifestFileName stores the list of dirty files that the
	// last build put in the "uncommitted" Zoekt repo. Each entry binds a path
	// to its working-tree identity, so the next build finds the added,
	// changed, and removed files without a full rebuild of the shard family.
	uncommittedManifestFileName = ".uncommitted-manifest-v1"
	uncommittedManifestVersion  = 4
	// maxUncommittedDeltaShards caps stacked delta shards before falling
	// back to a full rebuild. Lower than the committed cap because the
	// uncommitted set churns one file per save in editors, and each shard
	// costs every search an open and a lookup. A delta of removals only adds
	// no shard: deltaNeedsCompaction bounds the dead content instead. A delta
	// of kept files alone passes the cap, by at most one shard per kept file,
	// so a file that stays unreadable does not rebuild the family on each
	// search.
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
	// instead of the content, as the read found it (nameOnlyMarkerBytes):
	// a skip reason from the reader, or content with a NUL byte or of one or
	// two bytes. Zero means the read found no marker; heldBytes then still
	// counts a marker for a kept file (size -1) or a file too large for the
	// index.
	Marker int64 `json:"marker,omitempty"`
}

// readUncommittedManifest returns the manifest persisted at cacheDir when it
// matches expectedState. Returns ok=false on any mismatch, missing file, or
// unmarshal error; callers fall back to a full rebuild in that case.
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
// gone file has no entry: when the file comes back, the diff finds no old copy
// to tombstone.
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
// can emit tombstones. Files that are not regular files have no entry.
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
// dirty-file candidates and the prior manifest. Returns the subset of
// candidates whose content must be re-read for the delta shard, the paths
// that the delta marks as changed or removed (additions, renames, content
// changes, removals), and the bytes of the prior copies that the tombstones
// hide.
//
// Algorithm mirrors changedFolderDocumentsFromManifest in
// cmd/seek/folder_indexer.go. Both slices must already be sorted by name.
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
		case c.name == o.Name:
			if !uncommittedEntryMatches(o, c) {
				changedPaths = append(changedPaths, c.name)
				tombstoned += o.heldBytes()
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
			tombstoned += o.heldBytes()
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
		tombstoned += old[j].heldBytes()
	}
	return toRead, changedPaths, tombstoned
}

// heldBytes is the content bytes that the shard document of the entry holds:
// the NOT-INDEXED marker for a file kept by name only (Marker, a kept file of
// size -1, or one too large for the index), else its size. An unread file
// (unreadFileMarker) counts at the larger of that value and the largest marker
// (maxNotIndexedMarkerBytes).
func (e uncommittedManifestEntry) heldBytes() int64 {
	switch {
	case e.Marker == unreadFileMarker:
		unread := e
		unread.Marker = 0
		return max(unread.heldBytes(), maxNotIndexedMarkerBytes)
	case e.Marker > 0:
		return e.Marker
	case e.Size < 0:
		return notIndexedMarkerBytes(seekNoBytesReason)
	case !documentContentFits(e.Size):
		return notIndexedMarkerBytes(index.SkipReasonTooLarge)
	}
	return e.Size
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
// manifest entry of the same name, and unreadFileMarker for every other entry
// until the delta reads it. Both slices are sorted by name.
func copyUncommittedMarkers(entries []uncommittedManifestEntry, manifest uncommittedManifest) {
	for i := range entries {
		entries[i].Marker = unreadFileMarker
	}
	i, j := 0, 0
	for i < len(entries) && j < len(manifest.Files) {
		switch entry, old := entries[i], manifest.Files[j]; {
		case entry.Name == old.Name:
			if entry.Size == old.Size && entry.Mtime == old.Mtime && entry.Ino == old.Ino &&
				entry.Mode == old.Mode && entry.Ctime == old.Ctime {
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

func uncommittedEntryMatches(old uncommittedManifestEntry, c uncommittedCandidate) bool {
	return old.Size == c.size && old.Mtime == c.mtime && old.Ino == c.ino &&
		old.Mode == c.mode && old.Ctime == c.ctime
}
