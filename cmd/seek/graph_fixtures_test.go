package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// graphFixture builds folder and Git trees for graph-level tests. Global and
// system Git config are isolated, so runner config (for example git-lfs) does
// not change results.
type graphFixture struct {
	t            *testing.T
	root         string
	markers      string
	globalConfig string
}

// newGraphFixture returns a fixture with an empty root and a test-only global
// Git config.
func newGraphFixture(t *testing.T) *graphFixture {
	t.Helper()
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeFixtureFile(t, global, "[user]\n\tname = Test\n\temail = test@test.com\n[init]\n\tdefaultBranch = main\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return &graphFixture{t: t, root: t.TempDir(), markers: t.TempDir(), globalConfig: global}
}

func (f *graphFixture) path(rel string) string {
	return filepath.Join(f.root, filepath.FromSlash(rel))
}

// GitRepo creates a repository at rel with one committed file.
func (f *graphFixture) GitRepo(rel, file, content string) string {
	f.t.Helper()
	dir := f.path(rel)
	initGitRepoNoRemoteAt(f.t, dir, file, content)
	return dir
}

// graphFile writes a fixture file that holds marker and its fixture path.
func (f *graphFixture) graphFile(rel, marker string) {
	f.t.Helper()
	writeFixtureFile(f.t, f.path(rel), marker+" "+rel+"\n")
}

// graphRepo creates a repository at rel whose one committed file holds marker
// and its fixture path.
func (f *graphFixture) graphRepo(rel, file, marker string) string {
	f.t.Helper()
	return f.GitRepo(rel, file, marker+" "+rel+"/"+file+"\n")
}

// F6 builds a folder and Git graph with each edge kind of the graph
// expansion. Each file holds marker and its fixture path. It returns the files
// that a search of "." from the root finds, each with its corpus tag.
func (f *graphFixture) F6(marker string) map[string]string {
	f.t.Helper()
	repoA := f.graphRepo("repoA", "a.txt", marker)
	writeFixtureFile(f.t, filepath.Join(repoA, ".gitignore"), "ignored/\n")
	f.graphFile("repoA/sub/folder/b.txt", marker)
	gitRunIn(f.t, repoA, "add", ".gitignore", "sub/folder/b.txt")
	gitRunIn(f.t, repoA, "commit", "-m", "content")

	repoB := f.graphRepo("repoA/repoB", "b.txt", marker)
	f.graphFile("repoA/repoB/deep/d.txt", marker)
	gitRunIn(f.t, repoB, "add", "deep/d.txt")
	gitRunIn(f.t, repoB, "commit", "-m", "deep")
	f.graphRepo("repoA/sub/folder/repoD", "m.txt", marker)
	f.graphFile("repoA/ignored/loose.txt", marker)
	f.graphRepo("repoA/ignored/repoE", "m.txt", marker)

	repoC := f.graphRepo("folder1/folder2/repoC", "c.txt", marker)
	worktree := f.path("folder1/wt")
	gitRunIn(f.t, repoC, "worktree", "add", "-b", "wt", worktree)
	f.graphFile("folder1/wt/c.txt", marker)
	gitRunIn(f.t, worktree, "commit", "-am", "worktree")
	f.graphFile("folder1/wt/w.txt", marker)

	for _, rel := range []string{"loose.txt", "folder1/c.txt", ".config/x.txt", ".ssh/y.txt"} {
		f.graphFile(rel, marker)
	}
	if err := os.Symlink(repoB, f.path("folder1/link-to-repoB")); err != nil {
		f.t.Fatal(err)
	}
	return map[string]string{
		"loose.txt":                    "[folder]",
		"folder1/c.txt":                "[folder]",
		".config/x.txt":                "[folder]",
		".ssh/y.txt":                   "[folder]",
		"repoA/a.txt":                  "[git]",
		"repoA/sub/folder/b.txt":       "[git]",
		"repoA/repoB/b.txt":            "[git]",
		"repoA/repoB/deep/d.txt":       "[git]",
		"repoA/sub/folder/repoD/m.txt": "[git]",
		"folder1/folder2/repoC/c.txt":  "[git]",
		"folder1/wt/c.txt":             "[git]",
		"folder1/wt/w.txt":             "[uncommitted] [git]",
	}
}

// hostileVector names a repository setting that runs a program.
type hostileVector string

const (
	vectorFSMonitor  hostileVector = "fsmonitor"
	vectorHook       hostileVector = "hook"
	vectorHooksPath  hostileVector = "hooksPath"
	vectorGPGProgram hostileVector = "gpgProgram"
	// The filter vectors run a clean filter that the repository defines when
	// git status hashes a stat-dirty file again.
	vectorCleanFilter     hostileVector = "cleanFilter"
	vectorRequiredFilter  hostileVector = "requiredFilter"
	vectorSubmoduleFilter hostileVector = "submoduleFilter"
	// A "-c" argument splits at the first "=", so these driver names need the
	// GIT_CONFIG_COUNT overrides.
	vectorEqualsNameFilter hostileVector = "equalsNameFilter"
	vectorEmptyNameFilter  hostileVector = "emptyNameFilter"
)

// HostileRepo creates a repository at rel whose config runs a marker script
// through vector during a normal search. The committed file holds marker so
// the search must still find it.
func (f *graphFixture) HostileRepo(rel string, vector hostileVector, marker string) string {
	f.t.Helper()
	file := string(vector) + ".txt"
	dir := f.GitRepo(rel, file, marker+"\n")
	script := f.markerScript(string(vector))
	switch vector {
	case vectorFSMonitor:
		gitRunIn(f.t, dir, "config", "core.fsmonitor", script)
	case vectorHook:
		writeExecutable(f.t, filepath.Join(dir, ".git", "hooks", "post-index-change"), markerScriptBody(f.marker(string(vector))))
		f.makeStatDirty(dir, file)
	case vectorHooksPath:
		hooks := filepath.Join(f.markers, "hooks-"+rel)
		writeExecutable(f.t, filepath.Join(hooks, "post-index-change"), markerScriptBody(f.marker(string(vector))))
		gitRunIn(f.t, dir, "config", "core.hooksPath", hooks)
		f.makeStatDirty(dir, file)
	case vectorGPGProgram:
		f.forgeSignedHead(dir)
		gitRunIn(f.t, dir, "config", "gpg.program", script)
		gitRunIn(f.t, dir, "config", "log.showSignature", "true")
	case vectorCleanFilter, vectorRequiredFilter:
		f.commitAttributes(dir, "*.txt filter=pwn\n")
		gitRunIn(f.t, dir, "config", "filter.pwn.clean", script)
		if vector == vectorRequiredFilter {
			gitRunIn(f.t, dir, "config", "filter.pwn.required", "true")
		}
		f.makeStatDirty(dir, file)
	case vectorEqualsNameFilter, vectorEmptyNameFilter:
		name := "a=b"
		if vector == vectorEmptyNameFilter {
			name = ""
		}
		f.commitAttributes(dir, "*.txt filter="+name+"\n")
		gitRunIn(f.t, dir, "config", "filter."+name+".clean", script)
		f.makeStatDirty(dir, file)
	case vectorSubmoduleFilter:
		source := f.t.TempDir()
		initGitRepoNoRemoteAt(f.t, source, file, marker+"\n")
		f.commitAttributes(source, "*.txt filter=subpwn\n")
		gitRunIn(f.t, dir, "-c", "protocol.file.allow=always", "submodule", "add", source, "sub")
		gitRunIn(f.t, dir, "commit", "-m", "add submodule")
		sub := filepath.Join(dir, "sub")
		gitRunIn(f.t, sub, "config", "filter.subpwn.clean", script)
		f.makeStatDirty(sub, file)
	default:
		f.t.Fatalf("unknown hostile vector %q", vector)
	}
	return dir
}

// TrustedGlobalDriver creates a repository whose files use a filter driver
// from the global config. Seek must keep such a driver on (Git LFS works this
// way). The driver passes content through and touches its marker.
func (f *graphFixture) TrustedGlobalDriver(rel, marker string) string {
	f.t.Helper()
	const file = "globalDriver.txt"
	dir := f.GitRepo(rel, file, marker+"\n")
	f.commitAttributes(dir, "*.txt filter=trusted\n")
	script := filepath.Join(f.markers, "bin", "globalDriver.sh")
	writeExecutable(f.t, script, markerScriptBody(f.marker("globalDriver"))+"cat\n")
	config, err := os.OpenFile(f.globalConfig, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = config.Close() }()
	if _, err := config.WriteString("[filter \"trusted\"]\n\tclean = " + script + "\n"); err != nil {
		f.t.Fatal(err)
	}
	f.makeStatDirty(dir, file)
	return dir
}

// commitAttributes commits a .gitattributes file before any driver exists, so
// the commit itself runs no filter.
func (f *graphFixture) commitAttributes(dir, content string) {
	f.t.Helper()
	writeFixtureFile(f.t, filepath.Join(dir, ".gitattributes"), content)
	gitRunIn(f.t, dir, "add", ".gitattributes")
	gitRunIn(f.t, dir, "commit", "-m", "add attributes")
}

// makeStatDirty changes the mtime of a committed file so git status refreshes
// and writes the index, which runs index hooks.
func (f *graphFixture) makeStatDirty(dir, file string) {
	f.t.Helper()
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, file), future, future); err != nil {
		f.t.Fatal(err)
	}
}

