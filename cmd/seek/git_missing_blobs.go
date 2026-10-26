package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// A partial clone can lack blobs, and Seek indexes their files by name only: a
// search never fetches. A blob can arrive later (sparse-checkout add, a fetch)
// with no change to HEAD or to the work tree. So each build records the blobs
// that it read as missing, with the object-store fingerprint from before the
// read. A search compares the fingerprint, and checks the blobs again only
// when the store changed. On a Kubernetes checkout, a check of 31,250 missing
// blobs takes about 1 s, and the fingerprint less than 10 ms.
const (
	lexicalMissingBlobsFile  = ".git-missing-lexical-v1"
	semanticMissingBlobsFile = ".git-missing-semantic-v1"
	missingBlobsVersion      = "v1"
	// unknownObjectStore never equals a fingerprint, so each search checks a
	// record that has it. Such records are a damaged record, a store that Seek
	// cannot read, and the record that a build writes before it publishes.
	unknownObjectStore = "unknown"
)

// missingBlob is one file that a build indexed by name only.
type missingBlob struct {
	oid  gitObjectID
	path string
}

// missingBlobRecord names the blobs that a build of head read as missing.
type missingBlobRecord struct {
	head        string
	fingerprint string
	blobs       []missingBlob
}

// readMissingBlobRecord reads one record; false means there is none.
func readMissingBlobRecord(cacheDir, name string) (missingBlobRecord, bool) {
	data, err := os.ReadFile(filepath.Join(cacheDir, name))
	if err != nil {
		return missingBlobRecord{}, false
	}
	// A damaged record cannot say which blobs are missing, so it marks its
	// data stale: the search rebuilds that data.
	damaged := missingBlobRecord{fingerprint: unknownObjectStore}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) < 4 || lines[0] != missingBlobsVersion {
		return damaged, true
	}
	head, headOK := strings.CutPrefix(lines[1], "head ")
	fingerprint, storeOK := strings.CutPrefix(lines[2], "store ")
	if !headOK || !storeOK || head == "" {
		return damaged, true
	}
	record := missingBlobRecord{head: head, fingerprint: fingerprint, blobs: make([]missingBlob, 0, len(lines)-3)}
	for _, line := range lines[3:] {
		rawOID, quoted, _ := strings.Cut(line, " ")
		oid, oidErr := parseGitObjectID([]byte(rawOID))
		path, pathErr := strconv.Unquote(quoted)
		if oidErr != nil || pathErr != nil {
			return damaged, true
		}
		record.blobs = append(record.blobs, missingBlob{oid: oid, path: path})
	}
	return record, true
}

// writeMissingBlobRecord replaces the record, or removes it when the build
// read every blob.
func writeMissingBlobRecord(cacheDir, name string, record missingBlobRecord) error {
	if len(record.blobs) == 0 {
		removeCacheFile(cacheDir, name)
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nhead %s\nstore %s\n", missingBlobsVersion, record.head, record.fingerprint)
	for _, blob := range record.blobs {
		fmt.Fprintf(&b, "%s %s\n", blob.oid, strconv.Quote(blob.path))
	}
	return writeCacheFile(cacheDir, name, b.String())
}

// writePendingMissingBlobRecord writes, before a publish, the union of the
// blobs of the live data and of the new build. A stop before the exact record
// then leaves a record that names every name-only file of either, with a
// store that forces a check. A damaged live record (no head) stays as it is.
// It cannot name its blobs, but it makes the next search rebuild the data.
// The union would lose that mark.
func writePendingMissingBlobRecord(cacheDir, name, head string, blobs []missingBlob) error {
	if old, ok := readMissingBlobRecord(cacheDir, name); ok {
		if old.head == "" {
			return nil
		}
		blobs = append(slices.Clone(blobs), old.blobs...)
	}
	return writeMissingBlobRecord(cacheDir, name, missingBlobRecord{
		head:        head,
		fingerprint: unknownObjectStore,
		blobs:       blobs,
	})
}

// gitObjectStoreFingerprint returns the counts of `git count-objects -v` and
// a fingerprint of each alternate object directory, whose objects Git does not
// count. An object that arrives, loose or in a pack, changes them; a repack
// changes them too, and costs one check. An alternate that Seek cannot read
// gives unknownObjectStore.
func gitObjectStoreFingerprint(ctx context.Context, repoDir string) (string, error) {
	out, err := gitOutput(ctx, newGitCommand(ctx, repoDir, "count-objects", "-v"), "git count-objects")
	if err != nil {
		return "", err
	}
	var fields []string
	for line := range strings.Lines(string(out)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ": ")
		switch key {
		case "alternate":
			alternate, err := objectDirectoryFingerprint(value)
			if err != nil {
				return unknownObjectStore, nil
			}
			fields = append(fields, "alternate="+alternate)
		case "count", "size", "in-pack", "packs", "size-pack":
			fields = append(fields, key+"="+value)
		}
	}
	return strings.Join(fields, ","), nil
}

