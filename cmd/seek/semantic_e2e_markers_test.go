package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// End-to-end tests of the node states and the output markers of vector
// results. Each description is e2eMarkerQuery, whose words no fixture file
// holds, so each result comes from vector search unless a test doc says
// otherwise. The oracle is a literal set from the fixture or a one-word text
// search with no model on the same operands.

// e2eMarkerQuery is a description of two words that no fixture file holds.
const e2eMarkerQuery = "zqalpha zqbeta"

// e2eMarkerText runs a one-word text search with no model on operands. It is
// the oracle of these tests. A search with no result gives "".
func e2eMarkerText(t *testing.T, word string, operands ...string) string {
	t.Helper()
	output, err := captureStdout(t, func() error { return run(context.Background(), word, operands) })
	if err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("text search %q %v: %v\n%s", word, operands, err, output)
	}
	return output
}

// e2eMarkerDescribe runs e2eMarkerQuery with the test model and fails the
// test on an error other than no match.
func e2eMarkerDescribe(t *testing.T, operands ...string) (string, *hybridTestScorer, error) {
	t.Helper()
	output, scorer, err := describe(t, e2eMarkerQuery, operands...)
	if err != nil && !errors.Is(err, errNoMatch) {
		t.Fatalf("description %v: %v\n%s", operands, err, output)
	}
	return output, scorer, err
}

// e2eMarkerHeaders returns each result header of output with its language
// and tags, sorted.
func e2eMarkerHeaders(output string) []string {
	var headers []string
	for line := range strings.Lines(output) {
		if header, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "## "); ok {
			headers = append(headers, header)
		}
	}
	slices.Sort(headers)
	return headers
}

// e2eMarkerBlocks maps each result header of output to the lines that follow
// it, up to the blank line that ends the result.
func e2eMarkerBlocks(output string) map[string]string {
	blocks := make(map[string]string)
	header := ""
	for line := range strings.Lines(output) {
		line = strings.TrimSuffix(line, "\n")
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			header = rest
			blocks[header] = ""
			continue
		}
		if line == "" {
			header = ""
			continue
		}
		if header != "" {
			blocks[header] += line + "\n"
		}
	}
	return blocks
}

// e2eMarkerModelPaths returns the sorted, distinct node-relative paths that
// the test model embedded and scored.
func e2eMarkerModelPaths(scorer *hybridTestScorer) (embedded, scored []string) {
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	embedded = slices.Clone(scorer.embeddedPaths)
	for _, document := range scorer.documents {
		scored = append(scored, document.Path)
	}
	slices.Sort(embedded)
	slices.Sort(scored)
	return slices.Compact(embedded), slices.Compact(scored)
}

// e2eMarkerSameBlocks fails the test unless the description output and the
// text oracle print the same headers, tags and lines.
func e2eMarkerSameBlocks(t *testing.T, label, description, oracle string) {
	t.Helper()
	got, want := e2eMarkerBlocks(description), e2eMarkerBlocks(oracle)
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("%s: the description prints %d files, the text oracle %d:\ndescription:\n%s\noracle:\n%s",
			label, len(got), len(want), description, oracle)
	}
	for header, block := range want {
		if got[header] != block {
			t.Fatalf("%s: %q differs from the text oracle:\ndescription:\n%s\noracle:\n%s", label, header, description, oracle)
		}
	}
}

