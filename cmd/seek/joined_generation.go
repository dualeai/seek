package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	joinedGenerationFile     = ".joined-v1"
	joinedGenerationVersion  = 2
	joinedGenerationMaxBytes = 16 << 10
)

// joinedGenerationDescriptor binds a Zoekt state to its base semantic
// generation: the commit of a Git node, the folderSemanticSource of a folder.
// For a dirty Git node, it also binds the overlay of its dirty files. A Git
// node with no commit has an overlay and no base.
type joinedGenerationDescriptor struct {
	Format      uint32 `json:"format"`
	State       string `json:"state"`
	FamilySHA   string `json:"family_sha256"`
	SemanticKey string `json:"semantic_key,omitempty"`
	OverlayKey  string `json:"overlay_key,omitempty"`
}

// semanticOverlaySource names the overlay generation of a Git node state. The
// state hash covers the dirty paths and their stats, so a saved file gives a
// new overlay.
func semanticOverlaySource(state string) string {
	return semanticOverlayPrefix + state
}

const semanticOverlayPrefix = "overlay:"

// semanticSourceHasGraph reports whether a generation of source holds USearch
// shards: an overlay holds none.
func semanticSourceHasGraph(source string) bool {
	return !strings.HasPrefix(source, semanticOverlayPrefix)
}

// joinedKey is the generation key of source, or none for no generation.
func joinedKey(source string) string {
	if source == "" {
		return ""
	}
	return semanticGenerationKey(source)
}

// runJoinedIndexBuilds starts each required index branch before it waits for
// either branch. A nil function disables that branch for an already-current
// index or for --lexical-only.
func runJoinedIndexBuilds(lexical, semantic func()) {
	var builds sync.WaitGroup
	for _, build := range []func(){lexical, semantic} {
		if build != nil {
			builds.Go(build)
		}
	}
	builds.Wait()
}

