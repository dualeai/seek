package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// TestPublishMarksAFailedMoveBack gives the build folder an ACL entry that
// denies the test user delete_child and passes to new subfolders. The folder
// that swapFiles makes for the live shards gets the entry. The move aside adds
// an entry to that folder, so it works. The rename in removes an entry from
// the build folder, and the move back removes an entry from the aside folder,
// so both fail. The family is torn, and .swapping must name it for repair.
// Linux has no such trigger for a user that is not root. There, one mode bit
// gives the right to add and to remove an entry of a folder.
func TestPublishMarksAFailedMoveBack(t *testing.T) {
	const shard = "folder_x_v16.00000.zoekt"
	store := corpusStore{cacheDir: t.TempDir(), indexDir: filepath.Join(t.TempDir(), "index")}
	buildDir := filepath.Join(filepath.Dir(store.indexDir), "build")
	for _, dir := range []string{store.indexDir, buildDir} {
		writeFixtureFile(t, filepath.Join(dir, shard), "shard")
	}
	probe := filepath.Join(buildDir, "probe")
	writeFixtureFile(t, probe, "")
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	// The cleanup runs before the removal of t.TempDir, which the entries
	// would stop.
	t.Cleanup(func() {
		if out, err := exec.Command("/bin/chmod", "-RN", buildDir).CombinedOutput(); err != nil {
			t.Errorf("remove the ACL entries: %v: %s", err, out)
		}
	})
	if out, err := exec.Command("/bin/chmod", "+a", "user:"+me.Username+" deny delete_child,directory_inherit", buildDir).CombinedOutput(); err != nil {
		t.Fatalf("set the ACL entry: %v: %s", err, out)
	}
	if err := os.Remove(probe); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("remove an entry against the deny entry: %v, want a permission error: the test user ignores ACL entries, as root does", err)
	}
	if _, err := store.publishShardFamilyLocked(buildDir, familyAll); err == nil {
		t.Fatal("the publish renamed a shard out of a folder that denies delete_child")
	}
	if _, err := os.Lstat(filepath.Join(store.indexDir, shard)); !os.IsNotExist(err) {
		t.Fatalf("the live shard is in the index after the publish (err=%v), so the move aside did not work or the move back worked", err)
	}
	if label := readCacheFile(store.cacheDir, swappingMarkerFile); label != familyAll.label() {
		t.Fatalf(".swapping=%q after a failed move back, want %q", label, familyAll.label())
	}
}