// TestE2EMarkerUncommittedTagOnDirtyVectorHits finds files of one Git node
// only through vectors. Modified, staged and untracked files print with
// [uncommitted], and the clean file prints with no tag. A text search agrees.
func TestE2EMarkerUncommittedTagOnDirtyVectorHits(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "clean.go", "package repo\n// SEMANTIC_TARGET zqtagmark clean\n")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package repo\n// SEMANTIC_TARGET zqtagmark zqcommittedcopy\n")
	writeFixtureFile(t, filepath.Join(repo, "restaged.go"), "package repo\n// SEMANTIC_TARGET zqtagmark zqcommittedcopy\n// restaged\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "committed copies")
	writeFixtureFile(t, filepath.Join(repo, "modified.go"), "package repo\n// SEMANTIC_TARGET zqtagmark modified\n")
	writeFixtureFile(t, filepath.Join(repo, "restaged.go"), "package repo\n// SEMANTIC_TARGET zqtagmark restaged\n")
	gitRunIn(t, repo, "add", "restaged.go")
	writeFixtureFile(t, filepath.Join(repo, "staged.go"), "package repo\n// SEMANTIC_TARGET zqtagmark staged\n")
	gitRunIn(t, repo, "add", "staged.go")
	writeFixtureFile(t, filepath.Join(repo, "untracked.go"), "package repo\n// SEMANTIC_TARGET zqtagmark untracked\n")
	setTestUserCache(t)

	want := []string{
		filepath.Join(repo, "clean.go") + " (Go)",
		filepath.Join(repo, "modified.go") + " (Go) [uncommitted]",
		filepath.Join(repo, "restaged.go") + " (Go) [uncommitted]",
		filepath.Join(repo, "staged.go") + " (Go) [uncommitted]",
		filepath.Join(repo, "untracked.go") + " (Go) [uncommitted]",
	}
	cold, _, err := e2eMarkerDescribe(t, repo)
	if err != nil {
		t.Fatalf("no vector hit: %v", err)
	}
	if got := e2eMarkerHeaders(cold); !slices.Equal(got, want) {
		t.Fatalf("headers %q, want %q:\n%s", got, want, cold)
	}
	// The dirty files print their current bytes, never the committed copy.
	if strings.Contains(cold, "zqcommittedcopy") {
		t.Fatalf("a vector hit prints the committed copy of a dirty file:\n%s", cold)
	}
	e2eMarkerSameBlocks(t, "cold", cold, e2eMarkerText(t, "zqtagmark", repo))
	warm, scorer, _ := e2eMarkerDescribe(t, repo)
	if warm != cold || scorer.embedCalls.Load() != 0 {
		t.Fatalf("warm search embeds %d batches or differs:\ncold:\n%s\nwarm:\n%s", scorer.embedCalls.Load(), cold, warm)
	}
}

// TestE2EMarkerDirtyCommittedRowsNeverPrint changes committed vector targets
// in the work tree: an edit that removes the target, an unstaged and a staged
// deletion, and a staged and an unstaged rename. Only the clean file and the
// new names print. The model never reads a committed row of a dirty path,
// with a warm base and with a cold build.
func TestE2EMarkerDirtyCommittedRowsNeverPrint(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("repo", "clean.go", "package repo\n// SEMANTIC_TARGET zqrowmark clean\n")
	for name, content := range map[string]string{
		"edited.go":    "package repo\n// SEMANTIC_TARGET zqrowmark zqoldtext edited\n",
		"deleted.go":   "package repo\n// SEMANTIC_TARGET zqrowmark zqoldtext deleted\n",
		"gitrm.go":     "package repo\n// SEMANTIC_TARGET zqrowmark zqoldtext staged deletion\n",
		"gitmv_old.go": "package repo\n// SEMANTIC_TARGET zqrowmark staged rename\n",
		"mv_old.go":    "package repo\n// SEMANTIC_TARGET zqrowmark unstaged rename\n",
	} {
		writeFixtureFile(t, filepath.Join(repo, name), content)
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "targets")
	setTestUserCache(t)
	if output, _, err := e2eMarkerDescribe(t, repo); err != nil || len(e2eMarkerHeaders(output)) != 6 {
		t.Fatalf("the committed search must find the 6 targets: err=%v\n%s", err, output)
	}

	writeFixtureFile(t, filepath.Join(repo, "edited.go"), "package repo\n// nothing left here\n")
	if err := os.Remove(filepath.Join(repo, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "rm", "-q", "gitrm.go")
	gitRunIn(t, repo, "mv", "gitmv_old.go", "gitmv_new.go")
	if err := os.Rename(filepath.Join(repo, "mv_old.go"), filepath.Join(repo, "mv_new.go")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(repo, "clean.go") + " (Go)",
		filepath.Join(repo, "gitmv_new.go") + " (Go) [uncommitted]",
		filepath.Join(repo, "mv_new.go") + " (Go) [uncommitted]",
	}
	if got := e2eMarkerHeaders(e2eMarkerText(t, "zqrowmark", repo)); !slices.Equal(got, want) {
		t.Fatalf("the text oracle gives %q, want %q", got, want)
	}
	if oracle := e2eMarkerText(t, "zqoldtext", repo); oracle != "" {
		t.Fatalf("the text oracle prints the old text:\n%s", oracle)
	}
	check := func(label string) {
		t.Helper()
		output, scorer, err := e2eMarkerDescribe(t, repo)
		if err != nil {
			t.Fatalf("%s: no vector hit: %v", label, err)
		}
		if got := e2eMarkerHeaders(output); !slices.Equal(got, want) || strings.Contains(output, "zqoldtext") {
			t.Fatalf("%s: headers %q, want %q, and no old text:\n%s", label, got, want, output)
		}
		// No path of a committed row prints, also not as a copy.
		for _, gone := range []string{"edited.go", "deleted.go", "gitrm.go", "gitmv_old.go", "mv_old.go"} {
			if strings.Contains(output, gone) {
				t.Fatalf("%s: the old path %s prints:\n%s", label, gone, output)
			}
		}
		if _, scored := e2eMarkerModelPaths(scorer); !slices.Equal(scored, []string{"clean.go", "gitmv_new.go", "mv_new.go"}) {
			t.Fatalf("%s: the model scored %q, want only the current files", label, scored)
		}
	}
	check("warm base")
	// A new cache: the first build sees the changed tree.
	setTestUserCache(t)
	check("cold build")
}

// TestE2EMarkerNodeTagsAcrossRoots searches a Git node and a folder node
// with a nested repository. Each vector hit gets [git] or [folder] and keeps
// [uncommitted] before it. One node prints no node tag. A text search agrees.
func TestE2EMarkerNodeTagsAcrossRoots(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("roots/repo", "committed.go", "package repo\n// SEMANTIC_TARGET zqrootmark committed\n")
	writeFixtureFile(t, filepath.Join(repo, "untracked.go"), "package repo\n// SEMANTIC_TARGET zqrootmark untracked\n")
	loose := f.path("roots/loose")
	writeFixtureFile(t, filepath.Join(loose, "loose.go"), "package loose\n// SEMANTIC_TARGET zqrootmark loose\n")
	f.GitRepo("roots/loose/nested", "n.go", "package nested\n// SEMANTIC_TARGET zqrootmark nested\n")
	setTestUserCache(t)

	cases := []struct {
		name     string
		operands []string
		want     []string
	}{
		{
			name:     "two roots",
			operands: []string{repo, loose},
			want: []string{
				filepath.Join(loose, "loose.go") + " (Go) [folder]",
				filepath.Join(loose, "nested", "n.go") + " (Go) [git]",
				filepath.Join(repo, "committed.go") + " (Go) [git]",
				filepath.Join(repo, "untracked.go") + " (Go) [uncommitted] [git]",
			},
		},
		{
			name:     "one Git node",
			operands: []string{repo},
			want: []string{
				filepath.Join(repo, "committed.go") + " (Go)",
				filepath.Join(repo, "untracked.go") + " (Go) [uncommitted]",
			},
		},
	}
	for _, tc := range cases {
		output, _, err := e2eMarkerDescribe(t, tc.operands...)
		if err != nil {
			t.Fatalf("%s: no vector hit: %v", tc.name, err)
		}
		if got := e2eMarkerHeaders(output); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: headers %q, want %q:\n%s", tc.name, got, tc.want, output)
		}
		e2eMarkerSameBlocks(t, tc.name, output, e2eMarkerText(t, "zqrootmark", tc.operands...))
	}
}