// objectDirectoryFingerprint hashes what a new object changes in an object
// directory. That is its own mtime (a new loose fan-out directory), and the
// mtime of each fan-out directory (a new loose object). It is also the name,
// size, and mtime of each pack file.
func objectDirectoryFingerprint(dir string) (string, error) {
	hash := sha256.New()
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(hash, "%d\n", info.ModTime().UnixNano())
	packs, err := os.ReadDir(filepath.Join(dir, "pack"))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	for _, pack := range packs {
		info, err := pack.Info()
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(hash, "%s %d %d\n", pack.Name(), info.Size(), info.ModTime().UnixNano())
	}
	for fanOut := range 256 {
		if info, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%02x", fanOut))); err == nil {
			_, _ = fmt.Fprintf(hash, "%02x %d\n", fanOut, info.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(hash.Sum(nil)[:12]), nil
}

// buildObjectStoreFingerprint returns the fingerprint that a build records. It
// reads the fingerprint once, before the recheck and before any blob read. So
// a blob that arrives after it changes the store that the next search sees.
// Only a partial clone can lack a blob, and a node with a record keeps its
// check.
func buildObjectStoreFingerprint(ctx context.Context, repoDir, cacheDir string) string {
	if !repoIsPartialClone(ctx, repoDir) && len(nodeMissingBlobRecords(cacheDir, "", false)) == 0 {
		return ""
	}
	fingerprint, err := gitObjectStoreFingerprint(ctx, repoDir)
	if err != nil {
		return unknownObjectStore
	}
	return fingerprint
}

// nodeMissingBlobRecords returns the records that describe the node's current
// data: the lexical one, and the semantic one of head when a search reads it.
func nodeMissingBlobRecords(cacheDir, head string, semantic bool) map[string]missingBlobRecord {
	records := map[string]missingBlobRecord{}
	if record, ok := readMissingBlobRecord(cacheDir, lexicalMissingBlobsFile); ok {
		records[lexicalMissingBlobsFile] = record
	}
	if !semantic {
		return records
	}
	if record, ok := readMissingBlobRecord(cacheDir, semanticMissingBlobsFile); ok && (record.head == head || record.head == "") {
		records[semanticMissingBlobsFile] = record
	}
	return records
}

// missingBlobCheckDue reports whether the object store changed since a record
// of the node was written. With no record, it runs no Git command.
func missingBlobCheckDue(ctx context.Context, repoDir, cacheDir, head string, semantic bool) bool {
	records := nodeMissingBlobRecords(cacheDir, head, semantic)
	if len(records) == 0 {
		return false
	}
	fingerprint, err := gitObjectStoreFingerprint(ctx, repoDir)
	if err != nil {
		// The build path checks again and reports the failure.
		return true
	}
	for _, record := range records {
		if record.fingerprint != fingerprint || fingerprint == unknownObjectStore {
			return true
		}
	}
	return false
}

// missingBlobArrival says which data of the node holds a name-only file whose
// blob is now present. It also holds the blobs that a lexical delta must keep
// in its record.
type missingBlobArrival struct {
	lexical      bool
	semantic     bool
	lexicalCarry []missingBlob
}

// recheckMissingBlobs checks the blobs of each record whose fingerprint is not
// fingerprint, with one `git cat-file` and no fetch. A record whose blobs are
// all still missing gets fingerprint, so the next search checks nothing. The
// caller holds the build lock: no build replaces a record meanwhile.
func recheckMissingBlobs(
	ctx context.Context,
	repoDir, cacheDir, head string,
	semantic bool,
	fingerprint string,
) (missingBlobArrival, error) {
	records := nodeMissingBlobRecords(cacheDir, head, semantic)
	if len(records) == 0 {
		return missingBlobArrival{}, nil
	}
	current := func(record missingBlobRecord) bool {
		return record.fingerprint == fingerprint && fingerprint != unknownObjectStore
	}
	var checked []gitObjectID
	for _, record := range records {
		if !current(record) {
			for _, blob := range record.blobs {
				checked = append(checked, blob.oid)
			}
		}
	}
	slices.Sort(checked)
	present, err := presentGitBlobs(ctx, repoDir, slices.Compact(checked))
	if err != nil {
		return missingBlobArrival{}, err
	}
	var arrival missingBlobArrival
	for name, record := range records {
		arrived := !current(record) && (record.head == "" || slices.ContainsFunc(record.blobs, func(blob missingBlob) bool {
			_, ok := present[blob.oid]
			return ok
		}))
		switch {
		case arrived && name == lexicalMissingBlobsFile:
			arrival.lexical = true
		case arrived:
			arrival.semantic = true
		default:
			if name == lexicalMissingBlobsFile {
				arrival.lexicalCarry = record.blobs
			}
			if current(record) {
				continue
			}
			record.fingerprint = fingerprint
			if err := writeMissingBlobRecord(cacheDir, name, record); err != nil {
				// The next search checks again; the result is correct, only
				// slower.
				slog.Debug("Failed to refresh a missing-blob record", "file", name, "error", err)
			}
		}
	}
	return arrival, nil
}

// deltaMissingBlobs returns the record of a delta build. It is the base record
// less the paths that the delta changed, plus the blobs that the delta read as
// missing.
func deltaMissingBlobs(carry []missingBlob, changedPaths []string, infos []gitBlobInfo) []missingBlob {
	changed := make(map[string]struct{}, len(changedPaths))
	for _, path := range changedPaths {
		changed[path] = struct{}{}
	}
	blobs := missingBlobsOf(infos)
	for _, blob := range carry {
		if _, ok := changed[blob.path]; !ok {
			blobs = append(blobs, blob)
		}
	}
	return blobs
}

// presentGitBlobs returns the blobs of oids that the object store now holds.
// The batch reads with no lazy fetch, and it does not warn: the build that
// recorded the blobs gave the warning.
func presentGitBlobs(ctx context.Context, repoDir string, oids []gitObjectID) (map[gitObjectID]struct{}, error) {
	present := map[gitObjectID]struct{}{}
	if len(oids) == 0 {
		return present, nil
	}
	entries := make([]gitTreeEntry, len(oids))
	for i, oid := range oids {
		entries[i] = gitTreeEntry{mode: "100644", oid: oid}
	}
	batch, err := startNativeGitBatch(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !batch.finished {
			batch.abort(nil)
		}
	}()
	for start := 0; start < len(entries); start += gitObjectBatchSize {
		infos, err := batch.check(entries[start:min(start+gitObjectBatchSize, len(entries))])
		if err != nil {
			return nil, err
		}
		for _, info := range infos {
			if !info.missing {
				present[info.entry.oid] = struct{}{}
			}
		}
	}
	batch.missing = 0
	return present, batch.close()
}

// missingBlobsOf returns the files of infos that the read found missing.
func missingBlobsOf(infos []gitBlobInfo) []missingBlob {
	var blobs []missingBlob
	for _, info := range infos {
		if info.missing {
			blobs = append(blobs, missingBlob{oid: info.entry.oid, path: info.entry.path})
		}
	}
	return blobs
}
