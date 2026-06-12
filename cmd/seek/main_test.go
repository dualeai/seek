package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestErrNoMatch_IsDistinct(t *testing.T) {
	// errNoMatch must not be confused with generic errors.
	generic := errors.New("something went wrong")
	if errors.Is(generic, errNoMatch) {
		t.Error("generic error should not match errNoMatch")
	}
}

func TestErrNoMatch_WrappedIsDetectable(t *testing.T) {
	// Even when wrapped, errors.Is must still detect errNoMatch so that
	// callers (main) can reliably map it to exit code 1.
	wrapped := fmt.Errorf("search: %w", errNoMatch)
	if !errors.Is(wrapped, errNoMatch) {
		t.Error("wrapped errNoMatch should be detectable via errors.Is")
	}
}

func TestExitCodeForError(t *testing.T) {
	if got := exitCodeForError(nil); got != 0 {
		t.Fatalf("nil error exit code: got %d, want 0", got)
	}
	if got := exitCodeForError(errNoMatch); got != 1 {
		t.Fatalf("no-match exit code: got %d, want 1", got)
	}
	if got := exitCodeForError(fmt.Errorf("wrapped: %w", errNoMatch)); got != 1 {
		t.Fatalf("wrapped no-match exit code: got %d, want 1", got)
	}
	if got := exitCodeForError(errors.New("boom")); got != 2 {
		t.Fatalf("generic error exit code: got %d, want 2", got)
	}
}

func TestParseCLIArgs_QueryOnly(t *testing.T) {
	got, err := parseCLIArgs([]string{"needle"})
	if err != nil {
		t.Fatalf("parseCLIArgs: %v", err)
	}
	want := cliOptions{query: "needle"}
	assertCLIOptions(t, got, want)
}

func TestParseCLIArgs_FlagsBeforeQuery(t *testing.T) {
	got, err := parseCLIArgs([]string{
		"-v",
		"--limit=5",
		"--max-matches",
		"2",
		"lang:go needle",
		"./cmd",
		"-literal-path",
	})
	if err != nil {
		t.Fatalf("parseCLIArgs: %v", err)
	}

	want := cliOptions{
		verbose:    true,
		limit:      5,
		maxMatches: 2,
		query:      "lang:go needle",
		paths:      []string{"./cmd", "-literal-path"},
	}
	assertCLIOptions(t, got, want)
}

func TestParseCLIArgs_QueryMayStartWithDash(t *testing.T) {
	got, err := parseCLIArgs([]string{"-file:test", "./cmd"})
	if err != nil {
		t.Fatalf("parseCLIArgs: %v", err)
	}

	want := cliOptions{query: "-file:test", paths: []string{"./cmd"}}
	assertCLIOptions(t, got, want)
}

func TestParseCLIArgs_DoubleDashBeforeQuery(t *testing.T) {
	got, err := parseCLIArgs([]string{"--", "-file:test", "./cmd"})
	if err != nil {
		t.Fatalf("parseCLIArgs: %v", err)
	}

	want := cliOptions{query: "-file:test", paths: []string{"./cmd"}}
	assertCLIOptions(t, got, want)
}

func TestParseCLIArgs_VersionWithoutQuery(t *testing.T) {
	got, err := parseCLIArgs([]string{"--version"})
	if err != nil {
		t.Fatalf("parseCLIArgs: %v", err)
	}

	want := cliOptions{showVersion: true}
	assertCLIOptions(t, got, want)
}

func TestParseCLIArgs_KnownFlagMissingValue(t *testing.T) {
	_, err := parseCLIArgs([]string{"--limit"})
	if err == nil {
		t.Fatal("expected missing value error")
	}
	if !strings.Contains(err.Error(), "--limit requires a value") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseCLIArgs_KnownFlagDoubleDashIsBadValue(t *testing.T) {
	_, err := parseCLIArgs([]string{"--limit", "--", "needle"})
	if err == nil {
		t.Fatal("expected bad value error")
	}
	if !strings.Contains(err.Error(), "--limit requires an integer value") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseCLIArgs_KnownFlagBadValue(t *testing.T) {
	_, err := parseCLIArgs([]string{"--max-matches=nope"})
	if err == nil {
		t.Fatal("expected bad value error")
	}
	if !strings.Contains(err.Error(), "--max-matches requires an integer value") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseCLIArgs_UnknownDashBeforeQueryIsQuery(t *testing.T) {
	got, err := parseCLIArgs([]string{"--unknown", "path"})
	if err != nil {
		t.Fatalf("parseCLIArgs: %v", err)
	}

	want := cliOptions{query: "--unknown", paths: []string{"path"}}
	assertCLIOptions(t, got, want)
}

func assertCLIOptions(t *testing.T, got, want cliOptions) {
	t.Helper()
	if got.showVersion != want.showVersion {
		t.Fatalf("showVersion: got %v, want %v", got.showVersion, want.showVersion)
	}
	if got.verbose != want.verbose {
		t.Fatalf("verbose: got %v, want %v", got.verbose, want.verbose)
	}
	if got.limit != want.limit {
		t.Fatalf("limit: got %d, want %d", got.limit, want.limit)
	}
	if got.maxMatches != want.maxMatches {
		t.Fatalf("maxMatches: got %d, want %d", got.maxMatches, want.maxMatches)
	}
	if got.query != want.query {
		t.Fatalf("query: got %q, want %q", got.query, want.query)
	}
	if !slices.Equal(got.paths, want.paths) {
		t.Fatalf("paths: got %#v, want %#v", got.paths, want.paths)
	}
}