// TestE2EMarkerVectorOnlyDuplicatesCollapse finds one content only through
// vectors in a repository, its linked worktree, an untracked copy and a
// folder. The copies print as one result with an "also:" line (D30), and the
// model scores one copy. An edited copy prints as its own result.
func TestE2EMarkerVectorOnlyDuplicatesCollapse(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	const content = "package dup\n// SEMANTIC_TARGET zqdupmark shared\n"
	repo := f.GitRepo("dup/main", "dup.go", content)
	worktree := f.path("dup/wt")
	edited := f.path("dup/wt2")
	gitRunIn(t, repo, "worktree", "add", "-q", "-b", "b", worktree)
	gitRunIn(t, repo, "worktree", "add", "-q", "-b", "c", edited)
	writeFixtureFile(t, filepath.Join(repo, "extra", "dup.go"), content)
	folder := f.path("dup/folder")
	writeFixtureFile(t, filepath.Join(folder, "dup.go"), content)
	writeFixtureFile(t, filepath.Join(edited, "dup.go"), "package dup\n// SEMANTIC_TARGET zqdupmark edited copy\n")
	setTestUserCache(t)

	operands := []string{repo, worktree, edited, folder}
	output, scorer, err := e2eMarkerDescribe(t, operands...)
	if err != nil {
		t.Fatalf("no vector hit: %v", err)
	}
	// The copies with the same bytes, from the fixture. The representative
	// is the smallest path (D30), and the other copies follow it.
	copies := []string{
		filepath.Join(folder, "dup.go"),
		filepath.Join(repo, "dup.go"),
		filepath.Join(repo, "extra", "dup.go"),
		filepath.Join(worktree, "dup.go"),
	}
	slices.Sort(copies)
	wantHeaders := []string{
		filepath.Join(edited, "dup.go") + " (Go) [uncommitted] [git]",
		filepath.Join(folder, "dup.go") + " (Go) [folder]",
	}
	slices.Sort(wantHeaders)
	if got := e2eMarkerHeaders(output); !slices.Equal(got, wantHeaders) {
		t.Fatalf("headers %q, want %q:\n%s", got, wantHeaders, output)
	}
	if !strings.Contains(output, "## "+copies[0]+" (Go) [folder]\nalso: "+strings.Join(copies[1:], ", ")+"\n") {
		t.Fatalf("want %s with the other copies %v:\n%s", copies[0], copies[1:], output)
	}
	if strings.Count(output, "also: ") != 1 {
		t.Fatalf("the edited copy has an also: line:\n%s", output)
	}
	_, scored := e2eMarkerModelPaths(scorer)
	scorer.mu.Lock()
	documents := len(scorer.documents)
	scorer.mu.Unlock()
	if documents != 2 || !slices.Equal(scored, []string{"dup.go"}) {
		t.Fatalf("the model scored %d documents %q, want one per printed result", documents, scored)
	}
}

