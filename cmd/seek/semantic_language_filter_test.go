package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// languageFilterFixture writes files that only vectors find for a description
// with no word in them: each holds SEMANTIC_TARGET, which the test model maps
// to the query vector, and the word zqlangmark, which Zoekt finds as text. The
// tree has a Git node (committed Go, Python and an extensionless Python
// script, and untracked Go and Python files in its dirty overlay) and a folder
// node (Go and Python). It returns the tree root.
func languageFilterFixture(t *testing.T) string {
	t.Helper()
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "committed.go", "package repo\n// zqlangmark SEMANTIC_TARGET committed go\n")
	writeFixtureFile(t, filepath.Join(repo, "committed.py"), "# zqlangmark SEMANTIC_TARGET committed python\n")
	// Zoekt finds the language of this file from its shebang line.
	writeFixtureFile(t, filepath.Join(repo, "script"),
		"#!/usr/bin/env python3\n# zqlangmark SEMANTIC_TARGET committed script\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "languages")
	writeFixtureFile(t, filepath.Join(repo, "dirty.go"), "package repo\n// zqlangmark SEMANTIC_TARGET dirty go\n")
	writeFixtureFile(t, filepath.Join(repo, "dirty.py"), "# zqlangmark SEMANTIC_TARGET dirty python\n")
	writeFixtureFile(t, f.path("tree/loose/loose.go"), "package loose\n// zqlangmark SEMANTIC_TARGET folder go\n")
	writeFixtureFile(t, f.path("tree/loose/loose.py"), "# zqlangmark SEMANTIC_TARGET folder python\n")
	setTestUserCache(t)
	return f.path("tree")
}

// headerPaths returns the file path of each result header in output.
func headerPaths(output string) []string {
	var paths []string
	for line := range strings.Lines(output) {
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			paths = append(paths, strings.Fields(rest)[0])
		}
	}
	slices.Sort(paths)
	return paths
}

// TestLanguageFilterKeepsTheVectorHitsOfThatLanguage runs descriptions whose
// words no file holds, so every result comes from vector search, with
// language filters. The oracle is the lexical-only search "zqlangmark
// <filter>", which runs no model: every file holds zqlangmark, so its hits are
// the files that the filter keeps. The vector results must be exactly those files,
// in the Git node's committed rows, its dirty overlay, and the folder node.
// The filter also keeps the model from scoring a file of another language.
// The cases are lang:go, lang:python, lang:Python, -lang:go, and lang:go with
// file:loose. The first case runs again on the built vectors and must print
// the same output.
func TestLanguageFilterKeepsTheVectorHitsOfThatLanguage(t *testing.T) {
	requireTools(t)
	tree := languageFilterFixture(t)
	t.Chdir(t.TempDir())
	cases := []struct {
		filter string
		// want holds the file names that the filter keeps, from the fixture.
		want []string
	}{
		{"lang:go", []string{"committed.go", "dirty.go", "loose.go"}},
		{"lang:python", []string{"committed.py", "dirty.py", "loose.py", "script"}},
		{"lang:Python", []string{"committed.py", "dirty.py", "loose.py", "script"}},
		{"-lang:go", []string{"committed.py", "dirty.py", "loose.py", "script"}},
		{"lang:go file:loose", []string{"loose.go"}},
	}
	for index, test := range cases {
		t.Run(test.filter, func(t *testing.T) {
			lexical, err := runHybridTestSearch(t, "zqlangmark "+test.filter, []string{tree}, lexicalOnlySearchPolicy(), nil)
			if err != nil {
				t.Fatalf("text oracle: %v", err)
			}
			oracle := headerPaths(lexical)
			names := make([]string, len(oracle))
			for at, path := range oracle {
				names[at] = filepath.Base(path)
			}
			slices.Sort(names)
			want := slices.Clone(test.want)
			slices.Sort(want)
			if !slices.Equal(names, want) {
				t.Fatalf("text search with %s kept %v, want %v", test.filter, names, want)
			}

			query := "zqalpha zqbeta " + test.filter
			output, scorer, err := describe(t, query, tree)
			if err != nil {
				t.Fatalf("description: %v\n%s", err, output)
			}
			if got := headerPaths(output); !slices.Equal(got, oracle) {
				t.Fatalf("vector results %v, want the text oracle %v:\n%s", got, oracle, output)
			}
			scorer.mu.Lock()
			defer scorer.mu.Unlock()
			for _, document := range scorer.documents {
				if !slices.Contains(want, filepath.Base(document.Path)) {
					t.Fatalf("the model scored %s, which %s drops", document.Path, test.filter)
				}
			}
			if index == 0 {
				// The first description built the vectors: a warm search
				// prints the same.
				warm, _, err := describe(t, query, tree)
				if err != nil || warm != output {
					t.Fatalf("warm search differs (err=%v):\ncold:\n%s\nwarm:\n%s", err, output, warm)
				}
			}
		})
	}
}

