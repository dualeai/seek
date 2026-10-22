package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc64"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/cespare/xxhash/v2"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

const (
	maxFolderDeltaShards   = 64
	folderManifestVersion  = 3
	folderManifestFileName = ".folder-manifest-v1"
)

var folderChecksumTable = crc64.MakeTable(crc64.ISO)

type folderCandidate struct {
	name  string
	path  string
	mode  os.FileMode
	size  int64
	mtime int64
	// ctime moves on every write and on every metadata change, so a rewrite
	// that keeps the size and puts the old mtime back is still seen.
	ctime int64
	dev   uint64
	ino   uint64
	// dataless marks a cloud-only file: it is indexed by name only, so the
	// walk never downloads it.
	dataless bool
}

// heldBytes is the content bytes that the shard document of the file holds:
// the NOT-INDEXED marker for a file kept by name only (marker, a cloud-only
// file, or one too large for the index), else its size. marker is the marker
// bytes that a read found (nameOnlyMarkerBytes), zero when the shard holds the
// content, or unreadFileMarker. An unread file counts at the larger of that
// value and the largest marker (maxNotIndexedMarkerBytes).
func (c folderCandidate) heldBytes(marker int64) int64 {
	switch {
	case marker == unreadFileMarker:
		return max(c.heldBytes(0), maxNotIndexedMarkerBytes)
	case marker > 0:
		return marker
	case c.dataless:
		return notIndexedMarkerBytes(seekNoBytesReason)
	case !documentContentFits(c.size):
		return notIndexedMarkerBytes(index.SkipReasonTooLarge)
	}
	return c.size
}

// indexedBytes is the content that the index reads for the file: none for a
// file indexed by name only.
func (c folderCandidate) indexedBytes() int64 {
	if c.dataless || !documentContentFits(c.size) {
		return 0
	}
	return c.size
}

// seedFolderDelta links the live family into buildDir for a delta and reports
// whether the delta can run. A full build links no seed, so after a link error
// it cleans buildDir and builds in full, as after a failed delta.
func seedFolderDelta(buildDir, repoName string, paths []string) bool {
	if err := seedFamilyFiles(buildDir, paths); err != nil {
		slog.Debug("Folder delta seed failed; full rebuild", "error", err)
		cleanRepositoryShards(buildDir, repoName)
		return false
	}
	return true
}

// folderBuildTestHook runs after the index builds and before the publish. Only
// tests set it.
var folderBuildTestHook func()

// folderFamilyUsable checks the current shard family after the build lock is
// acquired. A peer can publish after the pre-lock scan. One surviving shard is
// not sufficient because the manifest can record more family members.
func folderFamilyUsable(indexDir string) bool {
	sc, err := scanFamilyOptional(indexDir)
	return err == nil && sc.familyComplete(indexDir)
}

