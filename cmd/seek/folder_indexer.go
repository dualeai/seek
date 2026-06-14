package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"

	"github.com/cespare/xxhash/v2"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/sys/unix"
)

const (
	// Folder deltas are usually tiny. A smaller shard target reduces Zoekt's
	// per-builder map sizing without changing the shard compatibility hash.
	folderDeltaShardMax    = 1024 * 1024
	maxFolderDeltaShards   = 64
	folderManifestVersion  = 1
	folderManifestFileName = ".folder-manifest-v1"
	// Higher fanout was slower and more variable on APFS in the 1k-file bench.
	maxFolderFingerprintWorkers = 6
)

var folderChecksumTable = crc64.MakeTable(crc64.ISO)

type folderCandidate struct {
	name  string
	path  string
	mode  os.FileMode
	size  int64
	mtime int64
	dev   uint64
	ino   uint64
}

func ensureFolderCorpusFresh(ctx context.Context, plan corpusPlan) (corpusIndexState, error) {
	cachedState := readStateFile(plan.cacheDir)
	hasShards := shardsExist(plan.indexDir)

	var stateHash string
	var selected []folderCandidate
	var selectedCount int
	stateReady := false
	selectedLoaded := false

	if cachedState != "" {
		var err error
		stateHash, selectedCount, err = folderCorpusFingerprint(ctx, plan)
		if err != nil {
			return corpusSearchable, folderCorpusError(plan, err)
		}
		stateReady = true
		if cachedState == stateHash {
			if selectedCount == 0 {
				return corpusKnownEmpty, nil
			}
			if hasShards {
				return corpusSearchable, nil
			}
		}
	}

	if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
		return corpusSearchable, folderCorpusError(plan, fmt.Errorf("create folder index directory: %w", err))
	}

	lockPath := filepath.Join(plan.cacheDir, lockFile)
	lockFd, acquired, err := acquireLock(ctx, plan.indexDir, lockPath)
	if err != nil {
		return corpusSearchable, folderCorpusError(plan, err)
	}
	if !acquired {
		slog.Warn("Another process is indexing, using existing index")
		return corpusSearchable, nil
	}
	defer releaseLock(lockFd)
	defer func() {
		_ = os.Remove(filepath.Join(plan.cacheDir, stateTmpFile))
	}()

	if !stateReady {
		var err error
		stateHash, selected, err = folderCorpusState(ctx, plan)
		if err != nil {
			deleteStateFiles(plan.cacheDir)
			return corpusSearchable, folderCorpusError(plan, err)
		}
		selectedCount = len(selected)
		selectedLoaded = true
	} else if latestState := readStateFile(plan.cacheDir); latestState == stateHash {
		if selectedCount == 0 {
			return corpusKnownEmpty, nil
		}
		if shardsExist(plan.indexDir) {
			return corpusSearchable, nil
		}
	} else if latestState != cachedState {
		var err error
		stateHash, selected, err = folderCorpusState(ctx, plan)
		if err != nil {
			deleteStateFiles(plan.cacheDir)
			return corpusSearchable, folderCorpusError(plan, err)
		}
		selectedCount = len(selected)
		selectedLoaded = true
	}
	if !selectedLoaded && selectedCount > 0 {
		var err error
		stateHash, selected, err = folderCorpusState(ctx, plan)
		if err != nil {
			deleteStateFiles(plan.cacheDir)
			return corpusSearchable, folderCorpusError(plan, err)
		}
		selectedCount = len(selected)
	}
	if readStateFile(plan.cacheDir) == stateHash {
		if selectedCount == 0 {
			return corpusKnownEmpty, nil
		}
		if shardsExist(plan.indexDir) {
			return corpusSearchable, nil
		}
	}

	repoName := folderRepoName(plan)
	if selectedCount == 0 {
		cleanRepositoryShards(plan.indexDir, repoName)
		if err := writeStateFile(plan.cacheDir, stateHash); err != nil {
			return corpusSearchable, folderCorpusError(plan, fmt.Errorf("write folder state file: %w", err))
		}
		return corpusKnownEmpty, nil
	}

	if err := checkCtagsCached(); err != nil {
		deleteStateFiles(plan.cacheDir)
		return corpusSearchable, folderCorpusError(plan, err)
	}

	parallelism := indexParallelism()
	indexedAny, err := indexFolderDocuments(ctx, plan, repoName, selected, parallelism, cachedState != "" && hasShards, cachedState)
	if err != nil {
		deleteStateFiles(plan.cacheDir)
		return corpusSearchable, folderCorpusError(plan, err)
	}
	if err := writeStateFile(plan.cacheDir, stateHash); err != nil {
		return corpusSearchable, folderCorpusError(plan, fmt.Errorf("write folder state file: %w", err))
	}
	if err := writeFolderManifest(plan.cacheDir, stateHash, selected); err != nil {
		slog.Debug("Failed to write folder manifest", "error", err)
	}
	if !indexedAny {
		return corpusKnownEmpty, nil
	}
	return corpusSearchable, nil
}

