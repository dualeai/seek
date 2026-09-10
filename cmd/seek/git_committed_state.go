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

// committedGitState tracks the published committed family. head and budget
// describe its current commit and work totals. A full scan seeds those fields;
// an admitted delta updates them from old and new blob IDs in diff-tree without
// walking the unchanged target tree. baseHead and baseShards remain the commit
// and shard count of the last full build, so the delta threshold does not
// reject a large base.
type committedGitState struct {
	head       gitObjectID
	baseHead   gitObjectID
	budget     gitIndexBudget
	baseShards int
}

func readCommittedGitState(cacheDir string) (committedGitState, bool) {
	data, err := os.ReadFile(filepath.Join(cacheDir, committedGitStateFile))
	if err != nil || len(data) == 0 || data[len(data)-1] != '\n' {
		return committedGitState{}, false
	}
	raw := strings.TrimSuffix(string(data), "\n")
	lines := strings.Split(raw, "\n")
	if len(lines) != 6 || lines[0] != committedGitStateVersion {
		return committedGitState{}, false
	}
	value := func(line, prefix string) (string, bool) {
		if !strings.HasPrefix(line, prefix) {
			return "", false
		}
		v := strings.TrimPrefix(line, prefix)
		return v, v != ""
	}
	headRaw, ok := value(lines[1], "head ")
	if !ok {
		return committedGitState{}, false
	}
	head, err := parseGitObjectID([]byte(headRaw))
	if err != nil {
		return committedGitState{}, false
	}
	parseNonnegative := func(line, prefix string) (int64, bool) {
		rawValue, ok := value(line, prefix)
		if !ok {
			return 0, false
		}
		n, err := strconv.ParseInt(rawValue, 10, 64)
		return n, err == nil && n >= 0
	}
	baseHeadRaw, ok := value(lines[2], "base_head ")
	if !ok {
		return committedGitState{}, false
	}
	baseHead, err := parseGitObjectID([]byte(baseHeadRaw))
	if err != nil || len(baseHead) != len(head) {
		return committedGitState{}, false
	}
	candidates, ok := parseNonnegative(lines[3], "candidates ")
	if !ok || candidates > gitCandidateFileLimit {
		return committedGitState{}, false
	}
	indexedBytes, ok := parseNonnegative(lines[4], "indexed_bytes ")
	if !ok || indexedBytes > gitCorpusIndexedByteLimit {
		return committedGitState{}, false
	}
	baseShards64, ok := parseNonnegative(lines[5], "base_shards ")
	if !ok || baseShards64 > int64(maxInt()) {
		return committedGitState{}, false
	}
	return committedGitState{
		head:     head,
		baseHead: baseHead,
		budget: gitIndexBudget{
			candidates:   candidates,
			indexedBytes: indexedBytes,
		},
		baseShards: int(baseShards64),
	}, true
}

func writeCommittedGitState(cacheDir string, state committedGitState) error {
	head, headErr := parseGitObjectID([]byte(state.head))
	baseHead, baseHeadErr := parseGitObjectID([]byte(state.baseHead))
	if headErr != nil || baseHeadErr != nil || head != state.head || baseHead != state.baseHead ||
		len(state.head) != len(state.baseHead) || state.budget.candidates < 0 ||
		state.budget.candidates > gitCandidateFileLimit || state.budget.indexedBytes < 0 ||
		state.budget.indexedBytes > gitCorpusIndexedByteLimit || state.baseShards < 0 {
		return fmt.Errorf("invalid committed Git state")
	}
	value := fmt.Sprintf(
		"%s\nhead %s\nbase_head %s\ncandidates %d\nindexed_bytes %d\nbase_shards %d\n",
		committedGitStateVersion,
		state.head,
		state.baseHead,
		state.budget.candidates,
		state.budget.indexedBytes,
		state.baseShards,
	)
	return writeCacheFile(cacheDir, committedGitStateFile, value)
}

func deleteCommittedGitState(cacheDir string) {
	removeCacheFile(cacheDir, committedGitStateFile)
	removeCacheFile(cacheDir, committedGitStateFile+".tmp")
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
