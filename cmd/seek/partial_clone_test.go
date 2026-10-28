package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPartialCloneIndexesWithoutFetch searches a blobless sparse clone. A
// search must not fetch. The blob outside the sparse cone stays missing, its
// file is searchable by name, and one warning gives the count.
func TestPartialCloneIndexesWithoutFetch(t *testing.T) {
	requireTools(t)
	clone := newPartialClone(t)
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	logs := captureTestLogs(t, slog.LevelWarn)

	out, err := seek(t, "--lexical-only", "file:missing", clone)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dir/missing.txt") {
		t.Fatalf("missing blob is not searchable by name:\n%s", out)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "fetch") && (strings.Contains(line, "built-in") || strings.Contains(line, "run_command")) {
			t.Fatalf("search fetched over the network: %s", line)
		}
	}
	assertOneMissingBlobWarning(t, logs)
}

// TestPartialCloneWarnsOncePerCommand runs a descriptive search: the lexical
// and the semantic build each read the tree, and the command still warns once.
func TestPartialCloneWarnsOncePerCommand(t *testing.T) {
	requireTools(t)
	clone := newPartialClone(t)
	logs := captureTestLogs(t, slog.LevelWarn)
	_, scorer, err := stubSearch(t, payloadFar, "zqpartialmarker kept", clone)
	if err != nil {
		t.Fatal(err)
	}
	if scorer.embeds.Load() == 0 {
		t.Fatal("the semantic build did not run, so the test proves nothing")
	}
	assertOneMissingBlobWarning(t, logs)
}

// TestPartialCloneNeedsNoLazyFetchGit searches a partial clone with a Git
// that ignores GIT_NO_LAZY_FETCH. No Git command other than git config runs in
// the clone: Seek skips the node with one warning that gives the version
// message.
func TestPartialCloneNeedsNoLazyFetchGit(t *testing.T) {
	requireTools(t)
	clone := newPartialClone(t)
	cwdLog := filepath.Join(t.TempDir(), "git-cwd.log")
	installGitShim(t, "if [ \"$subcommand\" = version ]; then\n  echo 'git version 2.43.0'\n  exit 0\nfi\n"+
		"if [ \"$subcommand\" != config ]; then\n  pwd >> '"+cwdLog+"'\nfi\n"+
		"exec \"$SEEK_TEST_REAL_GIT\" \"$@\"\n")

	logs := captureTestLogs(t, slog.LevelWarn)
	err := seekRoot(context.Background(), t, nil, "--lexical-only", "zqpartialmarker", clone)
	if !errors.Is(err, errNoMatch) || !logsMention(logs.Records(), "a partial clone needs Git 2.45 or later") {
		t.Fatalf("err=%v warnings=%v, want the partial-clone version warning", err, logs.Records())
	}
	data, err := os.ReadFile(cwdLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for dir := range strings.Lines(string(data)) {
		if pathWithin(resolvedPath(t, clone), resolvedPath(t, strings.TrimSpace(dir))) {
			t.Fatalf("a Git command ran in the partial clone: %s", data)
		}
	}

	// Found below a folder seed, the clone is a child node. It gets one
	// warning, and the search goes on without it.
	logs = captureTestLogs(t, slog.LevelWarn)
	out, err := seek(t, "--lexical-only", "zqpartialmarker", filepath.Dir(clone))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	// The headers start with the operand as typed, so they hold the clone
	// path as typed, not its resolved path.
	below := func(dir string) func(string) bool {
		return func(header string) bool { return strings.HasPrefix(header, dir+string(filepath.Separator)) }
	}
	headers := headerPaths(out)
	if !slices.ContainsFunc(headers, below(filepath.Join(filepath.Dir(clone), "source"))) ||
		slices.ContainsFunc(headers, below(clone)) {
		t.Fatalf("want the source repository searched and the partial clone not:\n%s", out)
	}
	if records := logs.Records(); len(records) != 1 || !logsMention(records, "a partial clone needs Git 2.45 or later") {
		t.Fatalf("warnings=%v, want one partial-clone version warning for the child", records)
	}
}

// TestPartialCloneBlobArrivesLater fetches a missing blob after a search. The
// next search indexes its content in the lexical and the semantic data. A
// search with an unchanged object store runs no object check. A fetch that
// brings none of the missing blobs costs one check.
func TestPartialCloneBlobArrivesLater(t *testing.T) {
	requireTools(t)
	clone := newPartialClone(t, "far/still.txt")
	source := filepath.Join(filepath.Dir(clone), "source")
	scorer := observeModel(&hybridTestScorer{})
	newModel := scorer.factory()
	lexical := func(t *testing.T) string {
		t.Helper()
		out, err := seek(t, "--lexical-only", "zqpartialmarker", clone)
		if err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}
		return out
	}
	objectChecks := func(t *testing.T, search func()) int {
		t.Helper()
		trace := filepath.Join(t.TempDir(), "trace")
		t.Setenv("GIT_TRACE", trace)
		search()
		t.Setenv("GIT_TRACE", "")
		data, err := os.ReadFile(trace)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "built-in: git cat-file")
	}
	describe := func() {
		if _, err := seekWith(t, newModel, "zqpartialmarker missing", clone); err != nil && !errors.Is(err, errNoMatch) {
			t.Fatal(err)
		}
	}

	if out := lexical(t); strings.Contains(out, "dir/missing.txt") {
		t.Fatalf("the missing blob matched by content before it arrived:\n%s", out)
	}
	describe()
	embedded := scorer.embeds.Load()
	if embedded == 0 {
		t.Fatal("the semantic build did not run, so the test proves nothing")
	}

	gitRunIn(t, clone, "sparse-checkout", "add", "dir")
	if out := lexical(t); !strings.Contains(out, "dir/missing.txt") {
		t.Fatalf("the arrived blob is still name-only:\n%s", out)
	}
	describe()
	if scorer.embeds.Load() == embedded {
		t.Fatal("the semantic generation kept the name-only file after its blob arrived")
	}
	if paths := scorer.unitPaths(); !slices.Contains(paths, "dir/missing.txt") {
		t.Fatalf("embedded %v, want dir/missing.txt", paths)
	}
	plans, err := planCorpora(context.Background(), []string{clone})
	if err != nil {
		t.Fatal(err)
	}
	head := readHeadFile(plans[0].cacheDir)
	generation, err := openSemanticGenerationForSearch(semanticGenerationDir(plans[0].indexDir, head), head)
	if err != nil {
		t.Fatal(err)
	}
	published := slices.ContainsFunc(generation.rows, func(row semanticUnit) bool { return row.path == "dir/missing.txt" })
	_ = generation.Close()
	if !published {
		t.Fatal("the published semantic generation has no rows for the arrived blob")
	}

	if got := objectChecks(t, func() { lexical(t) }); got != 0 {
		t.Fatalf("a search with an unchanged object store ran %d object checks", got)
	}
	writeFixtureFile(t, filepath.Join(source, "late.txt"), "late\n")
	gitRunIn(t, source, "add", ".")
	gitRunIn(t, source, "commit", "-q", "-m", "late")
	gitRunIn(t, clone, "fetch", "-q", "origin")
	if got := objectChecks(t, func() { lexical(t) }); got != 1 {
		t.Fatalf("a fetch with no missing blob ran %d object checks, want 1", got)
	}
	if got := objectChecks(t, func() { lexical(t) }); got != 0 {
		t.Fatalf("the object check after the fetch ran again: %d", got)
	}
}