func folderCorpusState(ctx context.Context, plan corpusPlan) (string, []folderCandidate, error) {
	if plan.rootType == rootTypeDirectory {
		return folderCorpusStateParallel(ctx, plan)
	}
	stateHash, selected, _, err := scanFolderCorpus(ctx, plan, true)
	if err != nil {
		return "", nil, err
	}
	return stateHash, selected, nil
}

func folderCorpusError(plan corpusPlan, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(
		"folder corpus root=%q cache=%q index=%q: %w",
		plan.root,
		plan.cacheDir,
		plan.indexDir,
		err,
	)
}

func folderCorpusFingerprint(ctx context.Context, plan corpusPlan) (string, int, error) {
	if plan.rootType == rootTypeDirectory {
		return folderCorpusFingerprintParallel(ctx, plan)
	}
	stateHash, _, selectedCount, err := scanFolderCorpus(ctx, plan, false)
	return stateHash, selectedCount, err
}

func scanFolderCorpus(ctx context.Context, plan corpusPlan, collectSelected bool) (string, []folderCandidate, int, error) {
	stateHasher := newFolderStateHasher(plan)
	switch plan.rootType {
	case rootTypeFile:
		info, err := os.Lstat(plan.root)
		if err != nil {
			return "", nil, 0, fmt.Errorf("read file root: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", nil, 0, fmt.Errorf("unsupported file root: %s", plan.root)
		}
		candidate := newFolderCandidate(filepath.Base(plan.root), plan.root, info)
		appendFolderCandidateHash(stateHasher, candidate)
		var indexedBytes int64
		selected, selectedCandidate, err := selectFolderCandidate(nil, candidate, &indexedBytes, collectSelected)
		if err != nil {
			return "", nil, 0, err
		}
		selectedCount := 0
		if selectedCandidate {
			selectedCount = 1
		}
		return finishFolderStateHash(stateHasher), selected, selectedCount, nil
	case rootTypeDirectory:
	default:
		return "", nil, 0, fmt.Errorf("unsupported folder root type: %s", plan.rootType)
	}

	scanner := folderCorpusScanner{
		ctx:             ctx,
		stateHasher:     stateHasher,
		collectSelected: collectSelected,
	}
	if err := scanner.walkDirectory(plan.root, ""); err != nil {
		return "", nil, 0, err
	}
	return finishFolderStateHash(stateHasher), scanner.selected, scanner.selectedCount, nil
}

type folderCorpusScanner struct {
	ctx             context.Context
	stateHasher     *xxhash.Digest
	selected        []folderCandidate
	indexedBytes    int64
	candidateCount  int
	selectedCount   int
	collectSelected bool
}

func (s *folderCorpusScanner) walkDirectory(dir, relBase string) error {
	dirFile, entries, err := openSortedDirectory(dir)
	if err != nil {
		if relBase == "" {
			return fmt.Errorf("read folder root: %w", err)
		}
		return nil
	}
	defer func() { _ = dirFile.Close() }()
	dirFD := int(dirFile.Fd())
	separator := string(filepath.Separator)
	for _, entry := range entries {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		rel := name
		if relBase != "" {
			rel = relBase + "/" + name
		}
		if entry.IsDir() {
			path := dir + separator + name
			if isFolderMetadataDir(name) {
				continue
			}
			if err := s.walkDirectory(path, rel); err != nil {
				return err
			}
			continue
		}
		if s.collectSelected {
			path := dir + separator + name
			if err := s.addSelectedFile(dirFD, name, path, rel); err != nil {
				return err
			}
		} else {
			if err := s.addFingerprintFile(dirFD, name, rel); err != nil {
				return err
			}
		}
	}
	return nil
}

func openSortedDirectory(dir string) (*os.File, []os.DirEntry, error) {
	dirFile, err := os.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		_ = dirFile.Close()
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	return dirFile, entries, nil
}

func (s *folderCorpusScanner) addFingerprintFile(dirFD int, name, rel string) error {
	stat, ok := fstatatRegularFile(dirFD, name)
	if !ok {
		return nil
	}
	s.candidateCount++
	if s.candidateCount > maxFolderCandidateFiles {
		return folderCapError("folder file cap exceeded", "candidate_files", int64(s.candidateCount), maxFolderCandidateFiles)
	}
	appendFolderFileHash(s.stateHasher, rel, stat.mode, stat.size, stat.mtime, stat.dev, stat.ino)
	if stat.size > maxFolderFileSize {
		return nil
	}
	s.indexedBytes += stat.size
	if s.indexedBytes > maxFolderIndexedBytes {
		return folderCapError("folder indexed byte cap exceeded", "indexed_bytes", s.indexedBytes, maxFolderIndexedBytes)
	}
	s.selectedCount++
	return nil
}

func (s *folderCorpusScanner) addSelectedFile(dirFD int, name, path, rel string) error {
	stat, ok := fstatatRegularFile(dirFD, name)
	if !ok {
		return nil
	}
	s.candidateCount++
	if s.candidateCount > maxFolderCandidateFiles {
		return folderCapError("folder file cap exceeded", "candidate_files", int64(s.candidateCount), maxFolderCandidateFiles)
	}
	candidate := folderCandidate{
		name:  rel,
		path:  path,
		mode:  stat.mode,
		size:  stat.size,
		mtime: stat.mtime,
		dev:   stat.dev,
		ino:   stat.ino,
	}
	appendFolderCandidateHash(s.stateHasher, candidate)
	selected, selectedCandidate, err := selectFolderCandidate(
		s.selected,
		candidate,
		&s.indexedBytes,
		s.collectSelected,
	)
	if err != nil {
		return err
	}
	s.selected = selected
	if selectedCandidate {
		s.selectedCount++
	}
	return nil
}

type folderFileStat struct {
	mode  os.FileMode
	size  int64
	mtime int64
	dev   uint64
	ino   uint64
}

func fstatatRegularFile(dirFD int, name string) (folderFileStat, bool) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return folderFileStat{}, false
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return folderFileStat{}, false
	}
	return folderFileStat{
		mode:  regularFileMode(uint32(st.Mode)),
		size:  st.Size,
		mtime: st.Mtim.Nano(),
		dev:   uint64(st.Dev),
		ino:   uint64(st.Ino),
	}, true
}

