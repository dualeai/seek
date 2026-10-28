package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sourcegraph/zoekt/query"
)

// gitScopeSpec is the scope that restricts a Git corpus search to the selected
// paths: the parsed directory and file operands, below the work-tree root.
// rootIncluded selects the whole work tree.
type gitScopeSpec struct {
	dirs         []string
	files        []string
	rootIncluded bool
}

// maxRegexpFileOperands is the largest number of file operands that become
// anchored file-name regexps. Zoekt finds each such regexp through the
// file-name trigram index. A FileNameSet tests the name of every document of
// each shard of the whole-repository index. Above this count, the Or of that
// many regexp match trees costs more than one FileNameSet map, so the scope
// uses the set.
const maxRegexpFileOperands = 64

// query is the search-time filter of spec; nil covers the whole work tree.
func (spec gitScopeSpec) query() (query.Q, error) {
	if spec.rootIncluded {
		return nil, nil
	}

	queries := make([]query.Q, 0, len(spec.files)+len(spec.dirs))
	if n := len(spec.files); n > maxRegexpFileOperands {
		queries = append(queries, query.NewFileNameSet(spec.files...))
	} else if n > 0 {
		for _, file := range spec.files {
			fileQ, err := query.RegexpQuery("^"+regexp.QuoteMeta(file)+"$", false, true)
			if err != nil {
				return nil, fmt.Errorf("build file scope %q: %w", file, err)
			}
			queries = append(queries, fileQ)
		}
	}
	for _, dir := range spec.dirs {
		dirQ, err := query.RegexpQuery("^"+regexp.QuoteMeta(dir)+"/", false, true)
		if err != nil {
			return nil, fmt.Errorf("build directory scope %q: %w", dir, err)
		}
		queries = append(queries, dirQ)
	}

	if len(queries) == 1 {
		return queries[0], nil
	}
	return query.NewOr(queries...), nil
}

// buildGitScopeSpec resolves the path operands below the work-tree root. An
// operand at the root selects the whole work tree. A directory that another
// directory holds, and a file that a directory holds, add nothing.
func buildGitScopeSpec(root string, operands []string) (gitScopeSpec, error) {
	root = canonicalCorpusPath(root)
	fileSet := make(map[string]struct{})
	dirSet := make(map[string]struct{})
	for _, operand := range operands {
		abs, err := filepath.Abs(operand)
		if err != nil {
			return gitScopeSpec{}, newPathOperandError(pathOperandResolve, operand, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return gitScopeSpec{}, newPathOperandError(pathOperandRead, operand, err)
		}
		canonical := canonicalCorpusPath(abs)
		rel, ok := relWithin(root, canonical)
		if !ok {
			return gitScopeSpec{}, fmt.Errorf("path outside current git worktree: %s", operand)
		}
		if rel == "." {
			return gitScopeSpec{rootIncluded: true}, nil
		}
		name := filepath.ToSlash(rel)
		if info.IsDir() {
			dirSet[name] = struct{}{}
			continue
		}
		if !info.Mode().IsRegular() {
			return gitScopeSpec{}, fmt.Errorf("unsupported path operand: %s", operand)
		}
		fileSet[name] = struct{}{}
	}
	dirs := collapseDirectoryScopes(slices.Sorted(maps.Keys(dirSet)))
	files := dropFilesCoveredByDirs(slices.Sorted(maps.Keys(fileSet)), dirs)
	return gitScopeSpec{dirs: dirs, files: files}, nil
}

func collapseDirectoryScopes(dirs []string) []string {
	if len(dirs) < 2 {
		return dirs
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !coveredByAnyDir(dir, out) {
			out = append(out, dir)
		}
	}
	return out
}

func dropFilesCoveredByDirs(files, dirs []string) []string {
	if len(files) == 0 || len(dirs) == 0 {
		return files
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		if !coveredByAnyDir(file, dirs) {
			out = append(out, file)
		}
	}
	return out
}

func coveredByAnyDir(name string, dirs []string) bool {
	for _, dir := range dirs {
		if name == dir || strings.HasPrefix(name, dir+"/") {
			return true
		}
	}
	return false
}