// TestE2EMarkerNameOnlyFilesNeverReachTheModel puts a vector target in
// binary files (a NUL byte at the start and after the 8,000-byte probe), a
// file above the content limit, an unreadable file and a blob that a partial
// clone lacks. The model never embeds or scores them, and the name-only
// notice counts all 10, as the text search does.
func TestE2EMarkerNameOnlyFilesNeverReachTheModel(t *testing.T) {
	requireTools(t)
	swapDocumentContentLimitForTest(t, 12<<10)
	f := newGraphFixture(t)
	big := "zqnamemark SEMANTIC_TARGET " + strings.Repeat("large text ", 1300) + "\n"
	// The NUL byte is after the probe of readContent: only the full check
	// finds that the file is binary.
	late := "zqnamemark SEMANTIC_TARGET late binary\n" + strings.Repeat("late text line\n", 540) + "\x00\n"
	repo := f.GitRepo("names/repo", "target.go", "package repo\n// SEMANTIC_TARGET zqnamemark repo\n")
	writeFixtureFile(t, filepath.Join(repo, "data.bin"), "zqnamemark SEMANTIC_TARGET\x00binary\n")
	writeFixtureFile(t, filepath.Join(repo, "big.txt"), big)
	writeFixtureFile(t, filepath.Join(repo, "late.bin"), late)
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "name-only")
	writeFixtureFile(t, filepath.Join(repo, "newdata.bin"), "zqnamemark SEMANTIC_TARGET\x00new binary\n")
	writeFixtureFile(t, filepath.Join(repo, "newbig.txt"), big)

	folder := f.path("names/loose")
	writeFixtureFile(t, filepath.Join(folder, "target.go"), "package loose\n// SEMANTIC_TARGET zqnamemark folder\n")
	writeFixtureFile(t, filepath.Join(folder, "data.bin"), "zqnamemark SEMANTIC_TARGET\x00folder binary\n")
	writeFixtureFile(t, filepath.Join(folder, "big.txt"), big)
	writeFixtureFile(t, filepath.Join(folder, "flate.bin"), late)
	locked := filepath.Join(folder, "locked.txt")
	writeFixtureFile(t, locked, "zqnamemark SEMANTIC_TARGET locked\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if file, err := os.Open(locked); err == nil {
		_ = file.Close()
		t.Skip("the test user can read a mode-000 file")
	}

	source := f.GitRepo("names/source", "kept.go", "package source\n// SEMANTIC_TARGET zqnamemark kept\n")
	writeFixtureFile(t, filepath.Join(source, "dir", "missing.go"), "package dir\n// SEMANTIC_TARGET zqnamemark missing\n")
	gitRunIn(t, source, "add", ".")
	gitRunIn(t, source, "commit", "-q", "-m", "a file outside the sparse cone")
	gitRunIn(t, source, "config", "uploadpack.allowFilter", "true")
	clone := f.path("names/clone")
	gitRunIn(t, f.root, "clone", "--quiet", "--filter=blob:none", "--sparse", "file://"+source, clone)
	setTestUserCache(t)

	operands := []string{repo, folder, clone}
	const notice = "10 name-only files could also match: binary, too large, not on disk, or unreadable\n"
	output, scorer, err := e2eMarkerDescribe(t, operands...)
	if err != nil {
		t.Fatalf("no vector hit: %v", err)
	}
	want := []string{
		filepath.Join(clone, "kept.go") + " (Go) [git]",
		filepath.Join(folder, "target.go") + " (Go) [folder]",
		filepath.Join(repo, "target.go") + " (Go) [git]",
	}
	slices.Sort(want)
	if got := e2eMarkerHeaders(output); !slices.Equal(got, want) || !strings.HasSuffix(output, "\n\n"+notice) {
		t.Fatalf("headers %q, want %q and the notice %q:\n%s", got, want, notice, output)
	}
	oracle := e2eMarkerText(t, "zqnamemark", operands...)
	if !strings.HasSuffix(oracle, "\n\n"+notice) {
		t.Fatalf("the text oracle counts other name-only files:\n%s", oracle)
	}
	nameOnly := []string{"data.bin", "big.txt", "late.bin", "newdata.bin", "newbig.txt", "flate.bin", "locked.txt", "dir/missing.go"}
	embedded, scored := e2eMarkerModelPaths(scorer)
	for _, path := range nameOnly {
		if slices.Contains(embedded, path) || slices.Contains(scored, path) {
			t.Fatalf("the model read the name-only file %s: embedded %q, scored %q", path, embedded, scored)
		}
	}
}