func regularFileMode(mode uint32) os.FileMode {
	out := os.FileMode(mode & 0o777)
	if mode&unix.S_ISUID != 0 {
		out |= os.ModeSetuid
	}
	if mode&unix.S_ISGID != 0 {
		out |= os.ModeSetgid
	}
	if mode&unix.S_ISVTX != 0 {
		out |= os.ModeSticky
	}
	return out
}

func isFolderMetadataDir(name string) bool {
	return name == ".git"
}

func selectFolderCandidate(
	selected []folderCandidate,
	candidate folderCandidate,
	indexedBytes *int64,
	collectSelected bool,
) ([]folderCandidate, bool, error) {
	if candidate.size > maxFolderFileSize {
		return selected, false, nil
	}
	*indexedBytes += candidate.size
	if *indexedBytes > maxFolderIndexedBytes {
		return selected, false, folderCapError("folder indexed byte cap exceeded", "indexed_bytes", *indexedBytes, maxFolderIndexedBytes)
	}
	if !collectSelected {
		return selected, true, nil
	}
	return append(selected, candidate), true, nil
}

type folderFingerprintPiece struct {
	hash               uint64
	candidates         int
	selected           int
	indexedBytes       int64
	selectedCandidates []folderCandidate
	present            bool
}

func folderCorpusFingerprintParallel(ctx context.Context, plan corpusPlan) (string, int, error) {
	stateHash, selectedCount, _, err := scanFolderRootEntriesParallel(ctx, plan, false)
	return stateHash, selectedCount, err
}