// ensureFolderCorpusFreshWithExecution makes the Zoekt family and, when
// enabled, its semantic generation describe one captured folder state. Missing
// branches build concurrently. It activates the joined descriptor last, under
// the publish lock. A semantic failure leaves the Zoekt family searchable.
// When a peer holds the build lock, it serves the current shards
// (corpusStale).
func ensureFolderCorpusFreshWithExecution(
	ctx context.Context,
	plan node,
	execution searchExecution,
) (corpusIndexState, error) {
	// A folder corpus has no commit, so the head test does not apply.
	semanticRequired := execution.buildsVectors("")
	cachedState := readStateFile(plan.cacheDir)
	// One scan checks both shard presence and manifest agreement. Presence
	// alone accepts any one surviving shard; the manifest detects missing or
	// changed family members.
	warmScan, warmScanErr := scanFamilyOptional(plan.indexDir)
	if warmScanErr != nil {
		return corpusSearchable, folderCorpusError(plan, warmScanErr)
	}
	hasShards := warmScan.familyComplete(plan.indexDir)

	var stateHash string
	var selected []folderCandidate
	var selectedCount int
	stateReady := false
	selectedLoaded := false

	// A leftover .swapping marker means that a publish stopped during its swap,
	// and the family may be torn. Skip the warm fast path, so that lockBuild
	// below repairs it (recoverIncompleteSwap).
	swapPending := readCacheFile(plan.cacheDir, swappingMarkerFile) != ""

	// The graph expansion's walk serves every preparation of the command: its
	// state and its files. After a route sees a change, the node walks again.
	if hash, captured, ok := plan.folder.current(); ok {
		stateHash, selected, selectedCount = hash, captured, len(captured)
		stateReady, selectedLoaded = true, true
	} else if cachedState != "" && !swapPending {
		var err error
		stateHash, selectedCount, err = folderCorpusFingerprint(ctx, plan)
		if err != nil {
			return corpusSearchable, folderCorpusError(plan, err)
		}
		stateReady = true
	}
	if stateReady && !swapPending {
		if cachedState == stateHash {
			if selectedCount == 0 {
				return corpusKnownEmpty, nil
			}
			if hasShards && (!semanticRequired || plan.joinedGenerationsMatch(stateHash, plan.folderSemanticSource(stateHash), "")) {
				return corpusSearchable, nil
			}
		}
	}

	if err := os.MkdirAll(plan.indexDir, 0o755); err != nil {
		return corpusSearchable, folderCorpusError(plan, fmt.Errorf("create folder index directory: %w", err))
	}

	release, err := acquireBuildSlot(ctx)
	if err != nil {
		return corpusSearchable, err
	}
	defer release()
	unlock, acquired, err := plan.lockBuild(ctx)
	if err != nil {
		return corpusSearchable, folderCorpusError(plan, err)
	}
	if !acquired {
		// A peer builds and shards exist: serve the last complete index
		// (corpusStale).
		return corpusStale, nil
	}
	defer unlock()

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
		// A peer can publish a family after the pre-lock scan, so scan again
		// when that scan found no complete family. Use the same test as the
		// warm path: presence alone would accept one surviving shard of a torn
		// family.
		if (hasShards || folderFamilyUsable(plan.indexDir)) &&
			(!semanticRequired || plan.joinedGenerationsMatch(stateHash, plan.folderSemanticSource(stateHash), "")) {
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
		if hasShards && (!semanticRequired || plan.joinedGenerationsMatch(stateHash, plan.folderSemanticSource(stateHash), "")) {
			return corpusSearchable, nil
		}
	}

	repoName := folderRepoName(plan)
	// Start a delta build only from the complete family recorded by the last
	// publish. A missing, extra, or resized shard or sidecar requires a full
	// build (matchesManifest). A full build starts in an empty temporary
	// directory.
	sc, scErr := scanFamily(plan.indexDir)
	if scErr != nil {
		return corpusSearchable, folderCorpusError(plan, scErr)
	}
	in := folderBuildInputs{
		stateHash:    stateHash,
		currentState: readStateFile(plan.cacheDir),
		hasShard:     sc.hasShard(familyAll),
		intact:       sc.matchesManifest(plan.indexDir),
		vectors:      semanticRequired,
	}
	if in.vectors {
		in.semanticPresent = semanticGenerationPresent(plan.indexDir, plan.folderSemanticSource(stateHash))
	}
	build := decideFolderBuild(in)
	if build.tornWarn {
		slog.Warn("Folder shard family does not match its manifest; rebuilding it in full",
			"index_dir", plan.indexDir)
	}
	// The published family is the one this scan describes. Every failure below
	// happens before the publish at the end of this function, so a failure
	// cannot have changed it. Keeping .state in that case keeps the delta base:
	// .state alone decides whether the next build can be a delta, so deleting
	// it turns every interrupted build into a full rebuild of a family that is
	// still correct on disk (build.keepState).
	//
	// dropStateUnlessPublished clears the cached state only when no usable
	// family survives. Every caller below sits before the publish.
	dropStateUnlessPublished := func() {
		if !build.keepState {
			deleteStateFiles(plan.cacheDir)
		}
	}
	semanticSource := plan.folderSemanticSource(stateHash)

	// Build in a temporary directory, then publish under the lock. A delta
	// build needs the prior shards, so seed them with hard links; a full build
	// ignores them.
	var buildDir string
	if build.lexical || build.semantic {
		buildDir, err = newBuildDir(plan.indexDir)
		if err != nil {
			return corpusSearchable, folderCorpusError(plan, err)
		}
		defer discardBuildDir(buildDir)
	}
	if build.delta {
		build.delta = seedFolderDelta(buildDir, repoName, sc.paths(familyAll))
	}
	indexedAny := !build.lexical && selectedCount > 0
	semanticBuilt := false
	semanticStaging := filepath.Join(buildDir, "semantic")
	var lexicalErr, semanticErr error
	if (build.lexical || build.semantic) && selectedCount > 0 {
		if err := checkCtagsCached(); err != nil {
			dropStateUnlessPublished()
			return corpusSearchable, folderCorpusError(plan, err)
		}
	}
	var lexicalBuild func()
	// tombstoned is the content that deltas tombstoned since the last full
	// build, and markers the marker bytes of each selected file, for the
	// folder manifest.
	var tombstoned int64
	var markers []int64
	if build.lexical && selectedCount > 0 {
		lexicalBuild = func() {
			indexedAny, tombstoned, markers, lexicalErr = indexFolderDocuments(
				ctx,
				plan,
				buildDir,
				repoName,
				selected,
				execution.resources.cpuLimit(),
				build.delta,
				in.currentState,
			)
		}
	}
	var semanticBuild func()
	if build.semantic && selectedCount > 0 {
		semanticBuild = func() {
			// Folders that hold this one, or that it holds, find its
			// generations by lineage.
			lineage := plan.semanticFolderLineage()
			_ = writeCacheFile(plan.cacheDir, semanticLineageFile, lineage)
			semanticErr = runSemanticBuild(ctx, execution.model, func(embedder semanticModel) error {
				_, err := buildSemanticFolderGeneration(
					ctx,
					selected,
					plan.indexDir,
					lineage,
					plan.modelPathBase(),
					semanticStaging,
					semanticSource,
					embedder,
					execution.resources,
				)
				return err
			})
			semanticBuilt = semanticErr == nil
		}
	}
	runJoinedIndexBuilds(lexicalBuild, semanticBuild)
	if folderBuildTestHook != nil {
		folderBuildTestHook()
	}
	if lexicalErr != nil {
		dropStateUnlessPublished()
		return corpusSearchable, folderCorpusError(plan, lexicalErr)
	}
	if semanticErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return corpusSearchable, ctxErr
		}
		semanticBuildFailed(execution.model, "Semantic folder index build failed; keeping lexical search", semanticErr)
	}

	// The index holds the bytes each file had when the build read it, under
	// the state of the walk. A file that changed after the walk is a change for
	// the next search: its stat no longer matches, so the next delta reads it
	// again.

	activateSemantic := func() {
		removeJoinedGeneration(plan.cacheDir)
		if !semanticRequired {
			return
		}
		var activationErr error
		if semanticBuilt {
			// Validate the new generation once, then bind it.
			activationErr = publishSemanticGeneration(plan.indexDir, semanticStaging, semanticSource, false)
		}
		if activationErr == nil && (semanticBuilt || in.semanticPresent) {
			activationErr = plan.bindSemanticGenerations(stateHash, semanticSource, "")
		}
		if activationErr != nil {
			semanticErr = activationErr
			slog.Debug(
				"Semantic folder index activation failed; keeping lexical search",
				"provider", semanticProviderName(),
				"error", activationErr,
			)
		}
	}

	if build.lexical {
		// Publish the shard family and state while the publish lock excludes
		// readers. The joined descriptor is the last activation record.
		if err := plan.publishGeneration(ctx, buildDir, familyAll, func() error {
			if err := writeStateFile(plan.cacheDir, stateHash); err != nil {
				return err
			}
			activateSemantic()
			return nil
		}); err != nil {
			if errors.Is(err, errCorpusEvicted) {
				return corpusSearchable, nil
			}
			return corpusSearchable, folderCorpusError(plan, err)
		}
	} else if semanticRequired && (build.semantic || !plan.joinedGenerationsMatch(stateHash, semanticSource, "")) {
		pub, publishErr := acquirePublishLock(ctx, plan.cacheDir)
		if publishErr != nil {
			if errors.Is(publishErr, errCorpusEvicted) {
				return corpusSearchable, nil
			}
			return corpusSearchable, folderCorpusError(plan, publishErr)
		}
		if readStateFile(plan.cacheDir) == stateHash && folderFamilyUsable(plan.indexDir) {
			activateSemantic()
		}
		releaseLock(pub)
	}

	if semanticErr != nil && !build.lexical {
		return corpusSearchable, nil
	}

	// Only a later build reads the folder state manifest. Searchers do not read
	// it, so it can be written after the publish lock is released. With no
	// lexical build, the published state is stateHash, so the manifest of that
	// build stays, with its tombstoned bytes.
	if build.lexical && selectedCount > 0 {
		if err := writeFolderManifest(plan.cacheDir, stateHash, selected, tombstoned, markers); err != nil {
			slog.Debug("Failed to write folder manifest", "error", err)
		}
	}
	if !indexedAny {
		return corpusKnownEmpty, nil
	}
	return corpusSearchable, nil
}