// TestE2EMarkerNestedRepositoriesAreTheirOwnNodes searches a repository with
// a submodule and an untracked nested repository. The vector hits of each
// print under its own path with its own tags, also a dirty submodule file.
// The parent never embeds a file of a child. A text search agrees.
func TestE2EMarkerNestedRepositoriesAreTheirOwnNodes(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	sub := f.GitRepo("nest/src", "s.go", "package sub\n// SEMANTIC_TARGET zqnestmark submodule\n")
	parent := f.GitRepo("nest/parent", "p.go", "package parent\n// SEMANTIC_TARGET zqnestmark parent\n")
	gitRunIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "mysub")
	gitRunIn(t, parent, "commit", "-q", "-m", "add submodule")
	writeFixtureFile(t, filepath.Join(parent, "mysub", "new.go"), "package sub\n// SEMANTIC_TARGET zqnestmark dirty submodule file\n")
	f.GitRepo("nest/parent/inner", "i.go", "package inner\n// SEMANTIC_TARGET zqnestmark inner\n")
	setTestUserCache(t)

	want := []string{
		filepath.Join(parent, "inner", "i.go") + " (Go) [git]",
		filepath.Join(parent, "mysub", "new.go") + " (Go) [uncommitted] [git]",
		filepath.Join(parent, "mysub", "s.go") + " (Go) [git]",
		filepath.Join(parent, "p.go") + " (Go) [git]",
	}
	cold, scorer, err := e2eMarkerDescribe(t, parent)
	if err != nil {
		t.Fatalf("no vector hit: %v", err)
	}
	if got := e2eMarkerHeaders(cold); !slices.Equal(got, want) {
		t.Fatalf("headers %q, want %q:\n%s", got, want, cold)
	}
	e2eMarkerSameBlocks(t, "cold", cold, e2eMarkerText(t, "zqnestmark", parent))
	embedded, _ := e2eMarkerModelPaths(scorer)
	for _, path := range embedded {
		if strings.HasPrefix(path, "mysub/") || strings.HasPrefix(path, "inner/") {
			t.Fatalf("the parent node embedded the child file %s: %q", path, embedded)
		}
	}
	if warm, _, _ := e2eMarkerDescribe(t, parent); warm != cold {
		t.Fatalf("cold and warm differ:\ncold:\n%s\nwarm:\n%s", cold, warm)
	}
}

