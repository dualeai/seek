package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// Builders shared by the classification gap tests. They sit on top of the
// existing initGitRepo* helpers and writeFileAt.

func writeTrackedFile(tb testing.TB, repo, name, content string) string {
	tb.Helper()
	writeFileAt(tb, repo, name, content)
	gitRunIn(tb, repo, "add", name)
	gitRunIn(tb, repo, "commit", "-m", "add "+name)
	return filepath.Join(repo, name)
}

func writeUntrackedFile(tb testing.TB, repo, name, content string) string {
	tb.Helper()
	writeFileAt(tb, repo, name, content)
	return filepath.Join(repo, name)
}

func writeIgnoredFile(tb testing.TB, repo, name, content string) string {
	tb.Helper()
	appendFileAt(tb, repo, ".gitignore", name+"\n")
	gitRunIn(tb, repo, "add", ".gitignore")
	gitRunIn(tb, repo, "commit", "-m", "ignore "+name)
	writeFileAt(tb, repo, name, content)
	return filepath.Join(repo, name)
}

func appendFileAt(tb testing.TB, repo, name, content string) {
	tb.Helper()
	p := filepath.Join(repo, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		tb.Fatal(err)
	}
}

func planForOperands(tb testing.TB, repo string, operands ...string) []corpusPlan {
	tb.Helper()
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		tb.Fatalf("resolve git paths: %v", err)
	}
	plans, err := planCorpora(context.Background(), &paths, operands)
	if err != nil {
		tb.Fatalf("planCorpora: %v", err)
	}
	return plans
}

// I3 — an untracked-but-visible file routes to the repo's scoped git corpus
// (its content is reached via the dirty layer), not a standalone folder corpus.
func TestPlanCorpora_UntrackedVisibleFileUsesScopedGitCorpus(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "committed.go", "package main\n")
	file := writeUntrackedFile(t, repo, "fresh.go", "package main\n")

	plans := planForOperands(t, repo, file)
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindGit || plans[0].scope == nil {
		t.Fatalf("untracked visible file should be a scoped git corpus, got kind=%q scope=%v", plans[0].kind, plans[0].scope)
	}
	if plans[0].root != canonicalCorpusPath(repo) {
		t.Fatalf("root mismatch: got %q want %q", plans[0].root, canonicalCorpusPath(repo))
	}
}

// I4 — a tracked file inside a nested repo routes to the INNER repo.
func TestPlanCorpora_FileInNestedGitUsesChildRepo(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	parent := initGitRepo(t, "parent.go", "package parent\n")
	nested := initGitRepoNoRemoteAt(t, filepath.Join(parent, "child"), "child.go", "package child\n")
	file := filepath.Join(nested, "child.go")

	plans := planForOperands(t, parent, file)
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindGit || plans[0].root != canonicalCorpusPath(nested) {
		t.Fatalf("nested file should route to inner repo %q, got kind=%q root=%q", canonicalCorpusPath(nested), plans[0].kind, plans[0].root)
	}
}

// I5 — multiple files of one repo collapse to a single scoped git plan.
func TestPlanCorpora_MultipleFilesSameRepoCollapseToOnePlan(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "a.go", "package main\n")
	b := writeTrackedFile(t, repo, "b.go", "package main\n")
	a := filepath.Join(repo, "a.go")

	plans := planForOperands(t, repo, a, b)
	if len(plans) != 1 {
		t.Fatalf("expected one collapsed plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindGit || plans[0].scope == nil {
		t.Fatalf("two files same repo should be one scoped git plan, got kind=%q scope=%v", plans[0].kind, plans[0].scope)
	}
}

// A file covered by a directory operand of the same repo does not spawn a
// second plan; both share one scoped git corpus.
func TestPlanCorpora_FileAndDirSameRepoCollapseToOnePlan(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "root.go", "package main\n")
	writeTrackedFile(t, repo, filepath.Join("sub", "x.go"), "package sub\n")
	dir := filepath.Join(repo, "sub")
	file := filepath.Join(repo, "sub", "x.go")

	plans := planForOperands(t, repo, dir, file)
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindGit || plans[0].scope == nil {
		t.Fatalf("expected one scoped git corpus (sub is non-root), got kind=%q scope=%v", plans[0].kind, plans[0].scope)
	}
}

