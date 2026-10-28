package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// writeTrackedFile writes name in repo and commits it. It returns the path of
// the file.
func writeTrackedFile(tb testing.TB, repo, name, content string) string {
	tb.Helper()
	writeFileAt(tb, repo, name, content)
	gitRunIn(tb, repo, "add", name)
	gitRunIn(tb, repo, "commit", "-m", "add "+name)
	return filepath.Join(repo, name)
}

// writeUntrackedFile writes name in repo with no commit. It returns the path
// of the file.
func writeUntrackedFile(tb testing.TB, repo, name, content string) string {
	tb.Helper()
	writeFileAt(tb, repo, name, content)
	return filepath.Join(repo, name)
}

// writeIgnoredFile adds name to the .gitignore of repo, commits the
// .gitignore, and writes name. It returns the path of the file.
func writeIgnoredFile(tb testing.TB, repo, name, content string) string {
	tb.Helper()
	ignore, err := os.OpenFile(filepath.Join(repo, ".gitignore"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		tb.Fatal(err)
	}
	_, err = ignore.WriteString(name + "\n")
	if err = errors.Join(err, ignore.Close()); err != nil {
		tb.Fatal(err)
	}
	gitRunIn(tb, repo, "add", ".gitignore")
	gitRunIn(tb, repo, "commit", "-m", "ignore "+name)
	writeFileAt(tb, repo, name, content)
	return filepath.Join(repo, name)
}

// planForOperands plans the nodes of operands and stops the test on an error.
func planForOperands(tb testing.TB, operands ...string) []node {
	tb.Helper()
	plans, err := planCorpora(context.Background(), operands)
	if err != nil {
		tb.Fatalf("planCorpora: %v", err)
	}
	return plans
}

// TestPlanCorpora_UntrackedVisibleFileUsesScopedGitCorpus checks that an
// untracked file that Git does not ignore goes to the scoped Git node of its
// repo. It gets no folder node. The dirty overlay holds its content.
func TestPlanCorpora_UntrackedVisibleFileUsesScopedGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "committed.go", "package main\n")
	file := writeUntrackedFile(t, repo, "fresh.go", "package main\n")

	plans := planForOperands(t, file)
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindGit || plans[0].scope == nil {
		t.Fatalf("untracked visible file should be a scoped git corpus, got kind=%q scope=%v", plans[0].kind, plans[0].scope)
	}
	if plans[0].root != resolvedPath(t, repo) {
		t.Fatalf("root mismatch: got %q want %q", plans[0].root, resolvedPath(t, repo))
	}
}

// TestPlanCorpora_FileInNestedGitUsesChildRepo checks that a tracked file of a
// nested repo goes to the node of the nested repo, not of the parent.
func TestPlanCorpora_FileInNestedGitUsesChildRepo(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	parent := initGitRepo(t, "parent.go", "package parent\n")
	nested := initGitRepoNoRemoteAt(t, filepath.Join(parent, "child"), "child.go", "package child\n")
	file := filepath.Join(nested, "child.go")

	plans := planForOperands(t, file)
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindGit || plans[0].root != resolvedPath(t, nested) {
		t.Fatalf("nested file should route to inner repo %q, got kind=%q root=%q", resolvedPath(t, nested), plans[0].kind, plans[0].root)
	}
}

// TestPlanCorpora_MultipleFilesSameRepoCollapseToOnePlan checks that two
// files of one repo give one scoped Git node.
func TestPlanCorpora_MultipleFilesSameRepoCollapseToOnePlan(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "a.go", "package main\n")
	b := writeTrackedFile(t, repo, "b.go", "package main\n")
	a := filepath.Join(repo, "a.go")

	plans := planForOperands(t, a, b)
	if len(plans) != 1 {
		t.Fatalf("expected one collapsed plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindGit || plans[0].scope == nil {
		t.Fatalf("two files same repo should be one scoped git plan, got kind=%q scope=%v", plans[0].kind, plans[0].scope)
	}
}

// TestPlanCorpora_FileAndDirSameRepoCollapseToOnePlan checks that a file
// below a directory operand of the same repo adds no second node. Both
// operands share one scoped Git node.
func TestPlanCorpora_FileAndDirSameRepoCollapseToOnePlan(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "root.go", "package main\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "x.go"), "package sub\n")
	dir := filepath.Join(repo, "sub")
	file := filepath.Join(repo, "sub", "x.go")

	plans := planForOperands(t, dir, file)
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindGit || plans[0].scope == nil {
		t.Fatalf("expected one scoped git corpus (sub is non-root), got kind=%q scope=%v", plans[0].kind, plans[0].scope)
	}
}