// TestE2EMarkerGitIgnoredFileFollowsTheOperand puts a vector target in a
// file that the repository ignores. A search of the repository or of a parent
// folder never embeds it. The ignored folder as an operand finds it. A text
// search agrees on each operand set. TestE2EMarkerExactFileOperandUsesVectors
// covers an exact file outside Git.
func TestE2EMarkerGitIgnoredFileFollowsTheOperand(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("ign/repo", ".gitignore", "build/\n")
	writeFixtureFile(t, filepath.Join(repo, "keep.go"), "package repo\n// SEMANTIC_TARGET zqignmark kept\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "keep")
	build := filepath.Join(repo, "build")
	writeFixtureFile(t, filepath.Join(build, "out.go"), "package build\n// SEMANTIC_TARGET zqignmark ignored\n")
	setTestUserCache(t)

	keep := filepath.Join(repo, "keep.go") + " (Go)"
	cases := []struct {
		name     string
		operands []string
		want     []string
	}{
		{name: "repository", operands: []string{repo}, want: []string{keep}},
		// The parent folder is a folder node and the repository a Git node.
		{name: "parent folder", operands: []string{f.path("ign")}, want: []string{keep + " [git]"}},
		{name: "ignored folder", operands: []string{build}, want: []string{filepath.Join(build, "out.go") + " (Go)"}},
		{name: "repository and ignored folder", operands: []string{repo, build}, want: []string{
			filepath.Join(build, "out.go") + " (Go) [folder]",
			keep + " [git]",
		}},
	}
	for _, tc := range cases {
		output, scorer, err := e2eMarkerDescribe(t, tc.operands...)
		if err != nil {
			t.Fatalf("%s: no vector hit: %v", tc.name, err)
		}
		if got := e2eMarkerHeaders(output); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: headers %q, want %q:\n%s", tc.name, got, tc.want, output)
		}
		e2eMarkerSameBlocks(t, tc.name, output, e2eMarkerText(t, "zqignmark", tc.operands...))
		if !strings.Contains(output, "out.go") {
			embedded, scored := e2eMarkerModelPaths(scorer)
			if slices.ContainsFunc(slices.Concat(embedded, scored), func(path string) bool { return strings.HasSuffix(path, "out.go") }) {
				t.Fatalf("%s: the model read the ignored file: embedded %q, scored %q", tc.name, embedded, scored)
			}
		}
	}
}

// TestE2EMarkerStaleIndexAddsNoFilesByMeaning holds the build locks of a Git
// node and a folder node, as a peer builder does, after a change. The
// description then gets its candidates from text matches only (README
// "Parallel use"). This test needs text hits: strict.go and fstrict.go hold
// both description words.
func TestE2EMarkerStaleIndexAddsNoFilesByMeaning(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("stale/repo", "target.go", "package repo\n// SEMANTIC_TARGET committed target\n")
	writeFixtureFile(t, filepath.Join(repo, "strict.go"), "package repo\n// zqalpha zqbeta text match\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "strict")
	folder := f.path("stale/loose")
	writeFixtureFile(t, filepath.Join(folder, "ftarget.go"), "package loose\n// SEMANTIC_TARGET folder target\n")
	writeFixtureFile(t, filepath.Join(folder, "fstrict.go"), "package loose\n// zqalpha zqbeta folder text match\n")
	setTestUserCache(t)
	operands := []string{repo, folder}
	strict := []string{
		filepath.Join(folder, "fstrict.go") + " (Go) [folder]",
		filepath.Join(repo, "strict.go") + " (Go) [git]",
	}
	fresh := append(slices.Clone(strict),
		filepath.Join(folder, "ftarget.go")+" (Go) [folder]",
		filepath.Join(repo, "target.go")+" (Go) [git]",
	)
	slices.Sort(fresh)
	if output, _, _ := e2eMarkerDescribe(t, operands...); !slices.Equal(e2eMarkerHeaders(output), fresh) {
		t.Fatalf("the first search must find the targets and the text matches, want %q:\n%s", fresh, output)
	}

	plans, err := planCorpora(t.Context(), operands)
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans=%d err=%v", len(plans), err)
	}
	holders := make([]*os.File, 0, len(plans))
	for _, plan := range plans {
		// tryBuildLock opens its own file description, so the lock conflicts
		// with the build of the search, as the lock of a peer process does.
		holder, ok, err := tryBuildLock(plan.cacheDir)
		if err != nil || !ok {
			t.Fatalf("take the build lock of %s: ok=%t err=%v", plan.cacheDir, ok, err)
		}
		holders = append(holders, holder)
	}
	release := func() {
		for _, holder := range holders {
			releaseLock(holder)
		}
		holders = nil
	}
	t.Cleanup(release)
	writeFixtureFile(t, filepath.Join(repo, "new.go"), "package repo\n// SEMANTIC_TARGET untracked target\n")
	writeFixtureFile(t, filepath.Join(folder, "fnew.go"), "package loose\n// SEMANTIC_TARGET new folder target\n")

	output, scorer, err := e2eMarkerDescribe(t, operands...)
	if err != nil {
		t.Fatalf("the stale index lost the text matches: %v", err)
	}
	if got := e2eMarkerHeaders(output); !slices.Equal(got, strict) {
		t.Fatalf("stale headers %q, want only the text matches %q:\n%s", got, strict, output)
	}
	embedded, scored := e2eMarkerModelPaths(scorer)
	if len(embedded) != 0 || !slices.Equal(scored, []string{"fstrict.go", "strict.go"}) {
		t.Fatalf("a stale index read vectors: embedded %q, scored %q", embedded, scored)
	}

	release()
	output, _, _ = e2eMarkerDescribe(t, operands...)
	current := append(slices.Clone(fresh),
		filepath.Join(folder, "fnew.go")+" (Go) [folder]",
		filepath.Join(repo, "new.go")+" (Go) [uncommitted] [git]",
	)
	slices.Sort(current)
	if got := e2eMarkerHeaders(output); !slices.Equal(got, current) {
		t.Fatalf("after the peer, headers %q, want %q:\n%s", got, current, output)
	}
}