type folderCorpusContextError struct {
	root     string
	cacheDir string
	indexDir string
	cause    error
}

func (e *folderCorpusContextError) Error() string {
	return fmt.Sprintf(
		"folder corpus root=%q cache=%q index=%q: %v",
		e.root,
		e.cacheDir,
		e.indexDir,
		e.cause,
	)
}

func (e *folderCorpusContextError) Unwrap() error {
	return e.cause
}

func folderCorpusError(plan node, err error) error {
	return &folderCorpusContextError{
		root:     plan.root,
		cacheDir: plan.cacheDir,
		indexDir: plan.indexDir,
		cause:    err,
	}
}

// indexFolderDocuments indexes selected into dir, the build directory: a
// delta on the prior shards that dir holds when tryDelta holds and the family
// has at most maxFolderDeltaShards shards, else a full build. A failed delta
// also falls back to a full build. It returns the bytes that deltas tombstoned
// since the last full build (zero after a full build) and, by index of
// selected, the marker bytes that a file's shard document holds instead of its
// content (nameOnlyMarkerBytes), for the folder manifest. A read error that
// may pass fails the build with a folderReadError.
func indexFolderDocuments(
	ctx context.Context,
	plan node,
	dir string,
	repoName string,
	selected []folderCandidate,
	parallelism int,
	tryDelta bool,
	cachedState string,
) (bool, int64, []int64, error) {
	if tryDelta {
		shardCount := repositoryShardCount(dir, repoName)
		if shardCount > 0 && shardCount <= maxFolderDeltaShards {
			indexedAny, tombstoned, markers, err := indexFolderDocumentsDelta(ctx, plan, dir, repoName, selected, parallelism, cachedState)
			if err == nil {
				return indexedAny, tombstoned, markers, nil
			}
			// Log why the delta fell back to a full build: its payload
			// reached the window, its tombstones need a compaction, or the
			// delta failed.
			switch {
			case errors.Is(err, errDeltaPayloadExceedsWindow):
				slog.Debug("Folder delta payload exceeds window threshold; full rebuild", "error", err)
			case errors.Is(err, errDeltaCompaction):
				slog.Debug("Folder deltas tombstoned as many bytes as the folder holds; full rebuild")
			default:
				slog.Debug("Folder delta indexing failed, falling back to full rebuild", "error", err)
			}
		} else if shardCount > maxFolderDeltaShards {
			slog.Debug("Folder delta shard limit reached, falling back to full rebuild", "shards", shardCount)
		}
	}
	readCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	markers := make([]int64, len(selected))
	fileCh := streamFolderFilesWithMarkers(readCtx, selected, parallelism, cancel, markers)
	indexed, err := indexDocuments(readCtx, dir, repoName, plan.root, fileCh, parallelism)
	if readErr, ok := errors.AsType[*folderReadError](context.Cause(readCtx)); ok {
		return false, 0, nil, readErr
	}
	return indexed, 0, markers, err
}

