package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
)

// corpusHashHexLen is the hex-encoded length of a corpus ID produced by
// newCorpusID — sha256 truncated to corpusHashBytes bytes, then hex-encoded.
// Used by the GC enumerator to filter unrelated entries in the corpora dir.
const (
	corpusHashBytes  = 16
	corpusHashHexLen = corpusHashBytes * 2
)

// hashParts terminates each part with a 0x00 byte before hashing, which makes
// the serialization injective ONLY while every part is NUL-free; an empty part
// is safe. All current inputs satisfy this (a POSIX path cannot contain 0x00,
// a file identity is "dev:ino" in digits, and labels and versions are fixed
// literals), so distinct part lists cannot collide. Do not feed a part that
// may contain 0x00, or collisions become constructible.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:corpusHashBytes])
}

func newCorpusID(parts ...string) corpusID {
	return corpusID(hashParts(parts...))
}

func indexOptionsHash() string {
	return hashParts(indexOptionsParts()...)
}

func indexOptionsParts() []string {
	return []string{
		"ctags_must_succeed", "true",
		// TrigramMax always equals SizeMax (indexBuildOptions), so size_max
		// covers both.
		"size_max", strconv.FormatInt(documentContentLimit, 10),
		"shard_max", strconv.Itoa(shardMax),
		"document_naming", seekDocumentNamingVersion,
		"zoekt_compatibility", zoektCompatibilityVersion,
	}
}

// gitCorpusFingerprint joins its parts with a 0x00 separator, with no 0x00
// after the last part. hashParts ends every part with 0x00. The two framings
// are not interchangeable: a change of framing rotates every state key.
func gitCorpusFingerprint(paths gitPaths, state repoState) string {
	return strings.Join([]string{
		"git-state-v2",
		"index_generation", seekIndexGeneration,
		"cache_layout", seekCacheLayoutVersion,
		"document_naming", seekDocumentNamingVersion,
		"zoekt_compatibility", zoektCompatibilityVersion,
		"index_options", indexOptionsHash(),
		"worktree", pathIdentityOrClean(paths.RepoDir),
		"common_dir", pathIdentityOrClean(paths.CommonDir),
		"repo_state", repoStateFingerprint(paths.RepoDir, state),
	}, "\x00")
}

func gitCorpusStateHash(paths gitPaths, state repoState) string {
	return computeStateHash(gitCorpusFingerprint(paths, state))
}

// pathIdentityOrClean returns the file identity (dev:ino) of path, so a state
// hash does not depend on how the path was typed. When stat fails, it returns
// the clean path.
func pathIdentityOrClean(path string) string {
	if id, err := filesystemIdentity(path); err == nil {
		return id
	}
	return filepath.Clean(path)
}