func folderCorpusStateParallel(ctx context.Context, plan corpusPlan) (string, []folderCandidate, error) {
	stateHash, _, selected, err := scanFolderRootEntriesParallel(ctx, plan, true)
	return stateHash, selected, err
}

func scanFolderRootEntriesParallel(
	ctx context.Context,
	plan corpusPlan,
	collectSelected bool,
) (string, int, []folderCandidate, error) {
	entries, err := os.ReadDir(plan.root)
	if err != nil {
		return "", 0, nil, fmt.Errorf("read folder root: %w", err)
	}
	if len(entries) <= 1 {
		stateHash, selected, selectedCount, err := scanFolderCorpus(ctx, plan, collectSelected)
		return stateHash, selectedCount, selected, err
	}

	workers := fileReadWorkerCount(maxFolderFingerprintWorkers, len(entries))
	jobs := make(chan int, workers)
	pieces := make([]folderFingerprintPiece, len(entries))
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				piece, err := fingerprintRootEntry(ctx, plan.root, entries[i], collectSelected)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					continue
				}
				pieces[i] = piece
			}
		}()
	}
	for i := range entries {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return "", 0, nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errCh:
		return "", 0, nil, err
	default:
	}
	if err := ctx.Err(); err != nil {
		return "", 0, nil, err
	}

	stateHasher := newFolderStateHasher(plan)
	var candidateCount int
	var selectedCount int
	var indexedBytes int64
	var selected []folderCandidate
	var numBuf [20]byte
	for _, piece := range pieces {
		if !piece.present {
			continue
		}
		appendFolderFingerprintPart(stateHasher, "entry")
		appendFolderFingerprintBytes(stateHasher, strconv.AppendUint(numBuf[:0], piece.hash, 10))
		candidateCount += piece.candidates
		if candidateCount > maxFolderCandidateFiles {
			return "", 0, nil, folderCapError("folder file cap exceeded", "candidate_files", int64(candidateCount), maxFolderCandidateFiles)
		}
		indexedBytes += piece.indexedBytes
		if indexedBytes > maxFolderIndexedBytes {
			return "", 0, nil, folderCapError("folder indexed byte cap exceeded", "indexed_bytes", indexedBytes, maxFolderIndexedBytes)
		}
		selectedCount += piece.selected
		if collectSelected {
			selected = append(selected, piece.selectedCandidates...)
		}
	}
	return finishFolderStateHash(stateHasher), selectedCount, selected, nil
}

func fingerprintRootEntry(
	ctx context.Context,
	root string,
	entry os.DirEntry,
	collectSelected bool,
) (folderFingerprintPiece, error) {
	name := entry.Name()
	if entry.Type()&os.ModeSymlink != 0 {
		return folderFingerprintPiece{}, nil
	}
	path := root + string(filepath.Separator) + name
	if entry.IsDir() {
		if isFolderMetadataDir(name) {
			return folderFingerprintPiece{}, nil
		}
		var h xxhash.Digest
		h.Reset()
		scanner := folderCorpusScanner{
			ctx:             ctx,
			stateHasher:     &h,
			collectSelected: collectSelected,
		}
		if err := scanner.walkDirectory(path, name); err != nil {
			return folderFingerprintPiece{}, err
		}
		return folderFingerprintPiece{
			hash:               h.Sum64(),
			candidates:         scanner.candidateCount,
			selected:           scanner.selectedCount,
			indexedBytes:       scanner.indexedBytes,
			selectedCandidates: scanner.selected,
			present:            scanner.candidateCount > 0,
		}, nil
	}
	info, err := entry.Info()
	if err != nil {
		return folderFingerprintPiece{}, nil
	}
	if !info.Mode().IsRegular() {
		return folderFingerprintPiece{}, nil
	}
	var h xxhash.Digest
	h.Reset()
	dev, ino, mtime := fileInfoIdentity(info)
	appendFolderFileHash(&h, name, info.Mode(), info.Size(), mtime, dev, ino)
	piece := folderFingerprintPiece{
		hash:       h.Sum64(),
		candidates: 1,
		present:    true,
	}
	if info.Size() <= maxFolderFileSize {
		piece.indexedBytes = info.Size()
		piece.selected = 1
		if collectSelected {
			piece.selectedCandidates = []folderCandidate{{
				name:  name,
				path:  path,
				mode:  info.Mode(),
				size:  info.Size(),
				mtime: mtime,
				dev:   dev,
				ino:   ino,
			}}
		}
	}
	return piece, nil
}