// indexFolderDocumentsDelta writes the changed files of the folder into a
// delta on the prior shards in dir, with up to parallelism shard jobs at the
// same time. A delta of removals only adds no document; Zoekt then writes only
// the tombstones (indexDeltaDocumentsWithRepository). It returns the bytes
// that deltas tombstoned since the last full build, the markers of
// indexFolderDocuments, and errDeltaCompaction when the tombstoned bytes reach
// both the live bytes and one shard (deltaNeedsCompaction). The rule runs
// before the reads, with an upper bound of each unread file's shard bytes
// (unreadFileMarker), and again after them, with the bytes that each read
// file's shard document holds. It returns errDeltaPayloadExceedsWindow when
// the changed content reaches the build's window, with or without a manifest
// of the cached state. With no such manifest, the dead bytes are what the
// shards list beyond the live files, so a missing, failed, or older manifest
// does not reset them.
func indexFolderDocumentsDelta(
	ctx context.Context,
	plan node,
	dir string,
	repoName string,
	selected []folderCandidate,
	parallelism int,
	cachedState string,
) (bool, int64, []int64, error) {
	var changedDocs []fileContent
	var changedPaths []string
	var tombstoned int64
	markers := make([]int64, len(selected))
	for i := range markers {
		markers[i] = unreadFileMarker
	}
	if manifest, ok := readFolderManifest(plan.cacheDir, cachedState); ok {
		tombstoned = manifest.DeadBytes + folderManifestTombstonedBytes(selected, manifest)
		folderManifestMarkers(selected, manifest, markers)
		if deltaNeedsCompaction(tombstoned, folderLiveBytes(selected, markers)) {
			return false, 0, nil, errDeltaCompaction
		}
		var err error
		changedDocs, changedPaths, err = changedFolderDocumentsFromManifest(selected, manifest, indexWindow(ctx))
		if err != nil {
			return false, 0, nil, err
		}
		setFolderMarkers(selected, changedDocs, markers)
		if deltaNeedsCompaction(tombstoned, folderLiveBytes(selected, markers)) {
			return false, 0, nil, errDeltaCompaction
		}
	} else {
		// With no manifest, the shards give the dead content: they list the
		// content and name bytes of every document since the last full build,
		// and the live files hold the rest.
		oldChecksums, shardBytes, err := enumerateIndexedFolderChecksums(ctx, dir, repoName)
		if err != nil {
			return false, 0, nil, err
		}
		// Before the reads, each file counts at an upper bound of its shard
		// bytes (unreadFileMarker), so these dead bytes are a lower bound: the
		// rule cannot fire too early.
		if deltaNeedsCompaction(shardBytes-folderLiveBytesWithNames(selected, markers), folderLiveBytes(selected, markers)) {
			return false, 0, nil, errDeltaCompaction
		}
		changedDocs, changedPaths, err = changedFolderDocuments(selected, oldChecksums, markers, indexWindow(ctx))
		if err != nil {
			return false, 0, nil, err
		}
		for _, doc := range changedDocs {
			shardBytes += int64(len(doc.name)) + heldContentBytes(doc)
		}
		tombstoned = max(shardBytes-folderLiveBytesWithNames(selected, markers), 0)
		if deltaNeedsCompaction(tombstoned, folderLiveBytes(selected, markers)) {
			return false, 0, nil, errDeltaCompaction
		}
	}
	if len(changedPaths) == 0 {
		return len(selected) > 0, tombstoned, markers, nil
	}
	// Both paths stop at the build's window (changedFolderDocumentsFromManifest
	// before it reads the file that brings the changed size to it,
	// changedFolderDocuments when the changed content reaches it), so the one
	// delta Builder holds less than a window of content until its Finish.
	indexedAny, err := indexDeltaDocuments(dir, repoName, plan.root, changedDocs, parallelism, changedPaths)
	return indexedAny, tombstoned, markers, err
}

// folderLiveBytes returns the content bytes that the shard documents of
// selected hold, with markers by index (folderCandidate.heldBytes).
func folderLiveBytes(selected []folderCandidate, markers []int64) int64 {
	var live int64
	for i, candidate := range selected {
		live += candidate.heldBytes(markers[i])
	}
	return live
}

// folderLiveBytesWithNames is folderLiveBytes with the name bytes, as Zoekt
// counts a document in index.RepoStats.ContentBytes.
func folderLiveBytesWithNames(selected []folderCandidate, markers []int64) int64 {
	live := folderLiveBytes(selected, markers)
	for _, candidate := range selected {
		live += int64(len(candidate.name))
	}
	return live
}

// folderManifestMarkers copies the marker of each unchanged manifest entry to
// markers, by index of selected: a changed file keeps its marker of
// unreadFileMarker until it is read. Both slices are sorted by name.
func folderManifestMarkers(selected []folderCandidate, manifest folderManifest, markers []int64) {
	selectedIndex, manifestIndex := 0, 0
	for selectedIndex < len(selected) && manifestIndex < len(manifest.Files) {
		switch candidate, old := selected[selectedIndex], manifest.Files[manifestIndex]; {
		case candidate.name == old.Name:
			if folderManifestFileMatchesCandidate(old, candidate) {
				markers[selectedIndex] = old.Marker
			}
			selectedIndex++
			manifestIndex++
		case candidate.name < old.Name:
			selectedIndex++
		default:
			manifestIndex++
		}
	}
}