// newPartialClone makes a blobless sparse clone with one blob outside the
// sparse cone, plus the extra files outside the cone. It also sets a new user
// cache for the test.
func newPartialClone(t *testing.T, extra ...string) string {
	t.Helper()
	f := newGraphFixture(t)
	source := f.GitRepo("source", "kept.txt", "zqpartialmarker kept\n")
	writeFixtureFile(t, filepath.Join(source, "dir", "missing.txt"), "zqpartialmarker missing\n")
	for _, name := range extra {
		writeFixtureFile(t, filepath.Join(source, name), "zqpartialmarker "+name+"\n")
	}
	gitRunIn(t, source, "add", ".")
	gitRunIn(t, source, "commit", "-m", "add a file outside the sparse cone")
	gitRunIn(t, source, "config", "uploadpack.allowFilter", "true")
	clone := f.path("clone")
	gitRunIn(t, f.root, "clone", "--quiet", "--filter=blob:none", "--sparse", "file://"+source, clone)
	setTestUserCache(t)
	return clone
}

// assertOneMissingBlobWarning fails the test unless logs hold exactly one
// missing-object warning.
func assertOneMissingBlobWarning(t *testing.T, logs *testLogRecorder) {
	t.Helper()
	var warnings []string
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, "missing locally") {
			warnings = append(warnings, record.Message)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d missing-object warnings, want 1: %v", len(warnings), warnings)
	}
}

