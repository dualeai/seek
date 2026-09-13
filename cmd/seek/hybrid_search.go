package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

const (
	// The proxy search reads more units than the final file limit because one
	// file can own many units. File collapse removes that size advantage before
	// the candidate union.
	hybridSemanticUnitLimit  = rerankCandidateLimit * 8
	hybridBranchQuota        = rerankCandidateLimit / 2
	hybridMissingLexicalRank = rerankCandidateLimit + 1
)

type semanticFileCandidate struct {
	unit       semanticUnit
	proxyScore float32
	result     corpusSearchResult
	document   rerankDocument
}

type hybridSemanticResult struct {
	model semanticModel
	query *semanticQueryEmbedding
	hits  []semanticHit
	err   error
}

// tryHybridSearch runs joined retrieval for one unscoped stable corpus. Git
// input must be a clean committed tree. A folder must contain no nested Git
// corpus. A false used result asks the caller to use lexical retrieval and, for
// an eligible description, model re-ranking without changing fallback output.
func tryHybridSearch(
	ctx context.Context,
	plans []corpusPlan,
	paths *gitPaths,
	strictQ query.Q,
	config searchConfig,
	rerankPlan rerankQueryPlan,
	execution searchExecution,
) (
	results []corpusSearchResult,
	dirty dirtyFilesByCorpus,
	used bool,
	err error,
) {
	if !hybridSearchEligible(plans, execution) {
		return nil, nil, false, nil
	}
	if plans[0].kind == corpusKindFolder {
		return tryHybridFolderSearch(
			ctx,
			plans[0],
			strictQ,
			config,
			rerankPlan,
			execution,
		)
	}
	plan := plans[0]
	planPaths := plan.gitPaths
	if planPaths == nil {
		planPaths = paths
	}
	if planPaths == nil {
		return nil, nil, false, nil
	}

	// Start the only query encoding before index preparation. Both retrieval and
	// final MaxSim wait on this same cached result.
	go func() {
		_, _, _ = execution.model.prepareQuery(ctx, rerankPlan.modelQuery)
	}()

	state, indexState, err := ensureGitCorpusFreshWithExecution(
		ctx,
		&plan,
		*planPaths,
		execution,
	)
	if err != nil {
		return nil, nil, false, err
	}
	if state.HeadSHA == "no-head" || len(state.Files) != 0 ||
		plan.scopedStateHash != "" {
		return nil, nil, false, nil
	}
	if indexState == corpusKnownEmpty {
		touchPlanUsed(plan)
		return nil, nil, true, nil
	}

	stateHash := gitCorpusStateHash(*planPaths, state)
	lockPath := filepath.Join(plan.cacheDir, lockFile)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open joined search lock: %w", err)
	}
	locked := false
	defer func() {
		if locked {
			unlockFile(lock)
		}
		_ = lock.Close()
	}()
	if err := acquireStrictReadLock(ctx, plan.indexDir, lock); err != nil {
		return nil, nil, false, fmt.Errorf("acquire joined search lock: %w", err)
	}
	locked = true

	lockedState, err := gitRepoStateIn(ctx, planPaths.RepoDir)
	if err != nil {
		return nil, nil, false, err
	}
	if lockedState.HeadSHA != state.HeadSHA || len(lockedState.Files) != 0 ||
		gitCorpusStateHash(*planPaths, lockedState) != stateHash {
		return nil, nil, false, errGitIndexStateChanged
	}
	if !joinedGenerationMatches(
		plan.cacheDir,
		plan.indexDir,
		stateHash,
		state.HeadSHA,
	) {
		return nil, nil, false, fmt.Errorf("joined generation does not match the current Git state")
	}
	generation, err := openJoinedSemanticGeneration(
		plan.cacheDir,
		plan.indexDir,
		state.HeadSHA,
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open semantic generation: %w", err)
	}
	defer func() { _ = generation.Close() }()

	strictFiles, relaxedFiles, semantic, err := runHybridBranches(
		ctx,
		plan,
		strictQ,
		config,
		rerankPlan,
		execution,
		generation,
	)
	if err != nil {
		return nil, nil, false, err
	}

	// USearch views and both Zoekt searches have stopped. The loaded row data
	// and the Git commit are immutable, so source checks can run after unlock.
	unlockFile(lock)
	locked = false

	semanticFiles, err := materializeSemanticCandidates(
		ctx,
		plan,
		*planPaths,
		state.HeadSHA,
		generation,
		semantic.hits,
		max(configuredContextLines(config), searchContextLines),
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf("check semantic evidence: %w", err)
	}
	finalState, err := gitRepoStateIn(ctx, planPaths.RepoDir)
	if err != nil {
		return nil, nil, false, err
	}
	if finalState.HeadSHA != state.HeadSHA || len(finalState.Files) != 0 ||
		gitCorpusStateHash(*planPaths, finalState) != stateHash {
		return nil, nil, false, errGitIndexStateChanged
	}

	fused, err := finishHybridSearch(
		ctx,
		plan,
		strictFiles,
		relaxedFiles,
		semanticFiles,
		semantic,
	)
	if err != nil {
		return nil, nil, false, err
	}
	touchPlanUsed(plan)
	return applyRerankDisplayConfig(fused, config), nil, true, nil
}

