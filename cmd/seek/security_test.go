package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSearchRunsNoRepoConfiguredCommands searches a parent folder that holds
// hostile repositories. A search must run no program that a repository
// configures, write no repository config, and still find every file.
func TestSearchRunsNoRepoConfiguredCommands(t *testing.T) {
	requireTools(t)
	const marker = "zqsecuritymarker"
	f := newGraphFixture(t)
	vectors := []hostileVector{
		vectorFSMonitor, vectorHook, vectorHooksPath, vectorGPGProgram,
		vectorCleanFilter, vectorRequiredFilter, vectorSubmoduleFilter,
		vectorEqualsNameFilter, vectorEmptyNameFilter,
	}
	repos := make(map[hostileVector]string, len(vectors))
	before := make(map[hostileVector]string, len(vectors))
	for _, vector := range vectors {
		repos[vector] = f.HostileRepo(string(vector), vector, marker)
		before[vector] = localGitConfig(t, repos[vector])
	}
	trusted := f.TrustedGlobalDriver("trusted", marker)
	setTestUserCache(t)
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)

	out, err := seek(t, "--lexical-only", marker, f.root)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	// Only the driver from the global config may run.
	if fired := f.markersFired(); !slices.Equal(fired, []string{"globalDriver"}) {
		t.Fatalf("fired markers %v, want only the trusted global driver", fired)
	}
	if want := filepath.Join(trusted, "globalDriver.txt"); !strings.Contains(out, want) {
		t.Fatalf("missing %s in output:\n%s", want, out)
	}
	// A parent status must not recurse into a submodule: the submodule is its
	// own node with its own hardened status (--ignore-submodules=dirty).
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "status --porcelain=2"); n != 0 {
		t.Fatalf("parent status recursed into submodules %d times", n)
	}
	for _, vector := range vectors {
		if got := localGitConfig(t, repos[vector]); got != before[vector] {
			t.Fatalf("%s: search changed .git/config:\nbefore:\n%s\nafter:\n%s", vector, before[vector], got)
		}
		want := filepath.Join(repos[vector], string(vector)+".txt")
		if vector == vectorSubmoduleFilter {
			want = filepath.Join(repos[vector], "sub", string(vector)+".txt")
		}
		if !strings.Contains(out, want) {
			t.Fatalf("%s: missing %s in output:\n%s", vector, want, out)
		}
	}
}

// TestParseRepoGitConfig checks that parseRepoGitConfig lists each local and
// worktree filter driver once, in order, also with a dotted, an equals-sign,
// or an empty name. A key from the global config adds no driver.
func TestParseRepoGitConfig(t *testing.T) {
	out := []byte("global\x00filter.lfs.clean\ngit-lfs clean\x00" +
		"local\x00filter.pwn.clean\n/bin/x\x00" +
		"local\x00filter.lfs.smudge\nlocal-smudge\x00" +
		"worktree\x00filter.a.b.process\np\x00" +
		"local\x00filter.pwn.required\ntrue\x00" +
		"local\x00filter.a=b.clean\n/bin/x\x00" +
		"local\x00filter..clean\n/bin/x\x00")
	got := parseRepoGitConfig(out).filterDrivers
	if want := []string{"pwn", "lfs", "a.b", "a=b", ""}; !slices.Equal(got, want) {
		t.Fatalf("drivers=%q, want %q", got, want)
	}
}

// TestAppendConfigEnvKeepsCallerPairs keeps the caller's GIT_CONFIG pairs and
// numbers the filter overrides after them.
func TestAppendConfigEnvKeepsCallerPairs(t *testing.T) {
	env := []string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*"}
	got := appendConfigEnv(env, filterOverrides([]string{"a=b"}))
	want := []string{
		"PATH=/bin", "GIT_CONFIG_COUNT=5", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*",
		"GIT_CONFIG_KEY_1=filter.a=b.clean", "GIT_CONFIG_VALUE_1=",
		"GIT_CONFIG_KEY_2=filter.a=b.smudge", "GIT_CONFIG_VALUE_2=",
		"GIT_CONFIG_KEY_3=filter.a=b.process", "GIT_CONFIG_VALUE_3=",
		"GIT_CONFIG_KEY_4=filter.a=b.required", "GIT_CONFIG_VALUE_4=false",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("env=%q\nwant %q", got, want)
	}
}

// TestFilterDriverReadFailureFailsClosed makes the driver-list read fail. The
// repository's Git commands must then fail, not run with the driver on: the
// node is skipped with one warning that gives the cause.
func TestFilterDriverReadFailureFailsClosed(t *testing.T) {
	requireTools(t)
	const marker = "zqfailclosedmarker"
	f := newGraphFixture(t)
	repo := f.HostileRepo("repo", vectorCleanFilter, marker)
	installGitShim(t, "if [ \"$subcommand\" = config ]; then\n  echo forced-config-failure >&2\n  exit 128\nfi\n"+
		"exec \"$SEEK_TEST_REAL_GIT\" \"$@\"\n")
	setTestUserCache(t)

	logs := captureTestLogs(t, slog.LevelWarn)
	err := seekRoot(t.Context(), t, nil, "--lexical-only", marker, repo)
	if !errors.Is(err, errNoMatch) || !logsMention(logs.Records(), "forced-config-failure") {
		t.Fatalf("err=%v warnings=%v, want the driver-list read failure", err, logs.Records())
	}
	if fired := f.markersFired(); len(fired) != 0 {
		t.Fatalf("fired markers %v, want none", fired)
	}
}