// setFolderMarkers sets the marker of each read document in markers, by index
// of selected.
func setFolderMarkers(selected []folderCandidate, docs []fileContent, markers []int64) {
	if len(docs) == 0 {
		return
	}
	read := make(map[string]int64, len(docs))
	for _, doc := range docs {
		read[doc.name] = nameOnlyMarkerBytes(doc)
	}
	for i, candidate := range selected {
		if marker, ok := read[candidate.name]; ok {
			markers[i] = marker
		}
	}
}

// folderManifestTombstonedBytes returns the held bytes of the manifest entries
// that a delta to selected tombstones: each changed and each removed file. It
// reads no file. Both slices are sorted by name.
func folderManifestTombstonedBytes(selected []folderCandidate, manifest folderManifest) int64 {
	var tombstoned int64
	selectedIndex, manifestIndex := 0, 0
	for selectedIndex < len(selected) && manifestIndex < len(manifest.Files) {
		candidate, old := selected[selectedIndex], manifest.Files[manifestIndex]
		switch {
		case candidate.name == old.Name:
			if !folderManifestFileMatchesCandidate(old, candidate) {
				tombstoned += old.heldBytes()
			}
			selectedIndex++
			manifestIndex++
		case candidate.name < old.Name:
			selectedIndex++
		default:
			tombstoned += old.heldBytes()
			manifestIndex++
		}
	}
	for ; manifestIndex < len(manifest.Files); manifestIndex++ {
		tombstoned += manifest.Files[manifestIndex].heldBytes()
	}
	return tombstoned
}

// enumerateIndexedFolderChecksums returns the content checksum of each live
// document of repoName in indexDir, and the content and name bytes that its
// shards hold, tombstoned documents included (index.RepoStats.ContentBytes).
func enumerateIndexedFolderChecksums(ctx context.Context, indexDir, repoName string) (map[string]uint64, int64, error) {
	searchers, err := loadShards(indexDir)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		for _, s := range searchers {
			s.Close()
		}
	}()

	// Every document: 0 would become Zoekt's default of 100,000 (SetDefaults).
	opts := zoekt.SearchOptions{ShardMaxMatchCount: math.MaxInt}
	oldChecksums := make(map[string]uint64)
	var shardBytes int64
	for _, searcher := range searchers {
		list, err := zoektList(ctx, searcher, &query.Const{Value: true}, nil)
		if err != nil {
			return nil, 0, err
		}
		for _, repo := range list.Repos {
			if repo.Repository.Name == repoName {
				shardBytes += repo.Stats.ContentBytes
			}
		}
		result, err := zoektSearch(ctx, searcher, &query.Const{Value: true}, &opts)
		if err != nil {
			return nil, 0, err
		}
		for _, file := range result.Files {
			if file.Repository != repoName {
				continue
			}
			if len(file.Checksum) != crc64.Size {
				return nil, 0, fmt.Errorf("indexed checksum missing for %s", file.FileName)
			}
			oldChecksums[file.FileName] = binary.BigEndian.Uint64(file.Checksum)
		}
	}
	return oldChecksums, shardBytes, nil
}

// changedFolderDocuments finds the changed files by content checksum, for a
// cached state with no matching manifest. It reads every selected file and
// sets its marker bytes in markers, by index (nameOnlyMarkerBytes). It keeps
// only the changed documents, and returns errDeltaPayloadExceedsWindow when
// the content that they hold reaches window, as the manifest path does
// (changedFolderDocumentsFromManifest): the one delta Builder holds them all
// until its Finish.
func changedFolderDocuments(selected []folderCandidate, oldChecksums map[string]uint64, markers []int64, window int64) ([]fileContent, []string, error) {
	changedDocs := make([]fileContent, 0, 1)
	changedPaths := make([]string, 0, 1)
	seen := make(map[string]struct{}, len(selected))
	var changedContent int64
	keep := func(doc fileContent) error {
		changedContent += heldContentBytes(doc)
		if changedContent >= window {
			return errDeltaPayloadExceedsWindow
		}
		changedDocs = append(changedDocs, doc)
		return nil
	}

	for i, candidate := range selected {
		seen[candidate.name] = struct{}{}
		doc, err := readFolderDocument(candidate)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		if err != nil {
			if _, ok := oldChecksums[candidate.name]; ok {
				changedPaths = append(changedPaths, candidate.name)
			}
			continue
		}
		markers[i] = nameOnlyMarkerBytes(doc)
		if doc.skipReason != index.SkipReasonNone {
			// Zoekt checksums its marker, not the file, so this path cannot
			// tell whether the name-only document changed. The manifest path
			// can.
			changedPaths = append(changedPaths, candidate.name)
			if err := keep(doc); err != nil {
				return nil, nil, err
			}
			continue
		}
		checksum := crc64.Checksum(doc.content, folderChecksumTable)
		oldChecksum, ok := oldChecksums[candidate.name]
		if !ok || oldChecksum != checksum {
			changedPaths = append(changedPaths, candidate.name)
			if err := keep(doc); err != nil {
				return nil, nil, err
			}
		}
	}

	for name := range oldChecksums {
		if _, ok := seen[name]; !ok {
			changedPaths = append(changedPaths, name)
		}
	}
	return changedDocs, changedPaths, nil
}

type folderManifest struct {
	Version int    `json:"version"`
	State   string `json:"state"`
	// DeadBytes is the content that deltas tombstoned since the last full
	// build (deltaNeedsCompaction).
	DeadBytes int64                 `json:"dead_bytes"`
	Files     []folderManifestEntry `json:"files"`
}

