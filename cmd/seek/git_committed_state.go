package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	committedGitStateFile    = ".git-committed-v1"
	committedGitStateVersion = "v1"
)

// committedGitState tracks the published committed family. head is its
// current commit: a full scan sets it, and an admitted delta moves it. baseHead
// and baseShards remain the commit and shard count of the last full build, so
// the delta threshold does not reject a large base.
type committedGitState struct {
	head       gitObjectID
	baseHead   gitObjectID
	baseShards int
}

func readCommittedGitState(cacheDir string) (committedGitState, bool) {
	data, err := os.ReadFile(filepath.Join(cacheDir, committedGitStateFile))
	if err != nil {
		return committedGitState{}, false
	}
	// Four lines, each with its newline: the split ends with one empty string.
	lines := strings.Split(string(data), "\n")
	if len(lines) != 5 || lines[0] != committedGitStateVersion || lines[4] != "" {
		return committedGitState{}, false
	}
	headRaw, headOK := strings.CutPrefix(lines[1], "head ")
	baseRaw, baseOK := strings.CutPrefix(lines[2], "base_head ")
	shardsRaw, shardsOK := strings.CutPrefix(lines[3], "base_shards ")
	head, headErr := parseGitObjectID([]byte(headRaw))
	baseHead, baseErr := parseGitObjectID([]byte(baseRaw))
	baseShards, shardsErr := strconv.Atoi(shardsRaw)
	if !headOK || !baseOK || !shardsOK || headErr != nil || baseErr != nil || shardsErr != nil ||
		len(baseHead) != len(head) || baseShards < 0 {
		return committedGitState{}, false
	}
	return committedGitState{head: head, baseHead: baseHead, baseShards: baseShards}, true
}

func writeCommittedGitState(cacheDir string, state committedGitState) error {
	if !validGitObjectID([]byte(state.head)) || !validGitObjectID([]byte(state.baseHead)) ||
		len(state.head) != len(state.baseHead) || state.baseShards < 0 {
		return fmt.Errorf("invalid committed Git state")
	}
	value := fmt.Sprintf(
		"%s\nhead %s\nbase_head %s\nbase_shards %d\n",
		committedGitStateVersion,
		state.head,
		state.baseHead,
		state.baseShards,
	)
	return writeCacheFile(cacheDir, committedGitStateFile, value)
}

func deleteCommittedGitState(cacheDir string) {
	removeCacheFile(cacheDir, committedGitStateFile)
	removeCacheFile(cacheDir, committedGitStateFile+".tmp")
}