// TestRepoFilterOverridesReadOncePerRepo runs three Git commands in one
// repository: the driver list is read once, and it holds the local driver.
func TestRepoFilterOverridesReadOncePerRepo(t *testing.T) {
	requireGit(t)
	f := newGraphFixture(t)
	dir := f.HostileRepo("repo", vectorCleanFilter, "zqonce")
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	for range 3 {
		if out, err := newGitCommand(t.Context(), dir, "rev-parse", "--git-dir").Output(); err != nil {
			t.Fatalf("rev-parse: %v %s", err, out)
		}
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if reads := strings.Count(string(data), "config --show-scope"); reads != 1 {
		t.Fatalf("driver list read %d times, want 1:\n%s", reads, data)
	}
	if config, err := readRepoGitConfig(t.Context(), dir); err != nil || !slices.Contains(config.filterDrivers, "pwn") {
		t.Fatalf("drivers=%q err=%v, want the local driver", config.filterDrivers, err)
	}
}

// TestParseRepoGitConfigPartialClone reads Git's boolean forms of
// remote.<name>.promisor, and extensions.partialclone, as a partial clone.
func TestParseRepoGitConfigPartialClone(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{out: "", want: false},
		{out: "local\x00remote.origin.promisor\ntrue\x00", want: true},
		{out: "local\x00remote.origin.promisor\nfalse\x00", want: false},
		{out: "local\x00remote.origin.promisor\nyes\x00", want: true},
		{out: "local\x00remote.origin.promisor\nOn\x00", want: true},
		{out: "local\x00remote.origin.promisor\n1\x00", want: true},
		{out: "local\x00remote.origin.promisor\n0\x00", want: false},
		{out: "local\x00remote.origin.promisor\x00", want: true},
		{out: "local\x00extensions.partialclone\norigin\x00", want: true},
	} {
		if got := parseRepoGitConfig([]byte(tc.out)).partialClone; got != tc.want {
			t.Fatalf("partialClone(%q)=%t, want %t", tc.out, got, tc.want)
		}
	}
}

// TestSearchReadsDriverListOncePerRepo searches a folder with three
// repositories: each repository's driver list is read once per command.
func TestSearchReadsDriverListOncePerRepo(t *testing.T) {
	requireTools(t)
	const marker = "zqdriverreadmarker"
	f := newGraphFixture(t)
	for _, rel := range []string{"a", "b", "c"} {
		f.graphRepo("folder/"+rel, "x.txt", marker)
	}
	setTestUserCache(t)
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	if got := graphSearch(t, marker, f.path("folder")); len(got) != 3 {
		t.Fatalf("found %v, want 3 files", got)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if reads := strings.Count(string(data), "config --show-scope"); reads != 3 {
		t.Fatalf("driver list read %d times for 3 repositories, want 3", reads)
	}
}

// TestLinkedWorktreeRunsNoRepoConfiguredCommands searches a linked worktree of
// a hostile repository. The config lives in the common directory and must stay
// off there too.
func TestLinkedWorktreeRunsNoRepoConfiguredCommands(t *testing.T) {
	requireTools(t)
	const marker = "zqworktreesecuritymarker"
	f := newGraphFixture(t)
	repo := f.HostileRepo("repo", vectorCleanFilter, marker)
	worktree := f.path("wt")
	gitRunIn(t, repo, "worktree", "add", "-q", worktree)
	f.makeStatDirty(worktree, string(vectorCleanFilter)+".txt")
	setTestUserCache(t)
	out, err := seek(t, "--lexical-only", marker, worktree)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if fired := f.markersFired(); len(fired) != 0 {
		t.Fatalf("fired markers %v, want none", fired)
	}
	if !strings.Contains(out, string(vectorCleanFilter)+".txt") {
		t.Fatalf("worktree file not found:\n%s", out)
	}
}

// TestWorktreeConfigDriverStaysOff sets a filter driver in the worktree-scope
// config (extensions.worktreeConfig). Seek turns it off like a local driver.
func TestWorktreeConfigDriverStaysOff(t *testing.T) {
	requireTools(t)
	const marker = "zqworktreeconfigmarker"
	f := newGraphFixture(t)
	const file = "worktreeConfig.txt"
	repo := f.GitRepo("repo", file, marker+"\n")
	f.commitAttributes(repo, "*.txt filter=wtpwn\n")
	gitRunIn(t, repo, "config", "extensions.worktreeConfig", "true")
	gitRunIn(t, repo, "config", "--worktree", "filter.wtpwn.clean", f.markerScript("worktreeConfig"))
	f.makeStatDirty(repo, file)
	setTestUserCache(t)
	out, err := seek(t, "--lexical-only", marker, repo)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if fired := f.markersFired(); len(fired) != 0 {
		t.Fatalf("fired markers %v, want none", fired)
	}
	if !strings.Contains(out, file) {
		t.Fatalf("missing %s in output:\n%s", file, out)
	}
}