// TestPlanCorpora_TrackedAndIgnoredFileSameRepoSplit checks that a tracked
// file and an ignored file of one repo give two nodes. The tracked file goes
// to the scoped Git node. The ignored file gets a file node of its own, so
// the search still reads its content.
func TestPlanCorpora_TrackedAndIgnoredFileSameRepoSplit(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "tracked.go", "package main\n")
	tracked := filepath.Join(repo, "tracked.go")
	ignored := writeIgnoredFile(t, repo, "secret.txt", "shh\n")

	plans := planForOperands(t, tracked, ignored)
	if len(plans) != 2 {
		t.Fatalf("expected two plans (git + folder fallback), got %d: %#v", len(plans), plans)
	}
	var git, folder *node
	for i := range plans {
		switch plans[i].kind {
		case corpusKindGit:
			git = &plans[i]
		case corpusKindFolder:
			folder = &plans[i]
		}
	}
	if git == nil || git.root != resolvedPath(t, repo) || git.scope == nil {
		t.Fatalf("expected scoped git plan rooted at repo (scope!=nil), got %#v", plans)
	}
	if folder == nil || folder.rootType != rootTypeFile || folder.root != resolvedPath(t, ignored) {
		t.Fatalf("expected ignored file to fall back to a file corpus, got %#v", plans)
	}
}

// TestRealCaseWithin checks that realCaseWithin keeps a path typed with the
// on-disk case as it is. On a case-insensitive file system, it must change a
// path typed with another case to the on-disk names. The steps that compare
// names byte for byte need those names. On a case-sensitive file system, that
// path names no file, so it stays as typed. Below a directory that the test
// user cannot read, onDiskPath cannot open that directory, and realCaseWithin
// still gives the stored names.
func TestRealCaseWithin(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Sub", "Real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exact := filepath.Join(root, "Sub", "Real.txt")
	if got := realCaseWithin(root, exact, dirListings{}); got != exact {
		t.Fatalf("exact path must be preserved: got %q want %q", got, exact)
	}
	wrong := filepath.Join(root, "sub", "real.txt")
	want := exact
	if _, err := os.Stat(wrong); err != nil {
		want = wrong
	}
	if got := realCaseWithin(root, wrong, dirListings{}); got != want {
		t.Errorf("case correction: got %q want %q", got, want)
	}

	writeFixtureFile(t, filepath.Join(root, "Locked", "Real.txt"), "x")
	chmodUnreadableOrFatal(t, filepath.Join(root, "Locked"), 0o311)
	typed := filepath.Join(root, "locked", "REAL.txt")
	want = filepath.Join(root, "Locked", "Real.txt")
	if _, err := os.Stat(typed); err != nil {
		want = typed
	}
	if got := realCaseWithin(root, onDiskPath(typed), dirListings{}); got != want {
		t.Errorf("below an unreadable directory: got %q want %q", got, want)
	}
}

// TestPlanCorpora_OperandOrderIndependent checks that the set of node IDs
// does not depend on the order of the operands.
func TestPlanCorpora_OperandOrderIndependent(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "a.go", "package main\n")
	b := writeTrackedFile(t, repo, "b.go", "package main\n")
	nested := initGitRepoNoRemoteAt(t, filepath.Join(repo, "child"), "c.go", "package child\n")
	a := filepath.Join(repo, "a.go")
	nestedFile := filepath.Join(nested, "c.go")

	ids := func(operands ...string) []string {
		plans := planForOperands(t, operands...)
		out := make([]string, 0, len(plans))
		for _, p := range plans {
			out = append(out, string(p.id))
		}
		sort.Strings(out)
		return out
	}

	got1 := ids(a, b, nestedFile)
	got2 := ids(nestedFile, b, a)
	got3 := ids(b, nestedFile, a)
	if !reflect.DeepEqual(got1, got2) || !reflect.DeepEqual(got1, got3) {
		t.Fatalf("plan ID set depends on operand order: %v / %v / %v", got1, got2, got3)
	}
}

// TestPlanCorpora_SymlinkToIgnoredFileFallsBack checks that a symlink to an
// ignored file gives a file node at the target. The route follows the
// resolved target.
func TestPlanCorpora_SymlinkToIgnoredFileFallsBack(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "app.go", "package main\n")
	target := writeIgnoredFile(t, repo, "secret.txt", "shh\n")
	link := filepath.Join(repo, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	plans := planForOperands(t, link)
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindFolder || plans[0].rootType != rootTypeFile {
		t.Fatalf("symlink to ignored file should fall back to a file corpus, got kind=%q rootType=%q", plans[0].kind, plans[0].rootType)
	}
	if plans[0].root != resolvedPath(t, target) {
		t.Fatalf("root should be the resolved target %q, got %q", resolvedPath(t, target), plans[0].root)
	}
}