// TestE2EMarkerCommitMovesTheBaseAndTheOverlay changes a Git node between
// descriptions: dirty files first, then a commit of them and a deletion. Each
// step prints the files, tags and lines that a text search prints. After the
// commit, no file keeps [uncommitted] and the deleted file is gone.
func TestE2EMarkerCommitMovesTheBaseAndTheOverlay(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("commit/repo", "stay.go", "package repo\n// SEMANTIC_TARGET zqcommitmark stay\n")
	writeFixtureFile(t, filepath.Join(repo, "gone.go"), "package repo\n// SEMANTIC_TARGET zqcommitmark gone\n")
	writeFixtureFile(t, filepath.Join(repo, "moved.go"),
		"package repo\n\nfunc Moved() { // zqcommitmark\n\t// SEMANTIC_TARGET moved\n}\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "first")
	setTestUserCache(t)
	step := func(label string, want []string) string {
		t.Helper()
		output, _, err := e2eMarkerDescribe(t, repo)
		if err != nil {
			t.Fatalf("%s: no vector hit: %v", label, err)
		}
		if got := e2eMarkerHeaders(output); !slices.Equal(got, want) {
			t.Fatalf("%s: headers %q, want %q:\n%s", label, got, want, output)
		}
		e2eMarkerSameBlocks(t, label, output, e2eMarkerText(t, "zqcommitmark", repo))
		return output
	}
	header := func(name, tag string) string { return filepath.Join(repo, name) + " (Go)" + tag }

	step("first commit", []string{header("gone.go", ""), header("moved.go", ""), header("stay.go", "")})

	writeFixtureFile(t, filepath.Join(repo, "new.go"), "package repo\n// SEMANTIC_TARGET zqcommitmark new\n")
	writeFixtureFile(t, filepath.Join(repo, "moved.go"),
		"package repo\n\n// zqshift one\n// zqshift two\n\nfunc Moved() { // zqcommitmark\n\t// SEMANTIC_TARGET moved\n}\n")
	dirty := step("dirty tree", []string{
		header("gone.go", ""), header("moved.go", " [uncommitted]"), header("new.go", " [uncommitted]"), header("stay.go", ""),
	})
	if !strings.Contains(dirty, "6 func Moved() { // zqcommitmark\n") {
		t.Fatalf("the dirty vector hit does not print its current line 6:\n%s", dirty)
	}

	gitRunIn(t, repo, "rm", "-q", "gone.go")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "second")
	committed := step("second commit", []string{header("moved.go", ""), header("new.go", ""), header("stay.go", "")})
	if !strings.Contains(committed, "6 func Moved() { // zqcommitmark\n") {
		t.Fatalf("the committed vector hit does not print line 6:\n%s", committed)
	}
	if warm, scorer, _ := e2eMarkerDescribe(t, repo); warm != committed || scorer.embedCalls.Load() != 0 {
		t.Fatalf("warm search embeds %d batches or differs:\n%s", scorer.embedCalls.Load(), warm)
	}
}

