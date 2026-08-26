package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sourcegraph/zoekt/query"
)

// This file builds the scope that restricts a git corpus search to selected
// paths: gitScopeSpec (the parsed dirs/files), the committed-layer zoekt
// query, the dirty-layer gitDirtyScope + git pathspecs, and the helpers that
// collapse/dedupe the selection.

type gitDirtyScope struct {
	includeDirs  []string
	includeFiles []string
	excludeDirs  []string
	excludeFiles []string
	key          string
}

type gitScopeSpec struct {
	dirs         []string
	files        []string
	rootIncluded bool
}

func combineGitScope(includeScope, excludeScope query.Q) query.Q {
	if excludeScope == nil {
		return includeScope
	}
	notExcluded := &query.Not{Child: excludeScope}
	if includeScope == nil {
		return notExcluded
	}
	return query.Simplify(query.NewAnd(includeScope, notExcluded))
}

// maxRegexpFileOperands bounds how many individual file operands are expanded
// into anchored filename regexps. Each anchored regexp distills to a filename
// trigram lookup (index-assisted), far cheaper than FileNameSet's O(numDocs)
// per-shard scan over a whole-repo combined index. Above this many operands the
// Or of that many regexp match-trees costs more than one FileNameSet map, so we
// fall back to the set. A var (not const) so tests can force either path.
var maxRegexpFileOperands = 64

func buildCurrentGitScope(root string, operands []string) (query.Q, error) {
	spec, err := buildGitScopeSpec(root, operands)
	if err != nil || spec.rootIncluded {
		return nil, err
	}

	queries := make([]query.Q, 0, len(spec.files)+len(spec.dirs))
	if n := len(spec.files); n > maxRegexpFileOperands {
		queries = append(queries, query.NewFileNameSet(spec.files...))
	} else if n > 0 {
		// Anchored exact-path filename regexps are trigram-assisted on the
		// combined whole-repo index; FileNameSet would scan every doc.
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

	switch len(queries) {
	case 0:
		return nil, nil
	case 1:
		return queries[0], nil
	default:
		return query.NewOr(queries...), nil
	}
}

func buildGitDirtyScope(root string, operands, excludes []string) (*gitDirtyScope, error) {
	include, err := buildGitScopeSpec(root, operands)
	if err != nil || include.rootIncluded {
		return nil, err
	}
	if len(include.dirs) == 0 && len(include.files) == 0 {
		return nil, nil
	}
	exclude, err := buildGitScopeSpec(root, excludes)
	if err != nil {
		return nil, err
	}
	scope := &gitDirtyScope{
		includeDirs:  include.dirs,
		includeFiles: include.files,
		excludeDirs:  exclude.dirs,
		excludeFiles: exclude.files,
	}
	scope.key = hashParts(
		"git_dirty_scope_v1",
		"include_dirs", strings.Join(scope.includeDirs, "\x00"),
		"include_files", strings.Join(scope.includeFiles, "\x00"),
		"exclude_dirs", strings.Join(scope.excludeDirs, "\x00"),
		"exclude_files", strings.Join(scope.excludeFiles, "\x00"),
	)
	return scope, nil
}

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
	dirs := collapseDirectoryScopes(sortedKeys(dirSet))
	files := dropFilesCoveredByDirs(sortedKeys(fileSet), dirs)
	return gitScopeSpec{dirs: dirs, files: files}, nil
}

func (s *gitDirtyScope) contains(name string) bool {
	name = filepath.ToSlash(name)
	if coveredByAnyDir(name, s.excludeDirs) || containsString(s.excludeFiles, name) {
		return false
	}
	return coveredByAnyDir(name, s.includeDirs) || containsString(s.includeFiles, name)
}

func (s *gitDirtyScope) gitIncludePathspecs() []string {
	if s == nil {
		return nil
	}
	pathspecs := make([]string, 0, len(s.includeDirs)+len(s.includeFiles))
	for _, dir := range s.includeDirs {
		pathspecs = append(pathspecs, gitLiteralPathspec(dir))
	}
	for _, file := range s.includeFiles {
		pathspecs = append(pathspecs, gitLiteralPathspec(file))
	}
	return pathspecs
}

func gitLiteralPathspec(name string) string {
	if name == "." {
		return "."
	}
	return ":(top,literal)" + name
}

func containsString(values []string, value string) bool {
	i := sort.SearchStrings(values, value)
	return i < len(values) && values[i] == value
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