func indexFolderDocuments(
	ctx context.Context,
	plan corpusPlan,
	repoName string,
	selected []folderCandidate,
	parallelism int,
	tryDelta bool,
	cachedState string,
) (bool, error) {
	if tryDelta {
		cleanEmptyShards(ctx, plan.indexDir, repoName)
		shardCount := repositoryShardCount(plan.indexDir, repoName)
		if shardCount > 0 && shardCount <= maxFolderDeltaShards {
			indexedAny, err := indexFolderDocumentsDelta(ctx, plan, repoName, selected, parallelism, cachedState)
			if err == nil {
				return indexedAny, nil
			}
			slog.Debug("Folder delta indexing failed, falling back to full rebuild", "error", err)
		} else if shardCount > maxFolderDeltaShards {
			slog.Debug("Folder delta shard limit reached, falling back to full rebuild", "shards", shardCount)
		}
	}
	fileCh := streamFolderFiles(ctx, selected, parallelism)
	return indexDocuments(ctx, plan.indexDir, repoName, plan.root, fileCh, parallelism)
}

func indexFolderDocumentsDelta(
	ctx context.Context,
	plan corpusPlan,
	repoName string,
	selected []folderCandidate,
	parallelism int,
	cachedState string,
) (bool, error) {
	changedDocs, changedPaths, err := changedFolderDocumentsSinceCachedState(ctx, plan, repoName, selected, cachedState)
	if err != nil {
		return false, err
	}
	if len(changedPaths) == 0 {
		return len(selected) > 0, nil
	}
	if len(changedDocs) == 0 {
		return false, fmt.Errorf("folder delta contains only removals")
	}

	return indexDeltaDocuments(plan.indexDir, repoName, plan.root, changedDocs, folderDeltaShardMax, changedPaths)
}

func changedFolderDocumentsSinceCachedState(
	ctx context.Context,
	plan corpusPlan,
	repoName string,
	selected []folderCandidate,
	cachedState string,
) ([]fileContent, []string, error) {
	if manifest, ok := readFolderManifest(plan.cacheDir, cachedState); ok {
		changedDocs, changedPaths := changedFolderDocumentsFromManifest(selected, manifest)
		return changedDocs, changedPaths, nil
	}

	oldChecksums, err := enumerateIndexedFolderChecksums(ctx, plan.indexDir, repoName)
	if err != nil {
		return nil, nil, err
	}
	changedDocs, changedPaths := changedFolderDocuments(selected, oldChecksums)
	return changedDocs, changedPaths, nil
}

func enumerateIndexedFolderChecksums(ctx context.Context, indexDir, repoName string) (map[string]uint64, error) {
	searchers, err := loadShards(indexDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, s := range searchers {
			s.Close()
		}
	}()

	opts := zoekt.SearchOptions{
		TotalMaxMatchCount: maxFolderCandidateFiles + 1,
		ShardMaxMatchCount: maxFolderCandidateFiles + 1,
		MaxDocDisplayCount: maxFolderCandidateFiles + 1,
		MaxWallTime:        searchTimeout,
	}
	oldChecksums := make(map[string]uint64)
	for _, searcher := range searchers {
		result, err := searcher.Search(ctx, &query.Const{Value: true}, &opts)
		if err != nil {
			return nil, err
		}
		for _, file := range result.Files {
			if file.Repository != repoName {
				continue
			}
			if len(file.Checksum) != crc64.Size {
				return nil, fmt.Errorf("indexed checksum missing for %s", file.FileName)
			}
			oldChecksums[file.FileName] = binary.BigEndian.Uint64(file.Checksum)
		}
	}
	return oldChecksums, nil
}

