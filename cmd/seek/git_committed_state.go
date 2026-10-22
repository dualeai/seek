package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	committedGitStateFile = ".git-committed-v1"
	// committedGitStateVersion names the contents rule of the committed
	// family: v2 holds every committed blob. A state of another version
	// selects one native full build, so no delta extends a base of another
	// rule.
	committedGitStateVersion = "v2"
)

// committedGitState tracks the published committed family. head is its
// current commit: a full build sets it, and an admitted delta moves it.
// baseHead and baseShards remain the commit and shard count of the last full
// build, so the delta threshold does not reject a large base. liveBytes is the
// content of the live blobs, and deadBytes the content that deltas tombstoned
// since the last full build (deltaNeedsCompaction).
type committedGitState struct {
	head       gitObjectID
	baseHead   gitObjectID
	baseShards int
	liveBytes  int64
	deadBytes  int64
}

func readCommittedGitState(cacheDir string) (committedGitState, bool) {
	data, err := os.ReadFile(filepath.Join(cacheDir, committedGitStateFile))
	if err != nil {
		return committedGitState{}, false
	}
	// Six lines, each with its newline: the split ends with one empty string.
	lines := strings.Split(string(data), "\n")
	if len(lines) != 7 || lines[0] != committedGitStateVersion || lines[6] != "" {
		return committedGitState{}, false
	}
	headRaw, headOK := strings.CutPrefix(lines[1], "head ")
	baseRaw, baseOK := strings.CutPrefix(lines[2], "base_head ")
	shardsRaw, shardsOK := strings.CutPrefix(lines[3], "base_shards ")
	liveRaw, liveOK := strings.CutPrefix(lines[4], "live_bytes ")
	deadRaw, deadOK := strings.CutPrefix(lines[5], "dead_bytes ")
	head, headErr := parseGitObjectID([]byte(headRaw))
	baseHead, baseErr := parseGitObjectID([]byte(baseRaw))
	baseShards, shardsErr := strconv.Atoi(shardsRaw)
	liveBytes, liveErr := strconv.ParseInt(liveRaw, 10, 64)
	deadBytes, deadErr := strconv.ParseInt(deadRaw, 10, 64)
	if !headOK || !baseOK || !shardsOK || !liveOK || !deadOK ||
		headErr != nil || baseErr != nil || shardsErr != nil || liveErr != nil || deadErr != nil ||
		len(baseHead) != len(head) || baseShards < 0 || liveBytes < 0 || deadBytes < 0 {
		return committedGitState{}, false
	}
	return committedGitState{head: head, baseHead: baseHead, baseShards: baseShards, liveBytes: liveBytes, deadBytes: deadBytes}, true
}

func writeCommittedGitState(cacheDir string, state committedGitState) error {
	if !validGitObjectID([]byte(state.head)) || !validGitObjectID([]byte(state.baseHead)) ||
		len(state.head) != len(state.baseHead) || state.baseShards < 0 || state.liveBytes < 0 || state.deadBytes < 0 {
		return fmt.Errorf("invalid committed Git state")
	}
	value := fmt.Sprintf(
		"%s\nhead %s\nbase_head %s\nbase_shards %d\nlive_bytes %d\ndead_bytes %d\n",
		committedGitStateVersion,
		state.head,
		state.baseHead,
		state.baseShards,
		state.liveBytes,
		state.deadBytes,
	)
	return writeCacheFile(cacheDir, committedGitStateFile, value)
}

func deleteCommittedGitState(cacheDir string) {
	removeCacheFile(cacheDir, committedGitStateFile)
	removeCacheFile(cacheDir, committedGitStateFile+".tmp")
}