type folderManifestEntry struct {
	Name     string      `json:"name"`
	Mode     os.FileMode `json:"mode"`
	Size     int64       `json:"size"`
	Mtime    int64       `json:"mtime"`
	Ctime    int64       `json:"ctime"`
	Dev      uint64      `json:"dev"`
	Ino      uint64      `json:"ino"`
	Dataless bool        `json:"dataless,omitempty"`
	// Marker is the size of the NOT-INDEXED marker that the shard holds
	// instead of the content, as the read found it (nameOnlyMarkerBytes):
	// content with a NUL byte, of one or two bytes, or unreadable. Zero means
	// the shard holds the content.
	Marker int64 `json:"marker,omitempty"`
}

// heldBytes is the content bytes that the shard document of the file holds:
// the NOT-INDEXED marker for a file kept by name only, else its size.
func (file folderManifestEntry) heldBytes() int64 {
	switch {
	case file.Marker > 0:
		return file.Marker
	case file.Dataless:
		return notIndexedMarkerBytes(seekNoBytesReason)
	case !documentContentFits(file.Size):
		return notIndexedMarkerBytes(index.SkipReasonTooLarge)
	}
	return file.Size
}

func readFolderManifest(cacheDir, expectedState string) (folderManifest, bool) {
	var manifest folderManifest
	if expectedState == "" {
		return manifest, false
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, folderManifestFileName))
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		return folderManifest{}, false
	}
	return manifest, manifest.Version == folderManifestVersion && manifest.State == expectedState
}

// writeFolderManifest writes the manifest of selected, with deadBytes and,
// when set, the marker bytes of each file by index. It stores an
// unreadFileMarker as zero, because a manifest never stores that value.
func writeFolderManifest(cacheDir, state string, selected []folderCandidate, deadBytes int64, markers []int64) error {
	manifest := folderManifest{
		Version:   folderManifestVersion,
		State:     state,
		DeadBytes: deadBytes,
		Files:     make([]folderManifestEntry, len(selected)),
	}
	for i, candidate := range selected {
		manifest.Files[i] = folderManifestEntry{
			Name:     candidate.name,
			Mode:     candidate.mode,
			Size:     candidate.size,
			Mtime:    candidate.mtime,
			Ctime:    candidate.ctime,
			Dev:      candidate.dev,
			Ino:      candidate.ino,
			Dataless: candidate.dataless,
		}
		if markers != nil {
			manifest.Files[i].Marker = max(markers[i], 0)
		}
	}
	path := filepath.Join(cacheDir, folderManifestFileName)
	tmp := path + ".tmp"
	// The encoder writes the encoded manifest to the file in one write. The
	// rename publishes only a complete file.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := json.NewEncoder(f).Encode(manifest); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// changedFolderDocumentsFromManifest compares selected files with the manifest
// and reads only new and changed files. It returns errDeltaPayloadExceedsWindow
// when the stat size of the files whose content it reads reaches window. The
// caller then uses a windowed full rebuild. A file removed after the walk
// gives only its path; another read error stops the comparison.
func changedFolderDocumentsFromManifest(
	selected []folderCandidate,
	manifest folderManifest,
	window int64,
) ([]fileContent, []string, error) {
	// Both slices are sorted by name: the walk sorts the selected files, and
	// the manifest keeps their order.
	changedDocs := make([]fileContent, 0, 1)
	changedPaths := make([]string, 0, 1)
	selectedIndex := 0
	manifestIndex := 0
	var estContent int64

	readCandidate := func(candidate folderCandidate) error {
		// A name-only file reads no content, so it adds nothing to the payload.
		if content := candidate.indexedBytes(); content > 0 {
			estContent += content
			if estContent >= window {
				return errDeltaPayloadExceedsWindow
			}
		}
		changedPaths = append(changedPaths, candidate.name)
		doc, err := readFolderDocument(candidate)
		if err == nil {
			changedDocs = append(changedDocs, doc)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}

	for selectedIndex < len(selected) && manifestIndex < len(manifest.Files) {
		candidate := selected[selectedIndex]
		old := manifest.Files[manifestIndex]
		switch {
		case candidate.name == old.Name:
			if !folderManifestFileMatchesCandidate(old, candidate) {
				if err := readCandidate(candidate); err != nil {
					return nil, nil, err
				}
			}
			selectedIndex++
			manifestIndex++
		case candidate.name < old.Name:
			if err := readCandidate(candidate); err != nil {
				return nil, nil, err
			}
			selectedIndex++
		default:
			changedPaths = append(changedPaths, old.Name)
			manifestIndex++
		}
	}

	for ; selectedIndex < len(selected); selectedIndex++ {
		if err := readCandidate(selected[selectedIndex]); err != nil {
			return nil, nil, err
		}
	}

	for ; manifestIndex < len(manifest.Files); manifestIndex++ {
		changedPaths = append(changedPaths, manifest.Files[manifestIndex].Name)
	}
	return changedDocs, changedPaths, nil
}

func folderManifestFileMatchesCandidate(file folderManifestEntry, candidate folderCandidate) bool {
	return file.Mode == candidate.mode &&
		file.Size == candidate.size &&
		file.Mtime == candidate.mtime &&
		file.Ctime == candidate.ctime &&
		file.Dev == candidate.dev &&
		file.Ino == candidate.ino &&
		file.Dataless == candidate.dataless
}

func newFolderCandidate(name, path string, info os.FileInfo) folderCandidate {
	dev, ino, mtime := fileInfoIdentity(info)
	candidate := folderCandidate{
		name:  name,
		path:  path,
		mode:  info.Mode(),
		size:  info.Size(),
		mtime: mtime,
		dev:   dev,
		ino:   ino,
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		candidate.ctime = statCtimeNano(*st)
		candidate.dataless = statDatalessFlags(uint32(statFlags(*st)))
	}
	return candidate
}

func fileInfoIdentity(info os.FileInfo) (uint64, uint64, int64) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino), statMtimeNano(*st)
	}
	return 0, 0, info.ModTime().UnixNano()
}

