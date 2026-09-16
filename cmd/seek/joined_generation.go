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
	joinedGenerationVersion  = 1
	joinedGenerationMaxBytes = 16 << 10
	joinedFamilyMaxBytes     = 8 << 20
)

type joinedGenerationDescriptor struct {
	Format      uint32 `json:"format"`
	State       string `json:"state"`
	FamilySHA   string `json:"family_sha256"`
	SemanticKey string `json:"semantic_key"`
}

// runJoinedIndexBuilds starts each required index branch before it waits for
// either branch. A nil function disables that branch for an already-current
// index or for --lexical-only.
func runJoinedIndexBuilds(lexical, semantic func()) {
	var builds sync.WaitGroup
	for _, build := range []func(){lexical, semantic} {
		if build == nil {
			continue
		}
		builds.Add(1)
		go func() {
			defer builds.Done()
			build()
		}()
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
		descriptor.SemanticKey == "" {
		return joinedGenerationDescriptor{}, fmt.Errorf("joined generation descriptor is incomplete")
	}
	if digest, err := hex.DecodeString(descriptor.FamilySHA); err != nil || len(digest) != sha256.Size {
		return joinedGenerationDescriptor{}, fmt.Errorf("joined generation family digest is invalid")
	}
	return descriptor, nil
}

func writeJoinedGeneration(
	cacheDir string,
	indexDir string,
	state string,
	source string,
) error {
	familySHA, err := joinedFamilyDigest(indexDir)
	if err != nil {
		return err
	}
	descriptor := joinedGenerationDescriptor{
		Format:      joinedGenerationVersion,
		State:       state,
		FamilySHA:   familySHA,
		SemanticKey: semanticGenerationKey(source),
	}
	content, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	content = append(content, '\n')
	if len(content) > joinedGenerationMaxBytes {
		return fmt.Errorf("joined generation descriptor is too large")
	}
	return writeCacheFile(cacheDir, joinedGenerationFile, string(content))
}

// joinedGenerationMatches performs the fast activation check for a joined
// index. It checks the descriptor, the Zoekt family-manifest digest, and the
// semantic manifest compatibility. It does not open the semantic row or vector
// data or validate USearch shards. openJoinedSemanticGeneration checks row and
// vector data before search and checks each graph before its first native use.
func joinedGenerationMatches(
	cacheDir string,
	indexDir string,
	state string,
	source string,
) bool {
	descriptor, err := readJoinedGeneration(cacheDir)
	if err != nil || descriptor.State != state ||
		descriptor.SemanticKey != semanticGenerationKey(source) {
		return false
	}
	familySHA, err := joinedFamilyDigest(indexDir)
	if err != nil || familySHA != descriptor.FamilySHA {
		return false
	}
	_, err = readSemanticManifest(semanticGenerationDir(indexDir, source), source)
	return err == nil
}

func joinedFamilyDigest(indexDir string) (string, error) {
	file, err := openRegularSemanticFile(filepath.Join(indexDir, familyManifestFile))
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, joinedFamilyMaxBytes+1)); err != nil {
		return "", err
	}
	if position, err := file.Seek(0, io.SeekCurrent); err != nil || position > joinedFamilyMaxBytes {
		return "", fmt.Errorf("zoekt family manifest is too large")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func publishSemanticGeneration(indexDir, stagingDir, source string) error {
	finalDir := semanticGenerationDir(indexDir, source)
	if err := validateSemanticGeneration(indexDir, source); err == nil {
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

func validateSemanticGeneration(indexDir, source string) error {
	generation, err := openSemanticGeneration(semanticGenerationDir(indexDir, source), source)
	if err != nil {
		return err
	}
	defer func() { _ = generation.Close() }()
	return generation.usearchErr
}

// openJoinedSemanticGeneration opens the source generation after the caller
// has matched its joined descriptor while holding the strict read lock. This
// function does not read or select the descriptor itself. It defers each graph
// checksum until a native plan first needs that shard. Graph damage clears the
// descriptor so the next run repairs the index.
func openJoinedSemanticGeneration(
	cacheDir string,
	indexDir string,
	source string,
) (*semanticGeneration, error) {
	generation, err := openSemanticGenerationForSearch(semanticGenerationDir(indexDir, source), source)
	if err != nil {
		removeJoinedGeneration(cacheDir)
		return nil, err
	}
	generation.onUSearchDamage = func() {
		removeJoinedGeneration(cacheDir)
	}
	return generation, nil
}

// bindSemanticGeneration binds an already validated semantic generation to
// the current Zoekt family. The caller must hold the publish lock.
func bindSemanticGeneration(
	cacheDir string,
	indexDir string,
	state string,
	source string,
) error {
	if err := writeJoinedGeneration(cacheDir, indexDir, state, source); err != nil {
		return fmt.Errorf("activate joined generation: %w", err)
	}
	removeOtherSemanticGenerations(indexDir, source)
	return nil
}

// publishAndBindSemanticGeneration validates a new generation once, then
// binds it to the current Zoekt family. The caller must hold the publish lock.
func publishAndBindSemanticGeneration(
	cacheDir string,
	indexDir string,
	state string,
	source string,
	stagingDir string,
) error {
	if err := publishSemanticGeneration(indexDir, stagingDir, source); err != nil {
		return err
	}
	return bindSemanticGeneration(cacheDir, indexDir, state, source)
}

// publishUnboundSemanticGeneration makes a finished generation reachable to a
// later search without binding it to the current Zoekt family.
//
// A drifted build has valid committed vectors: the generation is keyed by the
// commit, was built from a validated snapshot, and never reads the working
// tree. Throwing it away means embedding the same commit again on the next
// search. Publishing it without binding keeps it unreachable to this search's
// joined descriptor while a later clean search can bind it for free.
//
// It prunes in the same call, under the publish lock the caller holds. Without
// that, every drifted HEAD would leave one more generation behind, because
// binding is the only other thing that prunes. It keeps two generations: the
// one just published, and the one the live descriptor names, which a concurrent
// reader may hold.
func publishUnboundSemanticGeneration(
	cacheDir string,
	indexDir string,
	source string,
	stagingDir string,
) error {
	if err := publishSemanticGeneration(indexDir, stagingDir, source); err != nil {
		return err
	}
	keep := map[string]struct{}{
		filepath.Base(semanticGenerationDir(indexDir, source)): {},
	}
	if descriptor, err := readJoinedGeneration(cacheDir); err == nil && descriptor.SemanticKey != "" {
		keep[semanticGenerationPrefix+descriptor.SemanticKey] = struct{}{}
	}
	removeSemanticGenerationsExcept(indexDir, keep)
	return nil
}

func removeJoinedGeneration(cacheDir string) {
	removeCacheFile(cacheDir, joinedGenerationFile)
	removeCacheFile(cacheDir, joinedGenerationFile+".tmp")
}

// removeOtherSemanticGenerations removes rebuildable cache generations after
// a new joined descriptor is active. The caller must hold the publish lock.
func removeOtherSemanticGenerations(indexDir, source string) {
	removeSemanticGenerationsExcept(indexDir, map[string]struct{}{
		filepath.Base(semanticGenerationDir(indexDir, source)): {},
	})
}

// removeSemanticGenerationsExcept removes every semantic generation directory
// in indexDir whose name is not in keep. The caller must hold the publish lock.
//
// Both prune callers share this walk: binding keeps one generation, and an
// unbound publish keeps two, the new one and the one the live descriptor names.
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