// TestPublishKeepsAFamilyWithAnImmutableShard makes the later shard of a
// two-shard folder family immutable (no link, no move, no removal) and
// changes a file. The build fails, and the publish moves the live shards back:
// the family is whole, every file it held is found, and no .swapping stays.
// With the flag cleared, the next build publishes the change.
func TestPublishKeepsAFamilyWithAnImmutableShard(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "a.go", "package a\n// zqlocka\n")
	plan := planFolderTestCorpus(t, folder)
	ctx := t.Context()
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, folder, "b.go", "package b\n// zqlockb\n")
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	family := func() []string {
		t.Helper()
		scan, err := scanFamily(plan.indexDir)
		if err != nil || !scan.matchesManifest() {
			t.Fatalf("the family is not whole: %v", err)
		}
		names := make([]string, 0, len(scan.members))
		for _, member := range scan.members {
			names = append(names, member.name)
		}
		return names
	}
	before := family()
	var locked string
	for _, name := range before {
		if strings.HasSuffix(name, ".00001.zoekt") {
			locked = filepath.Join(plan.indexDir, name)
		}
	}
	if locked == "" {
		t.Fatalf("family %v has no later shard", before)
	}
	if err := syscall.Chflags(locked, 0x2 /* UF_IMMUTABLE */); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Chflags(locked, 0) })
	writeFileAt(t, folder, "c.go", "package c\n// zqlockc\n")
	if _, err := ensureFolderCorpusFresh(ctx, plan); err == nil {
		t.Fatal("the build published over an immutable shard")
	}
	if after := family(); !slices.Equal(after, before) {
		t.Fatalf("family %v after the failed publish, want %v", after, before)
	}
	if label := readCacheFile(plan.cacheDir, swappingMarkerFile); label != "" {
		t.Fatalf(".swapping=%q after a rolled-back swap, want none", label)
	}
	found := func(pattern string) int {
		t.Helper()
		files, err := searchPlannedCorpusForTest(ctx, plan, pattern)
		if err != nil {
			t.Fatal(err)
		}
		return len(files)
	}
	if found("zqlocka") != 1 || found("zqlockb") != 1 {
		t.Fatal("the failed publish lost a file of the live family")
	}
	if err := syscall.Chflags(locked, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureFolderCorpusFresh(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if found("zqlockc") != 1 {
		t.Fatal("the next build did not publish the change")
	}
}

// TestUncommittedPublishKeepsAFamilyWithAnImmutableShard gives a Git node an
// uncommitted family of two shards, makes the later one immutable, and
// changes the dirty files. The build fails, and its swap moves the live
// uncommitted shards back. The old family is whole and found, with no stale
// shard beside a new one. With the flag cleared, the next build publishes the
// change.
func TestUncommittedPublishKeepsAFamilyWithAnImmutableShard(t *testing.T) {
	requireTools(t)
	swapForTest(t, &shardMax, 1024)
	dir := initGitRepo(t, "app.go", "package main\n// zqucommitted\n")
	ctx := t.Context()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u1.go", "package main\n// zqu1\n"+strings.Repeat("// filler line of u1\n", 100))
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u2.go", "package main\n// zqu2old\n")
	reindexGit(t, ctx, paths, plan)
	shards := repositoryShardFiles(plan.indexDir, repoUncommitted)
	if len(shards) != 2 {
		t.Fatalf("uncommitted shards %v, want 2", shards)
	}
	if err := syscall.Chflags(shards[1], 0x2 /* UF_IMMUTABLE */); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Chflags(shards[1], 0) })
	if err := os.Remove(filepath.Join(dir, "u1.go")); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, dir, "u2.go", "package main\n// zqu2new\n")
	build := func() error {
		state := mustGitRepoStateIn(t, ctx, paths.RepoDir)
		return runIndexingWithCache(ctx, paths, plan.corpusStore, state, gitCorpusStateHash(paths, state))
	}
	found := func(pattern string) int {
		t.Helper()
		files, err := searchPlannedCorpusForTest(ctx, plan, pattern)
		if err != nil {
			t.Fatal(err)
		}
		return len(files)
	}
	if err := build(); err == nil {
		t.Fatal("the build published over an immutable uncommitted shard")
	}
	if after := repositoryShardFiles(plan.indexDir, repoUncommitted); !slices.Equal(after, shards) {
		t.Fatalf("uncommitted shards %v after the failed publish, want %v", after, shards)
	}
	if scan, err := scanFamily(plan.indexDir); err != nil || !scan.matchesManifest() {
		t.Fatalf("the family is not whole after the failed publish: %v", err)
	}
	if found("zqu1") != 1 || found("zqu2old") != 1 {
		t.Fatal("the failed publish lost a file of the live uncommitted family")
	}
	if err := syscall.Chflags(shards[1], 0); err != nil {
		t.Fatal(err)
	}
	if err := build(); err != nil {
		t.Fatal(err)
	}
	if found("zqu2new") != 1 || found("zqu2old") != 0 || found("zqu1") != 0 {
		t.Fatal("the next build did not publish the uncommitted change")
	}
}