func changedFolderDocuments(selected []folderCandidate, oldChecksums map[string]uint64) ([]fileContent, []string) {
	changedDocs := make([]fileContent, 0, 1)
	changedPaths := make([]string, 0, 1)
	seen := make(map[string]struct{}, len(selected))

	for _, candidate := range selected {
		seen[candidate.name] = struct{}{}
		content, err := readFolderFile(candidate)
		if err != nil {
			if _, ok := oldChecksums[candidate.name]; ok {
				changedPaths = append(changedPaths, candidate.name)
			}
			continue
		}
		checksum := crc64.Checksum(content, folderChecksumTable)
		oldChecksum, ok := oldChecksums[candidate.name]
		if !ok || oldChecksum != checksum {
			changedPaths = append(changedPaths, candidate.name)
			changedDocs = append(changedDocs, fileContent{name: candidate.name, content: content})
		}
	}

	for name := range oldChecksums {
		if _, ok := seen[name]; !ok {
			changedPaths = append(changedPaths, name)
		}
	}
	return changedDocs, changedPaths
}

type folderManifest struct {
	Version int                   `json:"version"`
	State   string                `json:"state"`
	Files   []folderManifestEntry `json:"files"`
}

type folderManifestEntry struct {
	Name  string      `json:"name"`
	Mode  os.FileMode `json:"mode"`
	Size  int64       `json:"size"`
	Mtime int64       `json:"mtime"`
	Dev   uint64      `json:"dev"`
	Ino   uint64      `json:"ino"`
}

func readFolderManifest(cacheDir, expectedState string) (folderManifest, bool) {
	if expectedState == "" {
		return folderManifest{}, false
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, folderManifestFileName))
	if err != nil {
		return folderManifest{}, false
	}
	var manifest folderManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return folderManifest{}, false
	}
	if manifest.Version != folderManifestVersion || manifest.State != expectedState {
		return folderManifest{}, false
	}
	return manifest, true
}

func writeFolderManifest(cacheDir, state string, selected []folderCandidate) error {
	manifest := folderManifest{
		Version: folderManifestVersion,
		State:   state,
		Files:   make([]folderManifestEntry, len(selected)),
	}
	for i, candidate := range selected {
		manifest.Files[i] = folderManifestEntry{
			Name:  candidate.name,
			Mode:  candidate.mode,
			Size:  candidate.size,
			Mtime: candidate.mtime,
			Dev:   candidate.dev,
			Ino:   candidate.ino,
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	path := filepath.Join(cacheDir, folderManifestFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	return os.Rename(tmp, path)
}

func changedFolderDocumentsFromManifest(selected []folderCandidate, manifest folderManifest) ([]fileContent, []string) {
	// Both slices are in scanner order: root entries are sorted, subdirectories
	// are sorted, and manifests preserve the selected list order.
	changedDocs := make([]fileContent, 0, 1)
	changedPaths := make([]string, 0, 1)
	selectedIndex := 0
	manifestIndex := 0

	for selectedIndex < len(selected) && manifestIndex < len(manifest.Files) {
		candidate := selected[selectedIndex]
		old := manifest.Files[manifestIndex]
		switch {
		case candidate.name == old.Name:
			if !folderManifestFileMatchesCandidate(old, candidate) {
				changedPaths = append(changedPaths, candidate.name)
				if content, err := readFolderFile(candidate); err == nil {
					changedDocs = append(changedDocs, fileContent{name: candidate.name, content: content})
				}
			}
			selectedIndex++
			manifestIndex++
		case candidate.name < old.Name:
			changedPaths = append(changedPaths, candidate.name)
			if content, err := readFolderFile(candidate); err == nil {
				changedDocs = append(changedDocs, fileContent{name: candidate.name, content: content})
			}
			selectedIndex++
		default:
			changedPaths = append(changedPaths, old.Name)
			manifestIndex++
		}
	}

	for ; selectedIndex < len(selected); selectedIndex++ {
		candidate := selected[selectedIndex]
		changedPaths = append(changedPaths, candidate.name)
		if content, err := readFolderFile(candidate); err == nil {
			changedDocs = append(changedDocs, fileContent{name: candidate.name, content: content})
		}
	}

	for ; manifestIndex < len(manifest.Files); manifestIndex++ {
		changedPaths = append(changedPaths, manifest.Files[manifestIndex].Name)
	}
	return changedDocs, changedPaths
}

func folderManifestFileMatchesCandidate(file folderManifestEntry, candidate folderCandidate) bool {
	return file.Mode == candidate.mode &&
		file.Size == candidate.size &&
		file.Mtime == candidate.mtime &&
		file.Dev == candidate.dev &&
		file.Ino == candidate.ino
}

func newFolderCandidate(name, path string, info os.FileInfo) folderCandidate {
	dev, ino, mtime := fileInfoIdentity(info)
	return folderCandidate{
		name:  name,
		path:  path,
		mode:  info.Mode(),
		size:  info.Size(),
		mtime: mtime,
		dev:   dev,
		ino:   ino,
	}
}

func fileInfoIdentity(info os.FileInfo) (uint64, uint64, int64) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino), statMtimeNano(*st)
	}
	return 0, 0, info.ModTime().UnixNano()
}