// TestE2EMarkerExactFileOperandUsesVectors gives an exact file as the one
// operand: a committed and a dirty file of a Git repository, and a file
// outside Git. README "Usage" accepts exact files, so the vector target of
// each prints, as the text search prints it.
func TestE2EMarkerExactFileOperandUsesVectors(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("exact/repo", "committed.go", "package repo\n// SEMANTIC_TARGET zqexactmark committed\n")
	writeFixtureFile(t, filepath.Join(repo, "dirty.go"), "package repo\n// SEMANTIC_TARGET zqexactmark dirty\n")
	loose := f.path("exact/loose/loose.go")
	writeFixtureFile(t, loose, "package loose\n// SEMANTIC_TARGET zqexactmark loose\n")
	writeFixtureFile(t, f.path("exact/loose/other.go"), "package loose\n// SEMANTIC_TARGET zqexactmark sibling\n")
	setTestUserCache(t)
	for _, tc := range []struct{ name, operand, want string }{
		{name: "committed Git file", operand: filepath.Join(repo, "committed.go"), want: filepath.Join(repo, "committed.go") + " (Go)"},
		{name: "dirty Git file", operand: filepath.Join(repo, "dirty.go"), want: filepath.Join(repo, "dirty.go") + " (Go) [uncommitted]"},
		{name: "file outside Git", operand: loose, want: loose + " (Go)"},
	} {
		output, scorer, err := e2eMarkerDescribe(t, tc.operand)
		oracle := e2eMarkerText(t, "zqexactmark", tc.operand)
		if got := e2eMarkerHeaders(oracle); !slices.Equal(got, []string{tc.want}) {
			t.Fatalf("%s: the text oracle gives %q, want %q", tc.name, got, tc.want)
		}
		if err != nil {
			embedded, _ := e2eMarkerModelPaths(scorer)
			t.Errorf("%s: no vector hit (%v), the build embedded %q; the text oracle prints:\n%s", tc.name, err, embedded, oracle)
			continue
		}
		e2eMarkerSameBlocks(t, tc.name, output, oracle)
	}
}

// e2eMarkerRegion returns a Go file whose Target function holds the vector
// target. pad comment lines after the package line move it down: Target
// starts at line 7+pad.
func e2eMarkerRegion(pad int, tag string) string {
	var builder strings.Builder
	builder.WriteString("package region\n")
	for index := range pad {
		fmt.Fprintf(&builder, "// zqpad %s %d\n", tag, index)
	}
	builder.WriteString("\nfunc Before() int {\n\treturn 1\n}\n\n")
	builder.WriteString("func Target() int { // zqregionmark " + tag + "\n\t// SEMANTIC_TARGET routes the call\n\treturn 2\n}\n\n")
	builder.WriteString("func After() int {\n\treturn 3\n}\n")
	return builder.String()
}

// e2eMarkerRegionBlock returns the lines that a hit on the Target function of
// e2eMarkerRegion(pad, tag) prints: its first line with 3 lines of context on
// each side, each after its line number.
func e2eMarkerRegionBlock(pad int, tag string) string {
	first := 7 + pad
	lines := []string{"\treturn 1", "}", "", "func Target() int { // zqregionmark " + tag, "\t// SEMANTIC_TARGET routes the call", "\treturn 2", "}"}
	var builder strings.Builder
	for index, line := range lines {
		fmt.Fprintf(&builder, "%d %s\n", first-3+index, line)
	}
	return builder.String()
}

// TestE2EMarkerVectorHitPrintsItsUnitRegion finds a function only through
// vectors in a committed Git file, a dirty Git file whose function moved
// down, and a folder file. Each prints the first line of the function with
// its line number and 3 lines of context on each side, as a text search does.
func TestE2EMarkerVectorHitPrintsItsUnitRegion(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("region/repo", "committed.go", e2eMarkerRegion(0, "committed"))
	writeFixtureFile(t, filepath.Join(repo, "shifted.go"), e2eMarkerRegion(0, "shifted"))
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "regions")
	writeFixtureFile(t, filepath.Join(repo, "shifted.go"), e2eMarkerRegion(5, "shifted"))
	folder := f.path("region/loose")
	writeFixtureFile(t, filepath.Join(folder, "folder.go"), e2eMarkerRegion(2, "folder"))
	setTestUserCache(t)

	operands := []string{repo, folder}
	output, _, err := e2eMarkerDescribe(t, operands...)
	if err != nil {
		t.Fatalf("no vector hit: %v", err)
	}
	want := map[string]string{
		filepath.Join(repo, "committed.go") + " (Go) [git]":             e2eMarkerRegionBlock(0, "committed"),
		filepath.Join(repo, "shifted.go") + " (Go) [uncommitted] [git]": e2eMarkerRegionBlock(5, "shifted"),
		filepath.Join(folder, "folder.go") + " (Go) [folder]":           e2eMarkerRegionBlock(2, "folder"),
	}
	got := e2eMarkerBlocks(output)
	if len(got) != len(want) {
		t.Fatalf("printed %d files, want %d:\n%s", len(got), len(want), output)
	}
	for header, block := range want {
		if got[header] != block {
			t.Fatalf("%s prints:\n%s\nwant:\n%s\nfull output:\n%s", header, got[header], block, output)
		}
	}
	e2eMarkerSameBlocks(t, "regions", output, e2eMarkerText(t, "zqregionmark", operands...))
}
