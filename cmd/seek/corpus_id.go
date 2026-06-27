package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// corpusHashHexLen is the hex-encoded length of a corpus ID produced by
// newCorpusID — sha256 truncated to corpusHashBytes bytes, then hex-encoded.
// Used by the GC enumerator to filter unrelated entries in the corpora dir.
const (
	corpusHashBytes  = 16
	corpusHashHexLen = corpusHashBytes * 2
)

// hashParts terminates each part with a 0x00 byte before hashing, which makes
// the serialization injective ONLY while every part is NUL-free and structural
// labels are non-empty. All current inputs satisfy this (POSIX path components
// cannot contain 0x00; labels are fixed literals), so distinct part lists
// cannot collide. Do not feed parts that may contain 0x00 or be empty in a
// label position, or collisions become constructible.
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
		"size_max", strconv.Itoa(maxIndexedDocumentBytes),
		"shard_max", strconv.Itoa(shardMax),
		"document_naming", seekDocumentNamingVersion,
		"zoekt_compatibility", zoektCompatibilityVersion,
	}
}

func gitCorpusFingerprint(paths gitPaths, state repoState) string {
	return stringsJoinNUL(
		"git-state-v2",
		"index_generation", seekIndexGeneration,
		"cache_layout", seekCacheLayoutVersion,
		"document_naming", seekDocumentNamingVersion,
		"zoekt_compatibility", zoektCompatibilityVersion,
		"index_options", indexOptionsHash(),
		"worktree", canonicalCorpusPath(paths.RepoDir),
		"common_dir", canonicalCorpusPath(paths.CommonDir),
		"repo_state", repoStateFingerprint(paths.RepoDir, state),
	)
}

func gitCorpusStateHash(paths gitPaths, state repoState) string {
	return computeStateHash(gitCorpusFingerprint(paths, state))
}

// stringsJoinNUL joins parts with a single 0x00 SEPARATOR (n-1 NULs, no
// trailing) — distinct from hashParts, which TERMINATES every part with 0x00.
// The two framings are NOT interchangeable: both feed the cache-key path
// (hashParts → corpus IDs, stringsJoinNUL → state fingerprints), so swapping
// one for the other would silently rotate every key.
func stringsJoinNUL(parts ...string) string {
	if len(parts) == 0 {
		return ""
	}
	n := len(parts) - 1
	for _, part := range parts {
		n += len(part)
	}
	buf := make([]byte, 0, n)
	for i, part := range parts {
		if i > 0 {
			buf = append(buf, 0)
		}
		buf = append(buf, part...)
	}
	return string(buf)
}