// TestLanguageFilterWithNoFileOfThatLanguage gives a description a language
// that no file has: vector search returns nothing, and the command reports no
// match.
func TestLanguageFilterWithNoFileOfThatLanguage(t *testing.T) {
	requireTools(t)
	tree := languageFilterFixture(t)
	t.Chdir(t.TempDir())
	output, _, err := describe(t, "zqalpha zqbeta lang:rust", tree)
	if !errors.Is(err, errNoMatch) || output != "" {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
}

// TestLanguageFilterOnTheFilteredVectorRoutes searches a folder whose Python
// rows are more than the direct exact route takes (semanticFilteredExactRows),
// so lang:python goes through the filtered USearch route, and whose Go rows
// are few, so lang:go takes the direct exact route. Neither route returns a
// file of another language. The direct route is exact, so it returns every Go
// file; the USearch route is approximate, so it must return at least one
// Python file but need not return all. Each file has its own content, so no
// result collapses into another's "also:" line, and its own vector near the
// query.
func TestLanguageFilterOnTheFilteredVectorRoutes(t *testing.T) {
	requireTools(t)
	setTestUserCache(t)
	folder := t.TempDir()
	const pythonFiles = semanticFilteredExactRows + 44
	const goFiles = 12
	for index := range pythonFiles {
		writeFileAt(t, folder, fmt.Sprintf("py/m%03d.py", index), fmt.Sprintf("# SEMANTIC_TARGET python module %d\n", index))
	}
	for index := range goFiles {
		writeFileAt(t, folder, fmt.Sprintf("go/m%03d.go", index), fmt.Sprintf("package m\n// SEMANTIC_TARGET go module %d\n", index))
	}
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		filter, dir, backend string
		files                int
		exact                bool
	}{
		{"lang:python", "py", "filtered-usearch", pythonFiles, false},
		{"lang:go", "go", "filtered-exact", goFiles, true},
	} {
		t.Run(test.filter, func(t *testing.T) {
			logs := captureTestLogs(t, slog.LevelDebug)
			queryVector := semanticVector{0: 1}
			scorer := &hybridTestScorer{
				queryVector: &queryVector,
				vectorForUnit: func(unit semanticUnit) semanticVector {
					// A unit vector with a small second axis that differs per
					// file keeps every row close to the query.
					sum := 0
					for _, c := range unit.path {
						sum += int(c)
					}
					side := 0.05 + 0.001*float64(sum%97)
					vector := semanticVector{0: float32(math.Sqrt(1 - side*side))}
					vector[1+sum%(semanticEmbeddingDimensions-1)] = float32(side)
					return vector
				},
			}
			output, err := runHybridTestSearch(t, "zqalpha zqbeta "+test.filter, []string{folder}, defaultSearchPolicy(),
				func(context.Context) (semanticModel, error) { return scorer, nil })
			if err != nil {
				t.Fatalf("description: %v", err)
			}
			paths := headerPaths(output)
			if len(paths) == 0 || len(paths) > test.files || test.exact && len(paths) != test.files {
				t.Fatalf("%s found %d files, want %d (exact route: %t)", test.filter, len(paths), test.files, test.exact)
			}
			for _, path := range paths {
				if filepath.Base(filepath.Dir(path)) != test.dir {
					t.Fatalf("%s found %s", test.filter, path)
				}
			}
			var backends []string
			for _, record := range logs.Records() {
				if record.Message == "Searched semantic index" {
					backends = append(backends, fmt.Sprint(testLogAttrs(record)["backend"]))
				}
			}
			if !slices.Contains(backends, test.backend) {
				t.Fatalf("%s ran the backends %v, want %s", test.filter, backends, test.backend)
			}
		})
	}
}