// TestPartialCloneBlobArrivesDuringDelta fetches a missing blob after the
// recheck of a delta build and before its reads. A Git shim runs the fetch
// when the recheck ends: the first `git cat-file --batch-command` of the
// delta search. The record keeps the store from before the recheck
// (buildObjectStoreFingerprint). So the next search sees the change and
// indexes the content.
func TestPartialCloneBlobArrivesDuringDelta(t *testing.T) {
	requireTools(t)
	clone := newPartialClone(t)
	source := filepath.Join(filepath.Dir(clone), "source")
	lexical := func() string {
		t.Helper()
		out, err := seek(t, "--lexical-only", "zqpartialmarker", clone)
		if err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}
		return out
	}
	if out := lexical(); strings.Contains(out, "dir/missing.txt") {
		t.Fatalf("the missing blob matched by content before it arrived:\n%s", out)
	}
	writeFixtureFile(t, filepath.Join(source, "kept.txt"), "zqpartialmarker kept two\n")
	gitRunIn(t, source, "commit", "-qam", "two")
	gitRunIn(t, clone, "pull", "-q", "--ff-only")
	// The shim keeps its stdout open until the fetch ends, so the recheck
	// reads the end of the cat-file output only after the blob arrived. The
	// rename of the armed file lets one cat-file run the fetch. The cache
	// estimate before the build runs `git cat-file --batch`, which the shim
	// passes on.
	armed := filepath.Join(t.TempDir(), "armed")
	t.Setenv("SEEK_TEST_ARMED", armed)
	t.Setenv("SEEK_TEST_CLONE", clone)
	t.Setenv("SEEK_TEST_OID", gitOutputIn(t, clone, "rev-parse", "HEAD:dir/missing.txt"))
	installGitShim(t, "case \"$subcommand $*\" in\n"+
		"cat-file*--batch-command*)\n"+
		"  if mv \"$SEEK_TEST_ARMED\" \"$SEEK_TEST_ARMED.done\" 2>/dev/null; then\n"+
		"    \"$SEEK_TEST_REAL_GIT\" \"$@\"\n"+
		"    status=$?\n"+
		"    \"$SEEK_TEST_REAL_GIT\" -C \"$SEEK_TEST_CLONE\" fetch -q origin \"$SEEK_TEST_OID\" >&2 || exit 1\n"+
		"    exit $status\n"+
		"  fi ;;\n"+
		"esac\n"+
		"exec \"$SEEK_TEST_REAL_GIT\" \"$@\"\n")
	writeFixtureFile(t, armed, "")
	lexical()
	if _, err := os.Stat(armed + ".done"); err != nil {
		t.Fatalf("no cat-file ran the fetch: %v", err)
	}
	if out := lexical(); !strings.Contains(out, "dir/missing.txt") {
		t.Fatalf("the blob that arrived during the delta is still name-only:\n%s", out)
	}
}

// TestPartialCloneSemanticPublishStops stops the semantic publish after a
// blob arrived, as a kill there would. The publish replaces the generation of
// HEAD, and each folder of that generation takes no change, so the publish
// fails. The pending record (writePendingMissingBlobRecord) still names the
// blob, so the next descriptive search rebuilds the generation.
func TestPartialCloneSemanticPublishStops(t *testing.T) {
	requireTools(t)
	clone := newPartialClone(t)
	scorer := &hybridTestScorer{}
	newModel := modelFactory(scorer)
	describe := func() {
		t.Helper()
		if _, err := seekWith(t, newModel, "zqpartialmarker missing", clone); err != nil && !errors.Is(err, errNoMatch) {
			t.Fatal(err)
		}
	}
	generationDir := func() (dir, head string) {
		t.Helper()
		plans, err := planCorpora(context.Background(), []string{clone})
		if err != nil {
			t.Fatal(err)
		}
		head = readHeadFile(plans[0].cacheDir)
		return semanticGenerationDir(plans[0].indexDir, head), head
	}
	rowsHaveArrivedBlob := func() bool {
		t.Helper()
		dir, head := generationDir()
		generation, err := openSemanticGenerationForSearch(dir, head)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = generation.Close() }()
		return slices.ContainsFunc(generation.rows, func(row semanticUnit) bool { return row.path == "dir/missing.txt" })
	}
	describe()
	if rowsHaveArrivedBlob() {
		t.Fatal("the generation has rows for a missing blob")
	}
	gitRunIn(t, clone, "fetch", "-q", "origin", gitOutputIn(t, clone, "rev-parse", "HEAD:dir/missing.txt"))
	dir, _ := generationDir()
	unlock := lockFolderTree(t, dir)
	describe()
	unlock()
	if rowsHaveArrivedBlob() {
		t.Fatal("the publish replaced a generation that takes no change, so the test stopped nothing")
	}
	describe()
	if !rowsHaveArrivedBlob() {
		t.Fatal("the generation kept the name-only file after a stopped publish")
	}
}