func newFolderStateHasher(plan node) *xxhash.Digest {
	h := xxhash.New()
	_, _ = h.WriteString(stateVersion)

	appendFolderFingerprintPart(h, "folder-state-v4")
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
	appendFolderFingerprintPart(h, "root_type")
	appendFolderFingerprintPart(h, string(plan.rootType))

	if id, err := filesystemIdentity(plan.root); err == nil {
		appendFolderFingerprintPart(h, "root_identity")
		appendFolderFingerprintPart(h, id)
	}
	return h
}

func appendFolderCandidateHash(h *xxhash.Digest, c folderCandidate) {
	var numBuf [20]byte
	appendFolderFingerprintPart(h, "file")
	appendFolderFingerprintPart(h, c.name)
	appendFolderFingerprintBytes(h, strconv.AppendUint(numBuf[:0], uint64(c.mode), 10))
	appendFolderFingerprintBytes(h, strconv.AppendInt(numBuf[:0], c.size, 10))
	appendFolderFingerprintBytes(h, strconv.AppendInt(numBuf[:0], c.mtime, 10))
	appendFolderFingerprintBytes(h, strconv.AppendInt(numBuf[:0], c.ctime, 10))
	appendFolderFingerprintBytes(h, strconv.AppendUint(numBuf[:0], c.dev, 10))
	appendFolderFingerprintBytes(h, strconv.AppendUint(numBuf[:0], c.ino, 10))
	appendFolderFingerprintBytes(h, strconv.AppendBool(numBuf[:0], c.dataless))
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

// streamFolderFilesWithMarkers returns a channel that yields file contents for
// each folder candidate. The output channel is unbuffered. In-flight
// memory is bounded by two limits, whichever is tighter:
//   - the worker count (each worker holds at most one fileContent),
//   - readSemaphore's readBudgetBytes weight budget (see caps.go).
//
// Each yielded fileContent carries readWeight of the candidate's pre-read size
// as its readSemaphore weight; a name-only document carries zero. The consumer
// (indexDocuments) releases that weight after Builder.Add, or after Finish for
// a document that fills a shard of its own. A caller must drain the channel or
// cancel ctx.
//
// Synchronous folder-delta reads use readFolderDocument and have zero weight.
// readSemaphore does not limit those reads.
//
// A read error that may pass (folderReadError) calls fail and stops the
// stream: the build then publishes nothing, so the next search reads the file
// again.
//
// When markers is set, it also sets the marker bytes of each sent document by
// index of candidates (nameOnlyMarkerBytes). Each worker writes its own
// indexes, and the channel closes after every worker ends.
func streamFolderFilesWithMarkers(
	ctx context.Context,
	candidates []folderCandidate,
	parallelism int,
	fail context.CancelCauseFunc,
	markers []int64,
) <-chan fileContent {
	workers := fileReadWorkerCount(parallelism, len(candidates))
	out := make(chan fileContent)
	work := make(chan int, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if ctx.Err() != nil {
					continue
				}
				marker := readOneFolderFileStreaming(ctx, candidates[i], out, fail)
				if markers != nil {
					markers[i] = marker
				}
			}
		}()
	}

	go func() {
		for i := range candidates {
			select {
			case work <- i:
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

// readOneFolderFileStreaming reads one candidate (readFolderDocument) and
// sends its document. A file whose content the index reads (indexedBytes)
// takes readSemaphore weight before the read. A document with content keeps
// the weight, and a successful send transfers its release to the consumer. A
// name-only document carries no weight: the reader releases it before the
// send. A read error that may pass calls fail and sends nothing. It returns the
// marker bytes of the document (nameOnlyMarkerBytes), or zero when it sends
// none.
func readOneFolderFileStreaming(
	ctx context.Context,
	candidate folderCandidate,
	out chan<- fileContent,
	fail context.CancelCauseFunc,
) int64 {
	weight := readWeight(candidate.indexedBytes())
	if weight > 0 {
		if err := readSemaphore.Acquire(ctx, weight); err != nil {
			return 0 // ctx cancelled.
		}
	}
	doc, err := readWeightedFolderDocument(candidate, weight)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Removed after the walk: the next search's delta drops it.
		return 0
	case err != nil:
		fail(err)
		return 0
	}
	// The consumer owns the document after the send.
	marker := nameOnlyMarkerBytes(doc)
	select {
	case out <- doc:
		return marker
	case <-ctx.Done():
		readSemaphore.Release(doc.weight)
		return 0
	}
}

// readWeightedFolderDocument reads candidate (readFolderDocument) with its
// readSemaphore weight already taken. A document with content keeps the
// weight; any other outcome releases it.
func readWeightedFolderDocument(candidate folderCandidate, weight int64) (fileContent, error) {
	doc, err := readFolderDocument(candidate)
	if err == nil && doc.skipReason == index.SkipReasonNone {
		doc.weight = weight
		return doc, nil
	}
	readSemaphore.Release(weight)
	return doc, err
}

// streamFolderFilesInOrder is streamFolderFilesWithMarkers that sends the
// documents in the order of candidates, for the vector build: a graph shard is
// reused only when its rows are the same (semanticUSearchShardID), and reads
// that end in another order would give other rows to every shard at each
// build. The reads still run on parallelism workers. The dispatcher takes the
// weight of each file in order, before its read starts, so the earliest
// document that is not sent always holds its weight, and its read can always
// finish: no read waits for the weight of a later document that waits to be
// sent.
func streamFolderFilesInOrder(
	ctx context.Context,
	candidates []folderCandidate,
	parallelism int,
	fail context.CancelCauseFunc,
) <-chan fileContent {
	workers := fileReadWorkerCount(parallelism, len(candidates))
	type readJob struct {
		candidate folderCandidate
		weight    int64
		result    chan fileContent
	}
	out := make(chan fileContent)
	work := make(chan readJob)
	queue := make(chan chan fileContent, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for job := range work {
				doc, err := readWeightedFolderDocument(job.candidate, job.weight)
				switch {
				case errors.Is(err, fs.ErrNotExist):
					// Removed after the walk: the next search's delta drops it.
					doc = fileContent{}
				case err != nil:
					fail(err)
					doc = fileContent{}
				}
				job.result <- doc
			}
		})
	}
	go func() {
		defer close(queue)
		defer func() {
			close(work)
			wg.Wait()
		}()
		for _, candidate := range candidates {
			weight := readWeight(candidate.indexedBytes())
			if weight > 0 {
				if err := readSemaphore.Acquire(ctx, weight); err != nil {
					return // ctx cancelled.
				}
			}
			result := make(chan fileContent, 1)
			select {
			case queue <- result:
			case <-ctx.Done():
				readSemaphore.Release(weight)
				return
			}
			work <- readJob{candidate: candidate, weight: weight, result: result}
		}
	}()
	go func() {
		defer close(out)
		for result := range queue {
			doc := <-result
			if doc.name == "" {
				continue
			}
			select {
			case out <- doc:
			case <-ctx.Done():
				readSemaphore.Release(doc.weight)
				for result := range queue {
					readSemaphore.Release((<-result).weight)
				}
				return
			}
		}
	}()
	return out
}