// TestNoDirtyCleanKeepsAFamilyWithAnImmutableShard changes an uncommitted file
// twice, so a later shard holds its first copy under a tombstone record. It
// makes that shard immutable and commits every file. With no dirty file, the
// build removes the uncommitted family. The move fails and moves the family
// back: every shard and record stays, and the first copy is not found. With
// the flag cleared, the next build removes the family.
func TestNoDirtyCleanKeepsAFamilyWithAnImmutableShard(t *testing.T) {
	requireTools(t)
	swapForTest(t, &shardMax, 1024)
	dir := initGitRepo(t, "app.go", "package main\n// zqcleanapp\n")
	ctx := t.Context()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u1.go", "package main\n// zqcleanu1\n"+strings.Repeat("// filler line of u1\n", 100))
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u2.go", "package main\n// zqcleanfirst\n")
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u2.go", "package main\n// zqcleansecond\n")
	reindexGit(t, ctx, paths, plan)
	family := func() []string { return repositoryFamilyFiles(plan.indexDir, repoUncommitted) }
	before := family()
	shards := repositoryShardFiles(plan.indexDir, repoUncommitted)
	if len(shards) != 3 || len(before) <= len(shards) {
		t.Fatalf("uncommitted family %v, want 3 shards and their records", before)
	}
	if err := syscall.Chflags(shards[1], 0x2 /* UF_IMMUTABLE */); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Chflags(shards[1], 0) })
	gitRunIn(t, dir, "add", "-A")
	gitRunIn(t, dir, "commit", "-q", "-m", "commit the dirty files")
	build := func() error {
		state := mustGitRepoStateIn(t, ctx, paths.RepoDir)
		return runIndexingWithCache(ctx, paths, plan.corpusStore, state, gitCorpusStateHash(paths, state))
	}
	found := func(pattern string) int {
		t.Helper()
		files, err := searchPlannedCorpusForTest(ctx, plan, pattern)
		if err != nil {
			t.Fatal(err)
		}
		return len(files)
	}
	if err := build(); err == nil {
		t.Fatal("the build removed an immutable uncommitted shard")
	}
	if after := family(); !slices.Equal(after, before) {
		t.Fatalf("uncommitted family %v after the failed removal, want %v", after, before)
	}
	if found("zqcleanfirst") != 0 || found("zqcleansecond") != 1 {
		t.Fatal("the failed removal changed what the uncommitted family serves")
	}
	if err := syscall.Chflags(shards[1], 0); err != nil {
		t.Fatal(err)
	}
	if err := build(); err != nil {
		t.Fatal(err)
	}
	if after := family(); len(after) != 0 || found("zqcleansecond") != 1 || found("zqcleanfirst") != 0 {
		t.Fatalf("the next build left the uncommitted family %v or lost the committed file", after)
	}
}

// TestCommittedPublishIsRecordedWhenTheUncommittedStepFails commits a change
// while an uncommitted shard is immutable. The dirty files change so that the
// build swaps the uncommitted family. The committed family publishes, then the
// uncommitted swap fails. The build writes the records of the new committed
// family right after its publish. So the next builds keep its shard, with no
// full committed rebuild, during the failure and after the flag clears.
func TestCommittedPublishIsRecordedWhenTheUncommittedStepFails(t *testing.T) {
	requireTools(t)
	swapForTest(t, &shardMax, 1024)
	dir := initGitRepo(t, "app.go", "package main\n// zqrecordc1\n")
	ctx := t.Context()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u1.go", "package main\n// zqrecordu1\n"+strings.Repeat("// filler line of u1\n", 100))
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u2.go", "package main\n// zqrecordold\n")
	reindexGit(t, ctx, paths, plan)
	shards := repositoryShardFiles(plan.indexDir, repoUncommitted)
	if len(shards) != 2 {
		t.Fatalf("uncommitted shards %v, want 2", shards)
	}
	if err := syscall.Chflags(shards[1], 0x2 /* UF_IMMUTABLE */); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Chflags(shards[1], 0) })
	writeFileAt(t, dir, "app.go", "package main\n// zqrecordc2 a longer committed line\n")
	gitRunIn(t, dir, "add", "app.go")
	gitRunIn(t, dir, "commit", "-q", "-m", "second")
	// Removing u1.go tombstones most of the family, so the uncommitted step
	// swaps a full family in.
	if err := os.Remove(filepath.Join(dir, "u1.go")); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, dir, "u2.go", "package main\n// zqrecordnew\n")
	build := func() error {
		state := mustGitRepoStateIn(t, ctx, paths.RepoDir)
		return runIndexingWithCache(ctx, paths, plan.corpusStore, state, gitCorpusStateHash(paths, state))
	}
	committedShard := func() os.FileInfo {
		t.Helper()
		for _, file := range familyShardFilesForTest(t, plan.indexDir, familyCommitted) {
			if strings.HasSuffix(file, ".zoekt") {
				info, err := os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				return info
			}
		}
		t.Fatal("no committed shard")
		return nil
	}
	if err := build(); err == nil {
		t.Fatal("the build published over an immutable uncommitted shard")
	}
	head := strings.TrimSpace(gitOutputIn(t, dir, "rev-parse", "HEAD"))
	if recorded, ok := readCommittedGitState(plan.cacheDir); !ok || string(recorded.head) != head {
		t.Fatalf("the committed state names %q (ok=%v) after the failed build, want the published %q", recorded.head, ok, head)
	}
	published := committedShard()
	if err := build(); err == nil {
		t.Fatal("the second build published over an immutable uncommitted shard")
	}
	if !os.SameFile(published, committedShard()) {
		t.Fatal("a build during the failure rebuilt the committed family")
	}
	if err := syscall.Chflags(shards[1], 0); err != nil {
		t.Fatal(err)
	}
	if err := build(); err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(published, committedShard()) {
		t.Fatal("the build after the failure rebuilt the committed family")
	}
	files, err := searchPlannedCorpusForTest(ctx, plan, "zqrecordnew")
	if err != nil || len(files) != 1 {
		t.Fatalf("the uncommitted change is not published: files=%d err=%v", len(files), err)
	}
}