// I1/I6 — a tracked file and an ignored file in the SAME repo split: the
// tracked file is git-scoped, the ignored file falls back to its own
// folder/file corpus so its content is still searched.
func TestPlanCorpora_TrackedAndIgnoredFileSameRepoSplit(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "tracked.go", "package main\n")
	tracked := filepath.Join(repo, "tracked.go")
	ignored := writeIgnoredFile(t, repo, "secret.txt", "shh\n")

	plans := planForOperands(t, repo, tracked, ignored)
	if len(plans) != 2 {
		t.Fatalf("expected two plans (git + folder fallback), got %d: %#v", len(plans), plans)
	}
	var git, folder *corpusPlan
	for i := range plans {
		switch plans[i].kind {
		case corpusKindGit:
			git = &plans[i]
		case corpusKindFolder:
			folder = &plans[i]
		}
	}
	if git == nil || git.root != canonicalCorpusPath(repo) || git.scope == nil {
		t.Fatalf("expected scoped git plan rooted at repo (scope!=nil), got %#v", plans)
	}
	if folder == nil || folder.rootType != rootTypeFile || folder.root != canonicalCorpusPath(ignored) {
		t.Fatalf("expected ignored file to fall back to a file corpus, got %#v", plans)
	}
}

// realCaseWithin must preserve a correctly-typed path exactly, and on a
// case/normalization-insensitive filesystem correct a mistyped one to the
// real on-disk name (so the byte-exact git scope matches).
func TestRealCaseWithin(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Sub", "Real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exact := filepath.Join(root, "Sub", "Real.txt")
	if got := realCaseWithin(root, exact); got != exact {
		t.Fatalf("exact path must be preserved: got %q want %q", got, exact)
	}
	// Only meaningful where the FS resolves a different-case path.
	wrong := filepath.Join(root, "sub", "real.txt")
	if _, err := os.Stat(wrong); err != nil {
		t.Skip("case-sensitive filesystem; correction path not exercisable")
	}
	if got := realCaseWithin(root, wrong); got != exact {
		t.Errorf("case correction: got %q want %q", got, exact)
	}
}

// Golden guard for the dirty-scope key. The key hashes only slash-relative
// names (no absolute paths), so a frozen hex literal is portable. It pins the
// whole serialization — tag, part order, separator AND the hash encoding —
// so any drift that would silently invalidate on-disk cache keys without a
// deliberate version bump fails here. Regenerate the literal only when the
// key formula is changed on purpose (and bump seekCacheLayoutVersion).
func TestGitDirtyScopeKey_FormulaStable(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "a.go", "package main\n")
	writeTrackedFile(t, repo, "b.go", "package main\n")
	paths, err := resolveGitPaths(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve git paths: %v", err)
	}
	plan, err := planCurrentGitCorpusWithOperands(paths, []string{
		filepath.Join(repo, "a.go"), filepath.Join(repo, "b.go"),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// Frozen: hashParts("git_dirty_scope_v1", "include_dirs", "",
	// "include_files", "a.go\x00b.go", "exclude_dirs", "", "exclude_files", "").
	const want = "710eba413e54cee6e5183a58b23bb6b3"
	if plan.dirtyScope == nil || plan.dirtyScope.key != want {
		t.Fatalf("dirty-scope key drifted: got %v want %q", plan.dirtyScope, want)
	}
}

// The set of corpus IDs a planning run produces must not depend on the
// order operands are passed (closes the order-independence concern without
// a full fuzzer).
func TestPlanCorpora_OperandOrderIndependent(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "a.go", "package main\n")
	b := writeTrackedFile(t, repo, "b.go", "package main\n")
	nested := initGitRepoNoRemoteAt(t, filepath.Join(repo, "child"), "c.go", "package child\n")
	a := filepath.Join(repo, "a.go")
	nestedFile := filepath.Join(nested, "c.go")

	ids := func(operands ...string) []string {
		plans := planForOperands(t, repo, operands...)
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

// I10 — a symlink whose resolved target is gitignored falls back to a
// file corpus (routing follows the resolved target).
func TestPlanCorpora_SymlinkToIgnoredFileFallsBack(t *testing.T) {
	requireGit(t)
	setTestUserCache(t)

	repo := initGitRepo(t, "app.go", "package main\n")
	target := writeIgnoredFile(t, repo, "secret.txt", "shh\n")
	link := filepath.Join(repo, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	plans := planForOperands(t, repo, link)
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d: %#v", len(plans), plans)
	}
	if plans[0].kind != corpusKindFolder || plans[0].rootType != rootTypeFile {
		t.Fatalf("symlink to ignored file should fall back to a file corpus, got kind=%q rootType=%q", plans[0].kind, plans[0].rootType)
	}
	if plans[0].root != canonicalCorpusPath(target) {
		t.Fatalf("root should be the resolved target %q, got %q", canonicalCorpusPath(target), plans[0].root)
	}
}