// forgeSignedHead replaces HEAD with a commit that carries a fake gpgsig
// header, so a signature check calls gpg.program.
func (f *graphFixture) forgeSignedHead(dir string) {
	f.t.Helper()
	tree := strings.TrimSpace(gitOutputIn(f.t, dir, "rev-parse", "HEAD^{tree}"))
	object := "tree " + tree + "\n" +
		"author Test <test@test.com> 1700000000 +0000\n" +
		"committer Test <test@test.com> 1700000000 +0000\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n fake\n -----END PGP SIGNATURE-----\n" +
		"\nsigned\n"
	cmd := exec.Command("git", "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(object)
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("hash-object: %v", err)
	}
	gitRunIn(f.t, dir, "update-ref", "HEAD", strings.TrimSpace(string(out)))
}

// marker returns the file that the marker script name creates when it runs.
func (f *graphFixture) marker(name string) string {
	return filepath.Join(f.markers, "fired", name)
}

// markerScript writes the marker script name and returns its path.
func (f *graphFixture) markerScript(name string) string {
	f.t.Helper()
	script := filepath.Join(f.markers, "bin", name+".sh")
	writeExecutable(f.t, script, markerScriptBody(f.marker(name)))
	return script
}

// markersFired returns the names of the marker scripts that ran.
func (f *graphFixture) markersFired() []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.markers, "fired"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// markerScriptBody returns a shell script that creates the file marker.
func markerScriptBody(marker string) string {
	return "#!/bin/sh\nmkdir -p '" + filepath.Dir(marker) + "'\ntouch '" + marker + "'\n"
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	writeFixtureFile(t, path, content)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// localGitConfig returns the repository-local config entries of dir.
func localGitConfig(t *testing.T, dir string) string {
	t.Helper()
	return gitOutputIn(t, dir, "config", "--local", "--list")
}

// gitShimSubcommand is a shell snippet for Git shims in tests. It sets
// $subcommand to the Git subcommand: Seek puts -c settings and global flags
// before it (newGitCommand).
const gitShimSubcommand = `subcommand=
skip=false
for arg do
  if [ "$skip" = true ]; then
    skip=false
    continue
  fi
  case "$arg" in
    -c) skip=true ;;
    -*) ;;
    *) subcommand=$arg; break ;;
  esac
done
`