func newFolderStateHasher(plan corpusPlan) *xxhash.Digest {
	h := xxhash.New()
	_, _ = h.WriteString(stateVersion)

	appendFolderFingerprintPart(h, "folder-state-v3")
	appendFolderFingerprintPart(h, "index_generation")
	appendFolderFingerprintPart(h, seekIndexGeneration)
	appendFolderFingerprintPart(h, "cache_layout")
	appendFolderFingerprintPart(h, seekCacheLayoutVersion)
	appendFolderFingerprintPart(h, "document_naming")
	appendFolderFingerprintPart(h, seekDocumentNamingVersion)
	appendFolderFingerprintPart(h, "zoekt_compatibility")
	appendFolderFingerprintPart(h, zoektCompatibilityVersion)
	appendFolderFingerprintPart(h, "index_options")
	appendFolderFingerprintPart(h, indexOptionsHash())
	appendFolderFingerprintPart(h, "root")
	appendFolderFingerprintPart(h, canonicalCorpusPath(plan.root))
	appendFolderFingerprintPart(h, "root_type")
	appendFolderFingerprintPart(h, string(plan.rootType))

	if id, err := filesystemIdentity(plan.root); err == nil {
		appendFolderFingerprintPart(h, "root_identity")
		appendFolderFingerprintPart(h, id)
	}
	return h
}

func appendFolderCandidateHash(h *xxhash.Digest, candidate folderCandidate) {
	appendFolderFileHash(h, candidate.name, candidate.mode, candidate.size, candidate.mtime, candidate.dev, candidate.ino)
}

func appendFolderFileHash(
	h *xxhash.Digest,
	name string,
	mode os.FileMode,
	size int64,
	mtime int64,
	dev uint64,
	ino uint64,
) {
	var numBuf [20]byte
	appendFolderFingerprintPart(h, "file")
	appendFolderFingerprintPart(h, name)
	appendFolderFingerprintBytes(h, strconv.AppendUint(numBuf[:0], uint64(mode), 10))
	appendFolderFingerprintBytes(h, strconv.AppendInt(numBuf[:0], size, 10))
	appendFolderFingerprintBytes(h, strconv.AppendInt(numBuf[:0], mtime, 10))
	appendFolderFingerprintBytes(h, strconv.AppendUint(numBuf[:0], dev, 10))
	appendFolderFingerprintBytes(h, strconv.AppendUint(numBuf[:0], ino, 10))
}

