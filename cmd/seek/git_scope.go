package main

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/sourcegraph/zoekt/query"
)

// maxRegexpFileOperands is the largest number of file operands that become
// anchored file-name regexps. Zoekt finds each such regexp through the
// file-name trigram index. A FileNameSet tests the name of every document of
// each shard of the whole-repository index. Above this count, the Or of that
// many regexp match trees costs more than one FileNameSet map, so the scope
// uses the set.
const maxRegexpFileOperands = 64

// gitScopeOperand is one path that the search of a Git node covers: an
// absolute path, and whether it is a directory. Any other operand is a regular
// file. statOperand read the type and rejected every other type, so the scope
// does not read the path again.
type gitScopeOperand struct {
	path string
	dir  bool
}

// gitScopeQuery gives the search-time filter that restricts a Git corpus
// search to the path operands below the work-tree root. An operand at the root
// selects the whole work tree: the filter is nil. A directory that another
// directory holds, and a file that a directory holds, add nothing.
func gitScopeQuery(root string, operands []gitScopeOperand) (query.Q, error) {
	root = canonicalCorpusPath(root)
	fileSet := make(map[string]struct{})
	dirSet := make(map[string]struct{})
	for _, operand := range operands {
		rel, ok := relWithin(root, canonicalCorpusPath(operand.path))
		if !ok {
			return nil, fmt.Errorf("path outside current git worktree: %s", operand.path)
		}
		if rel == "." {
			return nil, nil
		}
		name := filepath.ToSlash(rel)
		if operand.dir {
			dirSet[name] = struct{}{}
		} else {
			fileSet[name] = struct{}{}
		}
	}
	dirs := collapseDirectoryScopes(slices.Sorted(maps.Keys(dirSet)))
	files := dropFilesCoveredByDirs(slices.Sorted(maps.Keys(fileSet)), dirs)

	queries := make([]query.Q, 0, len(files)+len(dirs))
	if n := len(files); n > maxRegexpFileOperands {
		queries = append(queries, query.NewFileNameSet(files...))
	} else if n > 0 {
		for _, file := range files {
			fileQ, err := query.RegexpQuery("^"+regexp.QuoteMeta(file)+"$", false, true)
			if err != nil {
				return nil, fmt.Errorf("build file scope %q: %w", file, err)
			}
			queries = append(queries, fileQ)
		}
	}
	for _, dir := range dirs {
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

// coveredByAnyDir reports whether one of dirs is name or holds it
// (pathWithin). Both are slash paths below the work-tree root.
func coveredByAnyDir(name string, dirs []string) bool {
	return slices.ContainsFunc(dirs, func(dir string) bool { return pathWithin(dir, name) })
}
