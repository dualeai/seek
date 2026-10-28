package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// gitNoHead is the HeadSHA of a work tree whose HEAD names no commit: a new
// repository or an orphan branch.
const gitNoHead = "no-head"

// repoState holds the parsed result of a single git status call.
type repoState struct {
	HeadSHA   string   // commit SHA from # branch.oid, or gitNoHead
	RawOutput string   // raw output for state hashing, without child records
	Files     []string // paths of changed/untracked files
	// Children are the paths of nested repositories in the work tree: an
	// untracked repository (Git lists it as one "? dir/" entry) and a changed
	// gitlink that is still a repository. Each child is its own graph node,
	// and its records are not in Files or RawOutput. So a commit in a child
	// does not change the parent state.
	Children []string
}

type gitPaths struct {
	RepoDir   string
	CommonDir string
}

// resolveGitPaths asks Git for the work tree root and the common Git
// directory of the repository that holds dir.
func resolveGitPaths(ctx context.Context, dir string) (gitPaths, error) {
	cmd := newGitCommand(ctx, dir,
		"rev-parse",
		"--path-format=absolute",
		"--show-toplevel",
		"--git-common-dir",
	)
	out, err := gitOutput(ctx, cmd, "git rev-parse")
	if err != nil {
		return gitPaths{}, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return gitPaths{}, fmt.Errorf("unexpected git rev-parse output: got %d lines", len(lines))
	}

	paths := gitPaths{
		RepoDir:   strings.TrimSpace(lines[0]),
		CommonDir: strings.TrimSpace(lines[1]),
	}
	return paths, nil
}

// gitRepoStateIn runs one git status in dir and returns the repository state.
// It drops the untracked files of an untracked bare repository.
func gitRepoStateIn(ctx context.Context, dir string) (repoState, error) {
	cmd := newGitCommand(ctx, dir, "status", "--porcelain=v2", "--branch", "--no-renames", "--no-ahead-behind",
		"--untracked-files=all", "--ignore-submodules=dirty", "-z")
	out, err := gitOutput(ctx, cmd, "git status")
	if err != nil {
		return repoState{}, err
	}
	raw := string(out)
	return parseGitStatusV2Skipping(raw, untrackedBareRepos(dir, raw)), nil
}

// untrackedBareRepos returns the untracked directories of a work tree that are
// bare repositories. Git lists their files one by one, but they hold Git
// internals, not content. Only a "? <dir>/HEAD" entry can name one, so the
// check costs a few stats per such entry.
func untrackedBareRepos(repoDir, raw string) []string {
	var roots []string
	for record := range strings.SplitSeq(raw, "\x00") {
		path, ok := strings.CutPrefix(record, "? ")
		if !ok {
			continue
		}
		dir, ok := strings.CutSuffix(path, "/HEAD")
		if !ok {
			continue
		}
		if !detectBareRepoAt(filepath.Join(repoDir, filepath.FromSlash(dir))) {
			continue
		}
		roots = append(roots, dir)
	}
	return roots
}

// gitHeadTreeish returns the commit ID of HEAD in dir, or gitNoHead when HEAD
// names no commit.
func gitHeadTreeish(ctx context.Context, dir string) (string, error) {
	cmd := newGitCommand(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	out, err := gitOutput(ctx, cmd, "git rev-parse HEAD")
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			return gitNoHead, nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// parseGitStatusV2Skipping parses git status --porcelain=v2 --branch
// --no-renames -z output. With -z, a NUL byte ends each record, header or
// entry. It puts the nested repositories in Children, not in Files or
// RawOutput. It drops the untracked entries below the bareRepos directories
// from Files and RawOutput.
func parseGitStatusV2Skipping(raw string, bareRepos []string) repoState {
	state := repoState{HeadSHA: gitNoHead}

	var kept strings.Builder
	kept.Grow(len(raw))
	seen := make(map[string]struct{})
	pos := 0

	for pos < len(raw) {
		end := strings.IndexByte(raw[pos:], 0)
		if end < 0 {
			break
		}
		start := pos
		entry := raw[pos : pos+end]
		pos += end + 1

		if len(entry) < 2 {
			kept.WriteString(raw[start:pos])
			continue
		}

		// Header lines start with '#'
		if entry[0] == '#' {
			if strings.HasPrefix(entry, "# branch.oid ") {
				state.HeadSHA = entry[len("# branch.oid "):]
				// Git writes "(initial)" before the first commit. An empty
				// value names no object either, so it also means no HEAD.
				if state.HeadSHA == "(initial)" || state.HeadSHA == "" {
					state.HeadSHA = gitNoHead
				}
			}
			kept.WriteString(raw[start:pos])
			continue
		}

		var path string
		child := false
		switch entry[0] {
		case '?': // untracked: "? <path>"; a nested repository ends in "/"
			path = entry[2:]
			if trimmed, ok := strings.CutSuffix(path, "/"); ok {
				path, child = trimmed, true
			} else if slices.ContainsFunc(bareRepos, func(dir string) bool { return strings.HasPrefix(path, dir+"/") }) {
				continue
			}
		case '1': // changed: "1 XY sub mH mI mW hH hI <path>"
			path = extractV2Path(entry, 8)
			child = v2Field(entry, 5) == gitlinkMode
		case '2': // renamed/copied: "2 XY sub mH mI mW hH hI Xscore <path>"
			// With -z, a rename entry has one more NUL-terminated record
			// for the original path. Skip it, so that the parser does not
			// read it as a separate entry. --no-renames prevents rename
			// entries, but the parser does not depend on that.
			path = extractV2Path(entry, 9)
			child = v2Field(entry, 5) == gitlinkMode
			if nextEnd := strings.IndexByte(raw[pos:], 0); nextEnd >= 0 {
				pos += nextEnd + 1
			}
		case 'u': // unmerged: "u XY sub m1 m2 m3 mW h1 h2 h3 <path>"
			path = extractV2Path(entry, 10)
			child = v2Field(entry, 6) == gitlinkMode
		}

		if child {
			state.Children = append(state.Children, path)
			continue
		}
		kept.WriteString(raw[start:pos])
		if path != "" {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			state.Files = append(state.Files, path)
		}
	}

	state.RawOutput = kept.String()
	return state
}

// gitlinkMode is the Git file mode of a gitlink (a submodule commit).
const gitlinkMode = "160000"

// v2Field returns the space-separated field at index n of a porcelain v2
// entry. The work-tree mode tells whether a changed gitlink is still a
// repository: a gitlink replaced by a file is content of the parent.
func v2Field(entry string, n int) string {
	field, _, _ := strings.Cut(extractV2Path(entry, n), " ")
	return field
}

// extractV2Path returns the rest of a porcelain v2 entry after skipFields
// space-separated fields, or "" when no field follows them. When skipFields
// counts every field before the path, the rest is the path, spaces included.
func extractV2Path(entry string, skipFields int) string {
	idx := 0
	for range skipFields {
		space := strings.IndexByte(entry[idx:], ' ')
		if space < 0 {
			return ""
		}
		idx += space + 1
	}
	return entry[idx:]
}