func finishFolderStateHash(h *xxhash.Digest) string {
	return formatHex16(h.Sum64())
}

func appendFolderFingerprintPart(h *xxhash.Digest, part string) {
	_, _ = h.WriteString("\x00")
	_, _ = h.WriteString(part)
}

func appendFolderFingerprintBytes(h *xxhash.Digest, part []byte) {
	_, _ = h.WriteString("\x00")
	_, _ = h.Write(part)
}

// streamFolderFiles returns a channel that yields file contents for
// each folder candidate. The output channel is unbuffered. In-flight
// memory is bounded by two limits, whichever is tighter:
//   - the worker count (each worker holds at most one fileContent),
//   - readSemaphore's maxInFlightBytes byte budget (see caps.go).
//
// Each yielded fileContent carries a weight (= candidate.size pre-read)
// on the readSemaphore. The consumer MUST Release that weight after
// builder.Finish() returns. Abandoning the channel without draining
// would hang workers blocked on send until ctx cancellation.
//
// Synchronous folder-delta reads via readFolderFile DO NOT go through
// this entry point and pass fileContent with weight=0; that is by
// design (those reads are not bounded by readSemaphore today).
func streamFolderFiles(ctx context.Context, candidates []folderCandidate, parallelism int) <-chan fileContent {
	workers := fileReadWorkerCount(parallelism, len(candidates))
	out := make(chan fileContent)
	work := make(chan folderCandidate, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for candidate := range work {
				if ctx.Err() != nil {
					continue
				}
				readOneFolderFileStreaming(ctx, candidate, out)
			}
		}()
	}

	go func() {
		for _, candidate := range candidates {
			select {
			case work <- candidate:
			case <-ctx.Done():
				close(work)
				wg.Wait()
				close(out)
				return
			}
		}
		close(work)
		wg.Wait()
		close(out)
	}()

	return out
}

// readOneFolderFileStreaming is the per-file worker body for
// streamFolderFiles. It Acquires readSemaphore weight against the
// candidate's pre-read size, reads the file, then transfers Release
// ownership to the consumer via fileContent.weight on a successful
// channel send. Failure paths between Acquire and a successful send
// run the deferred sentinel and Release the weight here.
func readOneFolderFileStreaming(ctx context.Context, candidate folderCandidate, out chan<- fileContent) {
	size := candidate.size
	if size > maxFolderFileSize {
		// readFolderFile would have rejected this anyway; mirror the
		// guard here to avoid acquiring weight we cannot honor.
		return
	}
	if size > maxInFlightBytes {
		// Defensive: caps.go enforces size <= maxFolderFileSize,
		// and the compile-time invariant guarantees maxFolderFileSize
		// (= maxIndexedDocumentBytes) <= maxInFlightBytes via
		// inFlightHeadroomFiles. Guard against future cap drift
		// hanging Acquire forever (golang/go#59002).
		slog.Warn("Folder file exceeds in-flight memory ceiling, skipping",
			"path", candidate.name, "size", size)
		return
	}

	weight := size
	if err := readSemaphore.Acquire(ctx, weight); err != nil {
		return // ctx cancelled.
	}
	released := false
	defer func() {
		if !released {
			readSemaphore.Release(weight)
		}
	}()

	content, err := readFolderFile(candidate)
	if err != nil {
		return
	}
	select {
	case out <- fileContent{name: candidate.name, content: content, weight: weight}:
		released = true
	case <-ctx.Done():
		return
	}
}

func readFolderFile(candidate folderCandidate) ([]byte, error) {
	if candidate.size > maxFolderFileSize {
		return nil, fmt.Errorf("file exceeds max size")
	}
	file, err := os.Open(candidate.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	content := make([]byte, candidate.size)
	n, err := io.ReadFull(file, content)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	content = content[:n]

	var extra [1]byte
	extraN, err := file.Read(extra[:])
	if err != nil && err != io.EOF {
		return nil, err
	}
	if extraN > 0 {
		return nil, fmt.Errorf("file grew beyond max size")
	}
	return content, nil
}

func folderRepoName(plan corpusPlan) string {
	return "folder_" + string(plan.id)
}
