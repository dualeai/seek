package main

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestGitCommandHardening checks the hardening flags, the literal pathspecs,
// and the fixed environment of a Git command.
func TestGitCommandHardening(t *testing.T) {
	// A literal list: deleting a flag from gitHardeningArgs must fail this test.
	hardening := []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.sshCommand=",
		"-c", "log.showSignature=false",
		"-c", "diff.external=",
		"-c", "protocol.ext.allow=never",
		"--no-pager",
		"--no-replace-objects",
	}
	cmd := newGitCommand(t.Context(), "/repo", "status", "-z")
	if cmd.Dir != "/repo" {
		t.Fatalf("Dir=%q, want /repo", cmd.Dir)
	}
	want := append(append([]string{}, hardening...), "--literal-pathspecs", "status", "-z")
	if got := cmd.Args[1:]; !slices.Equal(got, want) {
		t.Fatalf("args=%q, want %q", got, want)
	}
	check := newGitCheckIgnoreCommand(t.Context(), "/repo", "-z", "--stdin")
	// Git rejects --literal-pathspecs for check-ignore.
	wantCheck := append(append([]string{}, hardening...), "check-ignore", "-z", "--stdin")
	if got := check.Args[1:]; !slices.Equal(got, wantCheck) {
		t.Fatalf("check-ignore args=%q, want %q", got, wantCheck)
	}
	if !slices.Contains(cmd.Env, "GIT_NO_LAZY_FETCH=1") || !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("env lacks the stable values: %q", cmd.Env)
	}
}

// TestGitCommandErrorCarriesStderr checks that a Git error carries Git's
// stderr and wraps *exec.ExitError.
func TestGitCommandErrorCarriesStderr(t *testing.T) {
	requireGit(t)
	_, err := resolveGitPaths(t.Context(), t.TempDir())
	if err == nil {
		t.Fatal("rev-parse outside a repository succeeded")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("error %q lacks Git's reason", err)
	}
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		t.Fatalf("error %v does not wrap *exec.ExitError", err)
	}
}

// TestGitCommandMissingGitIsTyped checks that a missing Git executable gives a
// gitUnavailableError.
func TestGitCommandMissingGitIsTyped(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := resolveGitPaths(t.Context(), t.TempDir())
	if _, ok := errors.AsType[*gitUnavailableError](err); !ok {
		t.Fatalf("error %v is not gitUnavailableError", err)
	}
}
