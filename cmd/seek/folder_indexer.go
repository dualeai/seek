package main

import (
	"bufio"
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
	// Folder deltas are usually tiny. A smaller shard target reduces Zoekt's
	// per-builder map sizing without changing the shard compatibility hash.
	folderDeltaShardMax    = 1024 * 1024
	maxFolderDeltaShards   = 64
	folderManifestVersion  = 1
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

// indexedBytes is the content that the index reads for the file: none for a
// file indexed by name only.
func (c folderCandidate) indexedBytes() int64 {
	if c.dataless || !documentContentFits(c.size) {
		return 0
	}
	return c.size
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
// branches build concurrently. The joined descriptor is activated last under
// the publish lock. A semantic failure leaves the Zoekt family searchable.
func ensureFolderCorpusFreshWithExecution(
	ctx context.Context,
	plan corpusPlan,
	execution searchExecution,
) (corpusIndexState, error) {
	// A folder corpus has no commit, so the head test does not apply.
	semanticRequired := execution.buildsVectors("")
	cachedState := readStateFile(plan.cacheDir)
	// One scan checks both shard presence and manifest agreement. Presence alone
	// accepts any one surviving shard; the manifest detects missing or changed
	// family members.
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

	// A leftover .swapping marker means a prior publish was interrupted and the
	// shards may be torn; skip the warm fast path so the build lock + recovery
	// run below.
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
			if hasShards && (!semanticRequired || joinedGenerationMatches(
				plan.cacheDir,
				plan.indexDir,
				stateHash,
				stateHash,
			)) {
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
	buildFd, acquired, err := acquireBuildLock(ctx, plan.cacheDir, plan.indexDir)
	if err != nil {
		return corpusSearchable, folderCorpusError(plan, err)
	}
	if !acquired {
		// Another builder active and a usable index exists — serve current.
		slog.Debug("Another process is indexing; serving current index")
		return corpusSearchable, nil
	}
	defer releaseLock(buildFd)
	defer func() {
		_ = os.Remove(filepath.Join(plan.cacheDir, stateTmpFile))
	}()

	// Repair a crash that left a half-published swap before trusting state.
	recoverIncompleteSwap(plan.cacheDir, plan.indexDir)

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
		// Re-scan post-lock so a concurrent indexer's freshly published
		// family is picked up; the pre-lock scan may be stale by now. Same
		// gate as the warm path — presence alone would accept one survivor of
		// a torn family.
		if (hasShards || folderFamilyUsable(plan.indexDir)) &&
			(!semanticRequired || joinedGenerationMatches(
				plan.cacheDir,
				plan.indexDir,
				stateHash,
				stateHash,
			)) {
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
		if hasShards && (!semanticRequired || joinedGenerationMatches(
			plan.cacheDir,
			plan.indexDir,
			stateHash,
			stateHash,
		)) {
			return corpusSearchable, nil
		}
	}

	repoName := folderRepoName(plan)
	// Start a delta build only from the complete family recorded by the last
	// publish. A gap, changed size, or missing sidecar requires a full build. A
	// full build starts in an empty temporary directory.
	sc, scErr := scanFamily(plan.indexDir)
	if scErr != nil {
		return corpusSearchable, folderCorpusError(plan, scErr)
	}
	intact := sc.matchesManifest(plan.indexDir)
	if sc.hasShard() && !intact {
		slog.Warn("Folder shard family does not match its manifest; rebuilding it in full",
			"index_dir", plan.indexDir)
	}
	// The published family is the one this scan describes. Every failure below
	// happens before the publish at the end of this function, so a failure
	// cannot have changed it. Keeping .state in that case keeps the delta base:
	// .state alone decides whether the next build can be a delta, so deleting
	// it turns every interrupted build into a full rebuild of a family that is
	// still correct on disk.
	//
	// This reuses the manifest result already in `intact` rather than calling
	// familyComplete, which would read the manifest file a second time.
	publishedFamilyUsable := sc.hasShard() && intact
	// dropStateUnlessPublished clears the cached state only when no usable
	// family survives. Every caller below sits before the publish.
	dropStateUnlessPublished := func() {
		if !publishedFamilyUsable {
			deleteStateFiles(plan.cacheDir)
		}
	}
	currentState := readStateFile(plan.cacheDir)
	needLexical := currentState != stateHash || !sc.hasShard() || !intact
	isDelta := needLexical && currentState != "" && publishedFamilyUsable
	semanticSource := stateHash
	semanticPresent := semanticRequired && semanticGenerationPresent(plan.indexDir, semanticSource)
	needSemantic := semanticRequired && !semanticPresent

	// Build in a temporary directory, then publish under the lock. A delta build
	// needs the prior shards, so seed them with hardlinks; a full build ignores
	// them.
	var buildDir string
	if needLexical || needSemantic {
		buildDir, err = newBuildDir(plan.indexDir)
		if err != nil {
			return corpusSearchable, folderCorpusError(plan, err)
		}
		defer discardBuildDir(buildDir)
	}
	if isDelta {
		if err := seedFamilyFiles(buildDir, sc.paths(familyAll)); err != nil {
			return corpusSearchable, folderCorpusError(plan, err)
		}
	}
	bp := plan
	bp.indexDir = buildDir // Shards go here; folder state stays in plan.cacheDir.

	indexedAny := !needLexical && selectedCount > 0
	semanticBuilt := false
	semanticStaging := filepath.Join(buildDir, "semantic")
	var lexicalErr, semanticErr error
	if (needLexical || needSemantic) && selectedCount > 0 {
		if err := checkCtagsCached(); err != nil {
			dropStateUnlessPublished()
			return corpusSearchable, folderCorpusError(plan, err)
		}
	}
	var lexicalBuild func()
	if needLexical && selectedCount > 0 {
		lexicalBuild = func() {
			indexedAny, lexicalErr = indexFolderDocuments(
				ctx,
				bp,
				repoName,
				selected,
				execution.resources.cpuLimit(),
				isDelta,
				currentState,
			)
		}
	}
	var semanticBuild func()
	if needSemantic && selectedCount > 0 {
		semanticBuild = func() {
			semanticErr = runSemanticBuild(ctx, execution.model, func(embedder semanticModel) error {
				_, err := buildSemanticFolderGeneration(
					ctx,
					selected,
					plan.indexDir,
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
		slog.Debug(
			"Semantic folder index build failed; keeping lexical search",
			"provider", semanticProviderName(),
			"error", semanticErr,
		)
		if errors.Is(semanticErr, errDegenerateSemanticVector) {
			rejectLateOnAcceleratedProvider(semanticErr.Error())
		}
	}

	// The index holds the bytes each file had when the build read it, under the
	// state of the walk. A file that changed after the walk is a change for the
	// next search: its stat no longer matches, so the next delta reads it again.

	activateSemantic := func() {
		removeJoinedGeneration(plan.cacheDir)
		if !semanticRequired {
			return
		}
		var activationErr error
		if semanticBuilt {
			activationErr = publishAndBindSemanticGeneration(
				plan.cacheDir, plan.indexDir, stateHash, semanticSource, semanticStaging, false,
			)
		} else if semanticPresent {
			activationErr = bindSemanticGeneration(
				plan.cacheDir, plan.indexDir, stateHash, semanticSource,
			)
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

	if needLexical {
		// Publish the shard family and state while readers are excluded by the
		// publish lock. The joined descriptor is the last activation record.
		if err := publishGeneration(ctx, plan.cacheDir, plan.indexDir, buildDir, familyAll, func() error {
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
	} else if semanticRequired && (needSemantic || !joinedGenerationMatches(
		plan.cacheDir,
		plan.indexDir,
		stateHash,
		semanticSource,
	)) {
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

	if semanticErr != nil && !needLexical {
		return corpusSearchable, nil
	}

	// Only a later build reads the folder state manifest. Searchers do not read
	// it, so it can be written after the publish lock is released.
	if selectedCount > 0 {
		if err := writeFolderManifest(plan.cacheDir, stateHash, selected); err != nil {
			slog.Debug("Failed to write folder manifest", "error", err)
		}
	}
	if selectedCount == 0 || !indexedAny {
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

func folderCorpusError(plan corpusPlan, err error) error {
	if err == nil {
		return nil
	}
	return &folderCorpusContextError{
		root:     plan.root,
		cacheDir: plan.cacheDir,
		indexDir: plan.indexDir,
		cause:    err,
	}
}

// indexFolderDocuments indexes selected into plan.indexDir: a delta on the
// prior shards when tryDelta holds and the family has at most
// maxFolderDeltaShards shards, else a full build. A read error that may pass
// fails the build with a folderReadError.
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
			indexedAny, err := indexFolderDocumentsDelta(ctx, plan, repoName, selected, cachedState)
			if err == nil {
				return indexedAny, nil
			}
			// Distinguish "delta too large → route to windowed rebuild"
			// from "Zoekt failed mid-Add" for log clarity.
			if errors.Is(err, errDeltaPayloadExceedsWindow) {
				slog.Debug("Folder delta payload exceeds window threshold; full rebuild", "error", err)
			} else {
				slog.Debug("Folder delta indexing failed, falling back to full rebuild", "error", err)
			}
		} else if shardCount > maxFolderDeltaShards {
			slog.Debug("Folder delta shard limit reached, falling back to full rebuild", "shards", shardCount)
		}
	}
	readCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	fileCh := streamFolderFiles(readCtx, selected, parallelism, cancel)
	indexed, err := indexDocuments(readCtx, plan.indexDir, repoName, plan.root, fileCh, parallelism)
	if readErr, ok := errors.AsType[*folderReadError](context.Cause(readCtx)); ok {
		return false, readErr
	}
	return indexed, err
}

func indexFolderDocumentsDelta(
	ctx context.Context,
	plan corpusPlan,
	repoName string,
	selected []folderCandidate,
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
	// On the manifest path, changedFolderDocumentsFromManifest stops before
	// it reads the file that brings the changed size to the build's window,
	// so the payload fits the single-builder bulk-Release contract of
	// indexDeltaDocuments.
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
		return changedFolderDocumentsFromManifest(selected, manifest, indexWindow(ctx))
	}

	oldChecksums, err := enumerateIndexedFolderChecksums(ctx, plan.indexDir, repoName)
	if err != nil {
		return nil, nil, err
	}
	return changedFolderDocuments(selected, oldChecksums)
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

	// Every document: 0 would become Zoekt's default of 100,000 (SetDefaults).
	opts := zoekt.SearchOptions{ShardMaxMatchCount: math.MaxInt}
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

// changedFolderDocuments finds the changed files by content checksum, for a
// cached state with no matching manifest. It reads every selected file.
func changedFolderDocuments(selected []folderCandidate, oldChecksums map[string]uint64) ([]fileContent, []string, error) {
	changedDocs := make([]fileContent, 0, 1)
	changedPaths := make([]string, 0, 1)
	seen := make(map[string]struct{}, len(selected))

	for _, candidate := range selected {
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
		if doc.skipReason != index.SkipReasonNone {
			// Zoekt checksums its marker, not the file, so this path cannot tell
			// whether the name-only document changed. The manifest path can.
			changedPaths = append(changedPaths, candidate.name)
			changedDocs = append(changedDocs, doc)
			continue
		}
		checksum := crc64.Checksum(doc.content, folderChecksumTable)
		oldChecksum, ok := oldChecksums[candidate.name]
		if !ok || oldChecksum != checksum {
			changedPaths = append(changedPaths, candidate.name)
			changedDocs = append(changedDocs, doc)
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
	Version int                   `json:"version"`
	State   string                `json:"state"`
	Files   []folderManifestEntry `json:"files"`
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

func writeFolderManifest(cacheDir, state string, selected []folderCandidate) error {
	manifest := folderManifest{
		Version: folderManifestVersion,
		State:   state,
		Files:   make([]folderManifestEntry, len(selected)),
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
	}
	path := filepath.Join(cacheDir, folderManifestFileName)
	tmp := path + ".tmp"
	// Stream entries so the full encoded manifest is not held in memory.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	bw := bufio.NewWriter(f)
	if err := json.NewEncoder(bw).Encode(manifest); err != nil {
		_ = f.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// changedFolderDocumentsFromManifest compares selected files with the manifest
// and reads only changed files. It returns errDeltaPayloadExceedsWindow when
// their cumulative stat size reaches window. The caller then uses a windowed
// full rebuild.
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

func newFolderStateHasher(plan corpusPlan) *xxhash.Digest {
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

// streamFolderFiles returns a channel that yields file contents for
// each folder candidate. The output channel is unbuffered. In-flight
// memory is bounded by two limits, whichever is tighter:
//   - the worker count (each worker holds at most one fileContent),
//   - readSemaphore's readBudgetBytes weight budget (see caps.go).
//
// Each yielded fileContent carries readWeight of the candidate's pre-read size
// as its readSemaphore weight; a name-only document carries zero. The consumer
// releases that weight after Builder.Finish stops using the content. A caller
// must drain the channel or cancel ctx.
//
// Synchronous folder-delta reads use readFolderDocument and have zero weight.
// readSemaphore does not limit those reads.
//
// A read error that may pass (folderReadError) calls fail and stops the
// stream: the build then publishes nothing, so the next search reads the file
// again.
func streamFolderFiles(
	ctx context.Context,
	candidates []folderCandidate,
	parallelism int,
	fail context.CancelCauseFunc,
) <-chan fileContent {
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
				readOneFolderFileStreaming(ctx, candidate, out, fail)
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

// readOneFolderFileStreaming acquires readSemaphore weight for one candidate.
// A successful send transfers release ownership to the consumer. On all other
// paths, this function releases the weight. A cloud-only file, a file whose
// content does not fit the index, an unreadable file, and a file that starts
// with binary content (readContent) are sent by name only, with no weight. A
// read error that may pass calls fail and sends nothing.
func readOneFolderFileStreaming(
	ctx context.Context,
	candidate folderCandidate,
	out chan<- fileContent,
	fail context.CancelCauseFunc,
) {
	size := candidate.size
	if candidate.dataless {
		// A cloud-only file is indexed by name, with no content.
		sendNameOnly(ctx, out, candidate.name, seekNoBytesReason)
		return
	}
	if !documentContentFits(size) {
		sendNameOnly(ctx, out, candidate.name, index.SkipReasonTooLarge)
		return
	}
	weight := readWeight(size)
	if err := readSemaphore.Acquire(ctx, weight); err != nil {
		return // ctx cancelled.
	}
	released := false
	defer func() {
		if !released {
			readSemaphore.Release(weight)
		}
	}()

	content, binary, err := readFolderFile(candidate)
	if errors.Is(err, fs.ErrNotExist) {
		// Removed after the walk: the next search's delta drops it.
		return
	}
	if err != nil && !folderReadIsLasting(err) {
		if fail != nil {
			fail(&folderReadError{path: candidate.path, err: err})
		}
		return
	}
	if err != nil {
		// An unreadable file (permissions, a path too long to open) stays
		// searchable by name.
		slog.Debug("Indexing an unreadable folder file by name only", "path", candidate.path, "error", err)
		select {
		case out <- fileContent{name: candidate.name, skipReason: seekNoBytesReason}:
		case <-ctx.Done():
		}
		return
	}
	if binary {
		// No content to hold: the deferred release returns the weight.
		sendNameOnly(ctx, out, candidate.name, index.SkipReasonBinary)
		return
	}
	select {
	case out <- fileContent{name: candidate.name, content: content, weight: weight}:
		released = true
	case <-ctx.Done():
		return
	}
}

// readFolderDocument returns the document for one candidate, with no
// readSemaphore weight: its content, or a name-only document when the file is
// cloud-only, too large for the index, unreadable, or starts with binary
// content. A read error that may pass returns a folderReadError.
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
var folderOpenFile = os.Open

var errFolderFileGrew = errors.New("file grew beyond max size")

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
// change, which the state hash sees.
func folderReadIsLasting(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, errFolderFileGrew) ||
		errors.Is(err, syscall.ENAMETOOLONG) || errors.Is(err, syscall.ELOOP) ||
		errors.Is(err, syscall.EISDIR)
}

// readFolderFile reads one candidate. binary is true, with no content, when the
// file starts with binary content (readContent); the rest is not read.
func readFolderFile(candidate folderCandidate) (content []byte, binary bool, err error) {
	if !documentContentFits(candidate.size) {
		return nil, false, fmt.Errorf("file exceeds max size")
	}
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

func folderRepoName(plan corpusPlan) string {
	return "folder_" + string(plan.id)
}
