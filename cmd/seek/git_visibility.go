package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// visibility is the routing decision for a single path operand resolved
// against one git worktree.
type visibility uint8

const (
	// visGit: the path is part of the repo's indexable content (tracked,
	// or untracked-but-not-ignored). Route to the repo's git corpus and
	// scope inside the zoekt index.
	visGit visibility = iota
	// visFallback: the path is excluded from the repo by a .gitignore rule,
	// so the git index never contains it. Route to a standalone folder/file
	// corpus so its content is still searched.
	visFallback
	// visOutside: the path is not owned by this repo — outside the worktree
	// or inside a nested submodule. The caller routes it to a plain
	// folder/file corpus (same as visFallback).
	visOutside
)

// classifyVisibility classifies every path against the single repo rooted at
// repoRoot. paths must be canonical absolute paths; callers must group by
// repo root and not mix roots in one call. Every input path appears in the
// returned map. An error is returned only for a genuine subprocess failure:
// git check-ignore exits 1 (nothing ignored) and 128 (outside repo /
// submodule boundary) are normal signals, not errors. It owns no boundary
// discovery (that is detectGitBoundary) and constructs no corpora.
func classifyVisibility(ctx context.Context, repoRoot string, paths []string) (map[string]visibility, error) {
	out := make(map[string]visibility, len(paths))
	relOf := make(map[string]string, len(paths)) // slash-relative -> canonical abs
	within := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, ok := relWithin(repoRoot, p)
		if !ok {
			out[p] = visOutside // not inside this worktree
			continue
		}
		if rel == "." {
			out[p] = visGit // the worktree root itself is never ignored
			continue
		}
		slashRel := filepath.ToSlash(rel)
		relOf[slashRel] = p
		within = append(within, slashRel)
	}
	if len(within) == 0 {
		return out, nil
	}

	ignored, aborted, err := checkGitIgnore(ctx, repoRoot, within)
	if err != nil {
		return nil, err
	}
	if !aborted {
		for rel, p := range relOf {
			out[p] = decideVisibility(ignored[rel])
		}
		return out, nil
	}

	// The batch hit a 128: a within-worktree path crosses a submodule
	// boundary and aborted the whole run. Re-query each path alone so one
	// offending path cannot misclassify the rest.
	for rel, p := range relOf {
		ign, aborted1, err := checkGitIgnore(ctx, repoRoot, []string{rel})
		if err != nil {
			return nil, err
		}
		if aborted1 {
			out[p] = visOutside // this path is the one inside a submodule
			continue
		}
		out[p] = decideVisibility(ign[rel])
	}
	return out, nil
}

func decideVisibility(ignored bool) visibility {
	if ignored {
		return visFallback
	}
	return visGit
}

// checkGitIgnore runs `git check-ignore -z --stdin` in DEFAULT (index-aware)
// mode over rels (repo-root-relative, slash form) and returns the set of
// ignored rels. aborted is true when git exits 128 (a path crosses a
// submodule boundary or is outside the repo); the batch output is then
// unusable and the caller must fall back per path.
//
// DEFAULT mode is deliberate — never pass --no-index. A tracked file that
// also matches a .gitignore pattern is reported NOT ignored (exit 1),
// which is correct: it is in the index and therefore indexed by the git
// corpus. --no-index would wrongly report it ignored.
//
// Each entry is sent as "./"+rel so git treats it as a literal path, not a
// pathspec. Without this, an operand whose repo-relative path starts with
// ':' would be parsed as pathspec magic — ":weird" reads as not-ignored
// (wrong), ":(...)"/":!" abort the batch with exit 128. git echoes the
// "./" back verbatim; collectIgnoredRels strips it.
func checkGitIgnore(ctx context.Context, repoDir string, rels []string) (map[string]bool, bool, error) {
	dotted := make([]string, len(rels))
	for i, rel := range rels {
		dotted[i] = "./" + rel
	}
	// core.fsmonitor=false: the ignore decision depends only on .gitignore
	// rules and index membership, never on working-tree mtimes, so skip the
	// fsmonitor IPC tax (~9ms/call on an fsmonitor-managed repo). This is NOT
	// --no-index — index membership is still consulted (tracked-but-ignored
	// stays tracked).
	cmd := gitCmd(ctx, "-c", "core.fsmonitor=false", "check-ignore", "-z", "--stdin")
	cmd.Dir = repoDir
	cmd.Stdin = strings.NewReader(strings.Join(dotted, "\x00"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return collectIgnoredRels(string(out)), false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 1: // no path was ignored
			return map[string]bool{}, false, nil
		case 128: // outside repo / submodule boundary — batch aborted
			return nil, true, nil
		}
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return nil, false, fmt.Errorf("git check-ignore: %w: %s", err, msg)
	}
	return nil, false, fmt.Errorf("git check-ignore: %w", err)
}

// collectIgnoredRels splits NUL-terminated check-ignore output into a set of
// repo-relative names, stripping the "./" literal-path prefix added by
// checkGitIgnore and dropping the trailing empty element after the final NUL.
func collectIgnoredRels(raw string) map[string]bool {
	set := make(map[string]bool)
	for _, entry := range strings.Split(raw, "\x00") {
		if entry != "" {
			set[strings.TrimPrefix(entry, "./")] = true
		}
	}
	return set
}