func tryHybridFolderSearch(
	ctx context.Context,
	plan corpusPlan,
	strictQ query.Q,
	config searchConfig,
	rerankPlan rerankQueryPlan,
	execution searchExecution,
) ([]corpusSearchResult, dirtyFilesByCorpus, bool, error) {
	var foundNestedGit atomic.Bool
	if plan.rootType == rootTypeDirectory {
		// A direct hybrid search has no corpus pool that can own a discovered Git
		// subtree. Exclude each boundary here, then ask the normal pool path to
		// search all corpora when one exists.
		plan.discover = func(gitBoundary) bool {
			foundNestedGit.Store(true)
			return true
		}
	}
	go func() {
		_, _, _ = execution.model.prepareQuery(ctx, rerankPlan.modelQuery)
	}()
	indexState, err := ensureFolderCorpusFreshWithExecution(ctx, plan, execution)
	if err != nil {
		return nil, nil, false, err
	}
	if foundNestedGit.Load() {
		return nil, nil, false, nil
	}
	if indexState == corpusKnownEmpty {
		touchPlanUsed(plan)
		return nil, nil, true, nil
	}
	state, _, err := folderCorpusFingerprint(ctx, plan)
	if err != nil {
		return nil, nil, false, err
	}

	lockPath := filepath.Join(plan.cacheDir, lockFile)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open joined folder search lock: %w", err)
	}
	locked := false
	defer func() {
		if locked {
			unlockFile(lock)
		}
		_ = lock.Close()
	}()
	if err := acquireStrictReadLock(ctx, plan.indexDir, lock); err != nil {
		return nil, nil, false, fmt.Errorf("acquire joined folder search lock: %w", err)
	}
	locked = true

	lockedState, _, err := folderCorpusFingerprint(ctx, plan)
	if err != nil {
		return nil, nil, false, err
	}
	if lockedState != state || readStateFile(plan.cacheDir) != state {
		return nil, nil, false, errGitIndexStateChanged
	}
	if !joinedGenerationMatches(plan.cacheDir, plan.indexDir, state, state) {
		return nil, nil, false, fmt.Errorf("joined generation does not match the current folder state")
	}
	generation, err := openJoinedSemanticGeneration(plan.cacheDir, plan.indexDir, state)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open folder semantic generation: %w", err)
	}
	defer func() { _ = generation.Close() }()
	strictFiles, relaxedFiles, semantic, err := runHybridBranches(
		ctx,
		plan,
		strictQ,
		config,
		rerankPlan,
		execution,
		generation,
	)
	if err != nil {
		return nil, nil, false, err
	}

	unlockFile(lock)
	locked = false
	semanticFiles, err := materializeFolderSemanticCandidates(
		ctx,
		plan,
		generation,
		semantic.hits,
		max(configuredContextLines(config), searchContextLines),
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf("check folder semantic evidence: %w", err)
	}
	finalState, _, err := folderCorpusFingerprint(ctx, plan)
	if err != nil {
		return nil, nil, false, err
	}
	if finalState != state {
		return nil, nil, false, errGitIndexStateChanged
	}
	fused, err := finishHybridSearch(
		ctx,
		plan,
		strictFiles,
		relaxedFiles,
		semanticFiles,
		semantic,
	)
	if err != nil {
		return nil, nil, false, err
	}
	touchPlanUsed(plan)
	return applyRerankDisplayConfig(fused, config), nil, true, nil
}

