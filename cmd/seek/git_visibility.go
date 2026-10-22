package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// classifyVisibility classifies every path against the single repo rooted at
// repoRoot. paths must be canonical absolute paths; callers must group by
// repo root and not mix roots in one call. Every input path appears in the
// returned map. True means that the path is indexable content of the
// repository (tracked, or untracked and not ignored): the Git corpus holds it,
// and the search scopes inside its index. False means that the path is
// ignored, outside the worktree, or inside a nested submodule: a folder or
// file corpus searches its content. git check-ignore exit 1 (nothing ignored)
// and an exit 128 that names a submodule boundary or a path outside the
// repository are normal signals. Any other failure is an error: Seek never
// searches a repository that Git cannot read as a folder. It owns no boundary
// discovery (that is detectGitBoundary) and constructs no corpora.
func classifyVisibility(ctx context.Context, repoRoot string, paths []string) (map[string]bool, error) {
	out := make(map[string]bool, len(paths))
	relOf := make(map[string]string, len(paths)) // slash-relative -> canonical abs
	within := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, ok := relWithin(repoRoot, p)
		if !ok {
			out[p] = false // not inside this worktree
			continue
		}
		if rel == "." {
			out[p] = true // the worktree root itself is never ignored
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
			out[p] = !ignored[rel]
		}
		return out, nil
	}

	// The batch hit a 128: a within-worktree path crosses a submodule
	// boundary or leaves the repository, and aborted the whole run. Re-query
	// each path alone so one offending path cannot misclassify the rest.
	for rel, p := range relOf {
		ign, aborted1, err := checkGitIgnore(ctx, repoRoot, []string{rel})
		if err != nil {
			return nil, err
		}
		// An aborted path is inside a submodule or outside the repository.
		out[p] = !aborted1 && !ign[rel]
	}
	return out, nil
}

// checkGitIgnore runs `git check-ignore -z --stdin` in DEFAULT (index-aware)
// mode over rels (repo-root-relative, slash form) and returns the set of
// ignored rels. aborted is true when git exits 128 because a path crosses a
// submodule boundary or leaves the repo; the batch output is then unusable
// and the caller must fall back per path. Any other exit 128 (a corrupt
// index, a dubious owner) is an error.
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
	cmd := newGitCheckIgnoreCommand(ctx, repoDir, "-z", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(dotted, "\x00"))
	out, err := gitOutput(ctx, cmd, "git check-ignore")
	if err == nil {
		return collectIgnoredRels(string(out)), false, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		switch exitErr.ExitCode() {
		case 1: // no path was ignored
			return map[string]bool{}, false, nil
		case 128:
			if message := err.Error(); strings.Contains(message, "is in submodule") ||
				strings.Contains(message, "is outside repository") {
				return nil, true, nil
			}
		}
	}
	return nil, false, err
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