// TestEarlyRecordKeepsADamagedUncommittedFamilyUnnamed removes a record of
// the uncommitted family, so the family does not match the family manifest.
// It makes an uncommitted shard immutable and commits a change. The committed
// family publishes and is recorded, then the uncommitted swap fails. The
// record names no uncommitted member of a family that did not match, so the
// next build still sees the damage.
func TestEarlyRecordKeepsADamagedUncommittedFamilyUnnamed(t *testing.T) {
	requireTools(t)
	swapForTest(t, &shardMax, 1024)
	dir := initGitRepo(t, "app.go", "package main\n// zqdamagec1\n")
	ctx := t.Context()
	paths, plan := planGitTestCorpus(t, dir)
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u1.go", "package main\n// zqdamageu1\n"+strings.Repeat("// filler line of u1\n", 100))
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u2.go", "package main\n// zqdamagefirst\n")
	reindexGit(t, ctx, paths, plan)
	writeFileAt(t, dir, "u2.go", "package main\n// zqdamagesecond\n")
	reindexGit(t, ctx, paths, plan)
	shards := repositoryShardFiles(plan.indexDir, repoUncommitted)
	metas, _ := filepath.Glob(filepath.Join(plan.indexDir, repoUncommitted+"_v*.zoekt.meta"))
	if len(shards) < 2 || len(metas) == 0 {
		t.Fatalf("uncommitted shards %v and records %v, want two shards and a record", shards, metas)
	}
	if err := os.Remove(metas[0]); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chflags(shards[1], 0x2 /* UF_IMMUTABLE */); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Chflags(shards[1], 0) })
	writeFileAt(t, dir, "app.go", "package main\n// zqdamagec2 a longer committed line\n")
	gitRunIn(t, dir, "add", "app.go")
	gitRunIn(t, dir, "commit", "-q", "-m", "second")
	state := mustGitRepoStateIn(t, ctx, paths.RepoDir)
	if err := runIndexingWithCache(ctx, paths, plan.corpusStore, state, gitCorpusStateHash(paths, state)); err == nil {
		t.Fatal("the build published over an immutable uncommitted shard")
	}
	scan, err := scanFamily(plan.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	if scan.matchesManifest() {
		t.Fatal("the record after the committed publish names a damaged uncommitted family")
	}
	if !scan.matchesManifestFamily(familyCommitted) {
		t.Fatal("the record does not name the published committed family")
	}
}