// runHybridBranches starts strict Zoekt, relaxed Zoekt, and semantic retrieval
// together. Semantic retrieval prepares the shared query once, plans each
// shard, and exact-scores the resulting row union. Any branch error asks the
// caller to use its normal fallback path.
func runHybridBranches(
	ctx context.Context,
	plan corpusPlan,
	strictQ query.Q,
	config searchConfig,
	rerankPlan rerankQueryPlan,
	execution searchExecution,
	generation *semanticGeneration,
) ([]zoekt.FileMatch, []zoekt.FileMatch, hybridSemanticResult, error) {
	modelConfig := rerankModelSearchConfig(config)
	modelConfig.contextGitCorpus = plan.kind == corpusKindGit
	relaxedConfig := rerankCandidateSearchConfig(config)
	relaxedConfig.contextGitCorpus = plan.kind == corpusKindGit

	var strictFiles, relaxedFiles []zoekt.FileMatch
	var strictErr, relaxedErr error
	var semantic hybridSemanticResult
	var wait sync.WaitGroup
	wait.Add(3)
	go func() {
		defer wait.Done()
		strictFiles, strictErr = executeParsedSearchScopedDirs(
			ctx,
			searchIndexDirs(plan),
			strictQ,
			plan.scope,
			modelConfig,
		)
	}()
	go func() {
		defer wait.Done()
		relaxedFiles, relaxedErr = executeParsedSearchScopedDirs(
			ctx,
			searchIndexDirs(plan),
			rerankPlan.relaxedQ,
			plan.scope,
			relaxedConfig,
		)
	}()
	go func() {
		defer wait.Done()
		semantic.model, semantic.query, semantic.err = execution.model.prepareQuery(
			ctx,
			rerankPlan.modelQuery,
		)
		if semantic.err != nil {
			return
		}
		semantic.hits, semantic.err = semanticUSearchFiltered(
			ctx,
			generation,
			semantic.query,
			rerankPlan.semanticFilter,
			hybridSemanticUnitLimit,
		)
	}()
	wait.Wait()
	if strictErr != nil {
		return nil, nil, semantic, fmt.Errorf("joined strict search: %w", strictErr)
	}
	if relaxedErr != nil {
		return nil, nil, semantic, fmt.Errorf("joined relaxed search: %w", relaxedErr)
	}
	if semantic.err != nil {
		return nil, nil, semantic, fmt.Errorf("joined semantic search: %w", semantic.err)
	}
	return strictFiles, relaxedFiles, semantic, nil
}

func finishHybridSearch(
	ctx context.Context,
	plan corpusPlan,
	strictFiles []zoekt.FileMatch,
	relaxedFiles []zoekt.FileMatch,
	semanticFiles []semanticFileCandidate,
	semantic hybridSemanticResult,
) ([]corpusSearchResult, error) {
	strictResults := wrapCorpusResults(plan, strictFiles)
	relaxedResults := wrapCorpusResults(plan, relaxedFiles)
	strictRanked := rankCorpusResultsBM25(strictResults, nil)
	relaxedRanked := rankCorpusResultsBM25(relaxedResults, nil)
	if len(relaxedRanked) > rerankCandidateLimit {
		relaxedRanked = relaxedRanked[:rerankCandidateLimit]
	}
	candidates := buildHybridRerankCandidates(strictRanked, relaxedRanked, semanticFiles)
	if len(candidates) == 0 {
		return nil, errRerankNotUseful
	}
	scores, err := semantic.model.ScoresWithSemanticQuery(
		ctx,
		semantic.query,
		rerankCandidateDocuments(candidates),
	)
	if err != nil {
		return nil, fmt.Errorf("score joined candidates: %w", err)
	}
	return fuseRerankCandidates(candidates, scores, strictRanked, relaxedRanked)
}

