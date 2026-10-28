package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// classifyVisibility classifies every path against the one repository at
// repoRoot. paths must be canonical absolute paths. Callers group them by
// repository root and give one root per call. Every input path appears in the
// returned map. True means that the path is indexable content of the
// repository: tracked, or untracked and not ignored. The Git corpus holds it,
// and the search scopes inside its index. False means that the path is ignored,
// outside the worktree, or inside a nested submodule. A folder or file corpus
// then searches its content. git check-ignore exit 1 (nothing ignored) is a
// normal signal. So is an exit 128 that names a submodule boundary or a path
// outside the repository. Any other failure is an error: Seek never searches
// a repository that Git cannot read as a folder. Boundary discovery belongs
// to detectGitBoundary, and this function makes no corpus.
func classifyVisibility(ctx context.Context, repoRoot string, paths []string) (map[string]bool, error) {
	out := make(map[string]bool, len(paths))
	// relOf maps a slash-relative path to its canonical absolute path.
	relOf := make(map[string]string, len(paths))
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

	// The batch got exit 128: a path in the worktree crosses a submodule
	// boundary or leaves the repository, and Git stopped the whole run. Ask
	// for each path alone, so that one such path cannot change the rest.
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

// checkGitIgnore runs `git check-ignore -z --stdin` in the default mode,
// which reads the Git index, over rels. rels are relative to the repository
// root, in slash form. It returns the set of ignored rels. aborted is true
// when Git exits 128 because a path crosses a submodule boundary or leaves
// the repository. The batch output is then not usable, and the caller asks
// for each path alone. Any other exit 128 (a corrupt index, a dubious owner)
// is an error.
//
// Never pass --no-index. In the default mode, Git reports a tracked file that
// also matches a .gitignore pattern as not ignored (exit 1). That is correct:
// the file is in the Git index, so the Git corpus indexes it. --no-index
// would report it as ignored.
//
// Each entry goes to Git as "./"+rel, so Git reads it as a literal path, not
// a pathspec. Else Git reads a leading ':' as pathspec magic. Then ":weird"
// reads as not ignored, which is wrong, and ":(...)" or ":!" stops the batch
// with exit 128. Git echoes the "./" back as it is; collectIgnoredRels
// removes it.
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
// names relative to the repository root. It removes the "./" prefix that
// checkGitIgnore adds, and drops the empty element after the last NUL.
func collectIgnoredRels(raw string) map[string]bool {
	set := make(map[string]bool)
	for _, entry := range strings.Split(raw, "\x00") {
		if entry != "" {
			set[strings.TrimPrefix(entry, "./")] = true
		}
	}
	return set
}