func readJoinedGeneration(cacheDir string) (joinedGenerationDescriptor, error) {
	path := filepath.Join(cacheDir, joinedGenerationFile)
	file, err := openRegularSemanticFile(path)
	if err != nil {
		return joinedGenerationDescriptor{}, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, joinedGenerationMaxBytes+1))
	if err != nil {
		return joinedGenerationDescriptor{}, err
	}
	if len(content) > joinedGenerationMaxBytes {
		return joinedGenerationDescriptor{}, fmt.Errorf("joined generation descriptor is too large")
	}
	if err := validateJSONFields(json.NewDecoder(bytes.NewReader(content))); err != nil {
		return joinedGenerationDescriptor{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var descriptor joinedGenerationDescriptor
	if err := decoder.Decode(&descriptor); err != nil {
		return joinedGenerationDescriptor{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return joinedGenerationDescriptor{}, err
	}
	if descriptor.Format != joinedGenerationVersion || descriptor.State == "" ||
		(descriptor.SemanticKey == "" && descriptor.OverlayKey == "") {
		return joinedGenerationDescriptor{}, fmt.Errorf("joined generation descriptor is incomplete")
	}
	if digest, err := hex.DecodeString(descriptor.FamilySHA); err != nil || len(digest) != sha256.Size {
		return joinedGenerationDescriptor{}, fmt.Errorf("joined generation family digest is invalid")
	}
	return descriptor, nil
}

// writeJoinedGenerations writes the descriptor that binds state to the source
// and overlay generations; an empty source or overlay binds none.
func (store corpusStore) writeJoinedGenerations(
	state string,
	source string,
	overlay string,
) error {
	familySHA, err := joinedFamilyDigest(store.indexDir)
	if err != nil {
		return err
	}
	descriptor := joinedGenerationDescriptor{
		Format:      joinedGenerationVersion,
		State:       state,
		FamilySHA:   familySHA,
		SemanticKey: joinedKey(source),
		OverlayKey:  joinedKey(overlay),
	}
	content, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	content = append(content, '\n')
	if len(content) > joinedGenerationMaxBytes {
		return fmt.Errorf("joined generation descriptor is too large")
	}
	return writeCacheFile(store.cacheDir, joinedGenerationFile, string(content))
}

// joinedGenerationsMatch is the fast activation check for a joined index: the
// base generation and the overlay that a dirty Git node binds. An empty source
// or overlay means that the node binds none. It checks the descriptor, the
// Zoekt family-manifest digest, and the manifest of each bound generation with
// its USearch shard list. It does not open the row, vector, or USearch shard
// files. The open for search (openSemanticGenerationForSearch) checks the row
// and vector data, and checks each graph before its first native use.
func (store corpusStore) joinedGenerationsMatch(
	state string,
	source string,
	overlay string,
) bool {
	descriptor, err := readJoinedGeneration(store.cacheDir)
	if err != nil || descriptor.State != state ||
		descriptor.SemanticKey != joinedKey(source) || descriptor.OverlayKey != joinedKey(overlay) {
		return false
	}
	familySHA, err := joinedFamilyDigest(store.indexDir)
	if err != nil || familySHA != descriptor.FamilySHA {
		return false
	}
	for _, bound := range []string{source, overlay} {
		if bound == "" {
			continue
		}
		if _, err := readSemanticManifest(semanticGenerationDir(store.indexDir, bound), bound); err != nil {
			return false
		}
	}
	return true
}

func joinedFamilyDigest(indexDir string) (string, error) {
	file, err := openRegularSemanticFile(filepath.Join(indexDir, familyManifestFile))
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// publishSemanticGeneration moves the build in stagingDir to the generation
// of source and checks it. A valid generation of source stays, and the build
// is discarded, unless replace is set. A generation that holds a name-only
// file whose blob arrived is valid but stale. The build after that arrival
// sets replace.
func publishSemanticGeneration(indexDir, stagingDir, source string, replace bool) error {
	finalDir := semanticGenerationDir(indexDir, source)
	if err := validateSemanticGeneration(indexDir, source); err == nil && !replace {
		discardBuildDir(stagingDir)
		return nil
	}
	if err := os.RemoveAll(finalDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stagingDir, finalDir); err != nil {
		return fmt.Errorf("publish semantic generation: %w", err)
	}
	if err := validateSemanticGeneration(indexDir, source); err != nil {
		return fmt.Errorf("validate published semantic generation: %w", err)
	}
	return nil
}

func semanticGenerationPresent(indexDir, source string) bool {
	return validateSemanticGeneration(indexDir, source) == nil
}

// validateSemanticGeneration opens a generation the way a search does. The
// build hashed each artifact while it wrote it. The checks recorded then let
// the open trust the files that the build wrote.
func validateSemanticGeneration(indexDir, source string) error {
	generation, err := openSemanticGenerationForSearch(semanticGenerationDir(indexDir, source), source)
	if err != nil {
		return err
	}
	return generation.Close()
}

// openJoinedSemanticGeneration opens the source generation. The caller holds
// the node's read lock and has matched the joined descriptor; this function
// does not read or select the descriptor itself. It defers each graph checksum
// until a native plan first needs that shard. An open error or graph damage
// clears the descriptor. On graph damage the node read also removes the
// generations (removeDamagedVectors), because the next bind would serve the
// same graph file.
func (store corpusStore) openJoinedSemanticGeneration(
	source string,
) (*semanticGeneration, error) {
	generation, err := openSemanticGenerationForSearch(semanticGenerationDir(store.indexDir, source), source)
	if err != nil {
		removeJoinedGeneration(store.cacheDir)
		return nil, err
	}
	generation.onUSearchDamage = func() {
		removeJoinedGeneration(store.cacheDir)
	}
	return generation, nil
}

// bindSemanticGenerations binds the base generation and the overlay; either
// may be empty. It removes every other generation of the node, because no
// search reads a superseded overlay after a new one binds. The caller must
// hold the publish lock.
func (store corpusStore) bindSemanticGenerations(
	state string,
	source string,
	overlay string,
) error {
	if err := store.writeJoinedGenerations(state, source, overlay); err != nil {
		return fmt.Errorf("activate joined generation: %w", err)
	}
	keep := map[string]struct{}{}
	var live []string
	for _, bound := range []string{source, overlay} {
		if bound != "" {
			dir := semanticGenerationDir(store.indexDir, bound)
			keep[filepath.Base(dir)] = struct{}{}
			live = append(live, dir)
		}
	}
	// The windows that only the replaced generations hold go to the unit
	// store first. So a later build copies them and does not embed them
	// again.
	archiveSupersededUnits(store.indexDir, live...)
	removeSemanticGenerationsExcept(store.indexDir, keep)
	// The removal changed the ctime of each graph file that the new
	// generation links: record the checks again, so searches trust them.
	for _, dir := range live {
		refreshSemanticUSearchChecks(dir)
	}
	return nil
}

func removeJoinedGeneration(cacheDir string) {
	removeCacheFile(cacheDir, joinedGenerationFile)
	removeCacheFile(cacheDir, joinedGenerationFile+".tmp")
}

// removeSemanticGenerationsExcept removes every semantic generation directory
// in indexDir whose name is not in keep. The caller must hold the publish lock.
func removeSemanticGenerationsExcept(indexDir string, keep map[string]struct{}) {
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, semanticGenerationPrefix) {
			continue
		}
		if _, ok := keep[name]; ok {
			continue
		}
		_ = os.RemoveAll(filepath.Join(indexDir, name))
	}
}