// readFolderDocument returns the document for one candidate, with no
// readSemaphore weight: its content, or a name-only document when the file is
// cloud-only, too large for the index, unreadable for a lasting reason
// (folderReadIsLasting), or starts with binary content. A file removed after
// the walk returns an fs.ErrNotExist error. A read error that may pass returns
// a folderReadError.
func readFolderDocument(candidate folderCandidate) (fileContent, error) {
	if candidate.dataless {
		return fileContent{name: candidate.name, skipReason: seekNoBytesReason}, nil
	}
	if !documentContentFits(candidate.size) {
		return fileContent{name: candidate.name, skipReason: index.SkipReasonTooLarge}, nil
	}
	content, binary, err := readFolderFile(candidate)
	if errors.Is(err, fs.ErrNotExist) {
		return fileContent{}, err
	}
	if err != nil && !folderReadIsLasting(err) {
		return fileContent{}, &folderReadError{path: candidate.path, err: err}
	}
	if err != nil {
		// An unreadable file stays searchable by name.
		slog.Debug("Indexing an unreadable folder file by name only", "path", candidate.path, "error", err)
		return fileContent{name: candidate.name, skipReason: seekNoBytesReason}, nil
	}
	if binary {
		return fileContent{name: candidate.name, skipReason: index.SkipReasonBinary}, nil
	}
	return fileContent{name: candidate.name, content: content}, nil
}

// folderOpenFile opens a folder file for reading; tests replace it to inject
// read errors.
var folderOpenFile = openWorkTreeFile

var errFolderFileGrew = errors.New("file grew beyond its stat size")

// folderReadError is a read error that may pass (EMFILE, EIO, ESTALE, a
// network timeout): the build stops and publishes nothing.
type folderReadError struct {
	path string
	err  error
}

func (e *folderReadError) Error() string { return fmt.Sprintf("read %s: %v", e.path, e.err) }
func (e *folderReadError) Unwrap() error { return e.err }

// folderReadIsLasting reports a read error that the next search would see
// again: the file stays searchable by name. Permissions heal through a ctime
// change, which the state hash sees. ELOOP comes from the Lstat of the dirty
// reader when a directory of the path is a symlink loop.
func folderReadIsLasting(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, errFolderFileGrew) ||
		errors.Is(err, syscall.ENAMETOOLONG) || errors.Is(err, syscall.ELOOP)
}

// readFolderFile reads one candidate. binary is true, with no content, when the
// file starts with binary content (readContent); the rest is not read. A file
// that shrank returns the bytes that the read found, and a file that grew
// returns errFolderFileGrew. The caller first checks that the size fits the
// index (documentContentFits).
func readFolderFile(candidate folderCandidate) (content []byte, binary bool, err error) {
	file, err := folderOpenFile(candidate.path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()

	content, binary, _, err = readContent(file, candidate.size)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, false, err
	}
	if binary {
		return nil, true, nil
	}

	var extra [1]byte
	extraN, err := file.Read(extra[:])
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	if extraN > 0 {
		return nil, false, errFolderFileGrew
	}
	return content, false, nil
}

func folderRepoName(plan node) string {
	return "folder_" + string(plan.id)
}