func hybridSearchEligible(plans []corpusPlan, execution searchExecution) bool {
	if len(plans) != 1 ||
		(plans[0].kind != corpusKindGit && plans[0].kind != corpusKindFolder) ||
		plans[0].scope != nil || plans[0].dirtyScope != nil ||
		execution.model == nil {
		return false
	}
	return execution.policy.semanticEnabled()
}

func configuredContextLines(config searchConfig) int {
	if config.opts == nil {
		return 0
	}
	return max(0, config.opts.NumContextLines)
}

func materializeSemanticCandidates(
	ctx context.Context,
	plan corpusPlan,
	paths gitPaths,
	head string,
	generation *semanticGeneration,
	hits []semanticHit,
	contextLines int,
) ([]semanticFileCandidate, error) {
	candidates, err := selectSemanticFileCandidates(generation, hits)
	if err != nil || len(candidates) == 0 {
		return candidates, err
	}

	args := []string{"ls-tree", "-r", "-z", "--full-tree", "--no-abbrev", head, "--"}
	for _, candidate := range candidates {
		args = append(args, candidate.unit.path)
	}
	entriesByPath := make(map[string]gitTreeEntry, len(candidates))
	err = readNativeGitTreeRecords(ctx, paths.RepoDir, args, func(entries []gitTreeEntry) error {
		for _, entry := range entries {
			if _, duplicate := entriesByPath[entry.path]; duplicate {
				return fmt.Errorf("git returned duplicate path %q", entry.path)
			}
			entriesByPath[entry.path] = entry
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	entries := make([]gitTreeEntry, len(candidates))
	for index, candidate := range candidates {
		entry, ok := entriesByPath[candidate.unit.path]
		if !ok || entry.mode == "160000" {
			return nil, fmt.Errorf("git source %q is missing or is not a blob", candidate.unit.path)
		}
		entries[index] = entry
	}

	var infos []gitBlobInfo
	if err := checkNativeGitBlobs(ctx, paths.RepoDir, entries, func(chunk []gitBlobInfo) error {
		infos = append(infos, chunk...)
		return nil
	}); err != nil {
		return nil, err
	}
	byPath := make(map[string]int, len(candidates))
	for index, candidate := range candidates {
		byPath[candidate.unit.path] = index
	}
	checked := make([]bool, len(candidates))
	if err := readNativeGitBlobs(ctx, paths.RepoDir, infos, func(content fileContent) error {
		index, ok := byPath[content.name]
		if !ok || checked[index] || len(content.content) == 0 {
			return fmt.Errorf("git returned invalid semantic source %q", content.name)
		}
		unit, document, materializeErr := checkedSemanticCandidate(
			candidates[index].unit,
			content.content,
		)
		if materializeErr != nil {
			return fmt.Errorf("semantic source %q: %w", content.name, materializeErr)
		}
		candidates[index].document = document
		candidates[index].result = wrapCorpusResults(plan, []zoekt.FileMatch{
			semanticEvidenceFileMatch(fallbackGitRepositoryName(paths.RepoDir), unit, content.content,
				candidates[index].proxyScore, contextLines),
		})[0]
		checked[index] = true
		readSemaphore.Release(content.weight)
		return nil
	}); err != nil {
		return nil, err
	}
	for index, ok := range checked {
		if !ok {
			return nil, fmt.Errorf("semantic source %q was not read", candidates[index].unit.path)
		}
	}
	return candidates, nil
}

func materializeFolderSemanticCandidates(
	ctx context.Context,
	plan corpusPlan,
	generation *semanticGeneration,
	hits []semanticHit,
	contextLines int,
) ([]semanticFileCandidate, error) {
	candidates, err := selectSemanticFileCandidates(generation, hits)
	if err != nil || len(candidates) == 0 {
		return candidates, err
	}
	for index := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unit := candidates[index].unit
		localName := filepath.FromSlash(unit.path)
		if !filepath.IsLocal(localName) {
			return nil, fmt.Errorf("semantic folder path %q is not local", unit.path)
		}
		path := filepath.Join(plan.root, localName)
		if plan.rootType == rootTypeFile {
			if unit.path != filepath.Base(plan.root) {
				return nil, fmt.Errorf("semantic file path %q does not match its corpus", unit.path)
			}
			path = plan.root
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxIndexedDocumentBytes {
			return nil, fmt.Errorf("semantic folder source %q is unavailable", unit.path)
		}
		content, err := readFolderFile(folderCandidate{path: path, size: info.Size()})
		if err != nil {
			return nil, fmt.Errorf("read semantic folder source %q: %w", unit.path, err)
		}
		unit, document, materializeErr := checkedSemanticCandidate(unit, content)
		if materializeErr != nil {
			return nil, fmt.Errorf("semantic folder source %q: %w", unit.path, materializeErr)
		}
		candidates[index].document = document
		candidates[index].result = wrapCorpusResults(plan, []zoekt.FileMatch{
			semanticEvidenceFileMatch(
				folderRepoName(plan),
				unit,
				content,
				candidates[index].proxyScore,
				contextLines,
			),
		})[0]
	}
	return candidates, nil
}

// checkedSemanticCandidate verifies that current source bytes still match the
// stored row and content identities before those bytes reach display or final
// model scoring.
func checkedSemanticCandidate(
	unit semanticUnit,
	content []byte,
) (semanticUnit, rerankDocument, error) {
	if unit.end > uint64(len(content)) || unit.start >= unit.end ||
		semanticContentID(sha256.Sum256(content)) != unit.contentID {
		return semanticUnit{}, rerankDocument{}, fmt.Errorf("content does not match semantic row")
	}
	unit.text = content[int(unit.start):int(unit.end)]
	if makeSemanticUnitID(unit) != unit.id {
		return semanticUnit{}, rerankDocument{}, fmt.Errorf("unit identity does not match semantic row")
	}
	text := string(bytes.ToValidUTF8(unit.text, []byte("\uFFFD")))
	return unit, rerankDocument{
		Path:     unit.path,
		Language: unit.language,
		Symbol:   unit.symbol,
		Text:     text,
		matchEnd: len(text),
	}, nil
}

func selectSemanticFileCandidates(
	generation *semanticGeneration,
	hits []semanticHit,
) ([]semanticFileCandidate, error) {
	if generation == nil {
		return nil, fmt.Errorf("semantic generation is nil")
	}
	candidates := make([]semanticFileCandidate, 0, rerankCandidateLimit)
	seenPaths := make(map[string]struct{}, rerankCandidateLimit)
	for _, hit := range hits {
		if hit.row >= uint64(len(generation.rows)) {
			return nil, fmt.Errorf("semantic row %d is out of range", hit.row)
		}
		unit := generation.rows[hit.row]
		if unit.row != hit.row || unit.path == "" {
			return nil, fmt.Errorf("semantic row %d has invalid identity", hit.row)
		}
		if _, exists := seenPaths[unit.path]; exists {
			continue
		}
		seenPaths[unit.path] = struct{}{}
		candidates = append(candidates, semanticFileCandidate{
			unit:       unit,
			proxyScore: hit.score,
		})
		if len(candidates) == rerankCandidateLimit {
			break
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	return candidates, nil
}

func semanticEvidenceFileMatch(
	repository string,
	unit semanticUnit,
	content []byte,
	score float32,
	contextLines int,
) zoekt.FileMatch {
	offset := int(unit.start)
	lineStart := bytes.LastIndexByte(content[:offset], '\n') + 1
	lineEnd := len(content)
	if relative := bytes.IndexByte(content[offset:], '\n'); relative >= 0 {
		lineEnd = offset + relative + 1
	}
	lineNumber := bytes.Count(content[:lineStart], []byte{'\n'}) + 1
	return zoekt.FileMatch{
		FileName:   unit.path,
		Repository: repository,
		Language:   unit.language,
		Score:      float64(score),
		LineMatches: []zoekt.LineMatch{{
			Line:       append([]byte(nil), content[lineStart:lineEnd]...),
			LineNumber: lineNumber,
			Before:     semanticBeforeContext(content, lineStart, contextLines),
			After:      semanticAfterContext(content, lineEnd, contextLines),
		}},
	}
}

func semanticBeforeContext(content []byte, end, lines int) []byte {
	if lines <= 0 || end <= 0 {
		return nil
	}
	start := end
	for range lines {
		if start <= 0 {
			break
		}
		searchEnd := start
		if content[searchEnd-1] == '\n' {
			searchEnd--
		}
		previous := bytes.LastIndexByte(content[:searchEnd], '\n')
		start = previous + 1
	}
	return append([]byte(nil), content[start:end]...)
}

func semanticAfterContext(content []byte, start, lines int) []byte {
	if lines <= 0 || start >= len(content) {
		return nil
	}
	end := start
	for range lines {
		if end >= len(content) {
			break
		}
		relative := bytes.IndexByte(content[end:], '\n')
		if relative < 0 {
			end = len(content)
			break
		}
		end += relative + 1
	}
	return append([]byte(nil), content[start:end]...)
}

// buildHybridRerankCandidates reserves up to half of the final candidate set
// for each retrieval branch, removes duplicate corpus files, then alternates
// remaining lexical and semantic candidates. For an overlap, it keeps the
// lexical display result and rank but scores the checked semantic document.
func buildHybridRerankCandidates(
	strictRanked []corpusSearchResult,
	relaxedRanked []corpusSearchResult,
	semantic []semanticFileCandidate,
) []rerankCandidate {
	lexical := buildRerankCandidates(strictRanked, relaxedRanked)
	candidates := make([]rerankCandidate, 0, rerankCandidateLimit)
	positions := make(map[corpusResultKey]int, rerankCandidateLimit)
	add := func(candidate rerankCandidate, semanticEvidence bool) {
		key := corpusResultKey{
			corpusID: candidate.result.corpusID,
			fileName: candidate.result.file.FileName,
		}
		if position, exists := positions[key]; exists {
			current := &candidates[position]
			if candidate.lexicalRank < current.lexicalRank {
				current.result = candidate.result
				current.lexicalRank = candidate.lexicalRank
				current.preservedStrictLeader = candidate.preservedStrictLeader
			}
			if semanticEvidence {
				current.document = candidate.document
				current.hasDocument = true
				current.preservedStrictLeader = false
			}
			return
		}
		if len(candidates) >= rerankCandidateLimit {
			return
		}
		positions[key] = len(candidates)
		candidates = append(candidates, candidate)
	}
	semanticCandidate := func(candidate semanticFileCandidate) rerankCandidate {
		return rerankCandidate{
			result:      candidate.result,
			lexicalRank: hybridMissingLexicalRank,
			document:    candidate.document,
			hasDocument: true,
		}
	}

	lexicalProtected := min(hybridBranchQuota, len(lexical))
	for _, candidate := range lexical[:lexicalProtected] {
		add(candidate, false)
	}
	semanticProtected := min(hybridBranchQuota, len(semantic))
	for _, candidate := range semantic[:semanticProtected] {
		add(semanticCandidate(candidate), true)
	}
	lexicalAt, semanticAt := lexicalProtected, semanticProtected
	for len(candidates) < rerankCandidateLimit &&
		(lexicalAt < len(lexical) || semanticAt < len(semantic)) {
		if lexicalAt < len(lexical) {
			add(lexical[lexicalAt], false)
			lexicalAt++
		}
		if semanticAt < len(semantic) && len(candidates) < rerankCandidateLimit {
			add(semanticCandidate(semantic[semanticAt]), true)
			semanticAt++
		}
	}
	return candidates
}
