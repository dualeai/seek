package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
)

// corpusHashBytes is the byte length of a corpus ID hash (hashParts), and
// corpusHashHexLen is its length in hex. The GC enumerator keeps only the
// entries of the corpora directory with that hex shape (isCorpusHashName).
const (
	corpusHashBytes  = 16
	corpusHashHexLen = corpusHashBytes * 2
)

// hashParts returns the hex of the first corpusHashBytes bytes of the SHA-256
// of parts. It ends each part with a 0x00 byte. So two different part lists
// give different input bytes only while no part holds 0x00; an empty part is
// safe. No current input holds 0x00: a POSIX path cannot, a file identity is
// "dev:ino" in digits, and labels, numbers and versions are fixed text. A part
// that can hold 0x00 would let a caller make two part lists with one hash.
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

// indexOptionsHash hashes the build options and the format versions that
// decide the shard content. A change of one gives a new corpus ID and a new
// Git state hash.
func indexOptionsHash() string {
	return hashParts(
		"ctags_must_succeed", "true",
		// TrigramMax always equals SizeMax (indexBuildOptions), so size_max
		// covers both.
		"size_max", strconv.FormatInt(documentContentLimit, 10),
		"shard_max", strconv.Itoa(shardMax),
		"document_naming", seekDocumentNamingVersion,
		"zoekt_compatibility", zoektCompatibilityVersion,
	)
}

// gitCorpusFingerprint is the input of a Git node's state hash. It joins its
// parts with a 0x00 separator, with no 0x00 after the last part. hashParts
// ends every part with 0x00. The two forms are not interchangeable: a change
// of form changes every state key.
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
