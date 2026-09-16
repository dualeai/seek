package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	cliProcessHelperEnv  = "SEEK_TEST_CLI_PROCESS_HELPER"
	cliProcessTestMarker = "1"
)

type cliProcessResult struct {
	stdout string
	stderr string
	code   int
}

func TestCLIProcessHelper(t *testing.T) {
	if os.Getenv(cliProcessHelperEnv) != cliProcessTestMarker {
		return
	}
	os.Args = append([]string{"seek"}, flag.Args()...)
	main()
	os.Exit(0)
}

func TestCLIProcessErrorsAndExitCodes(t *testing.T) {
	empty := t.TempDir()
	cases := []struct {
		name           string
		args           []string
		dir            string
		wantStderr     string
		stderrPrefix   string
		stderrContains []string
		code           int
	}{
		{
			name:         "query syntax",
			args:         []string{"("},
			dir:          empty,
			stderrPrefix: "seek: invalid query \"(\": ",
			code:         2,
		},
		{
			name:           "unknown flag fallback",
			args:           []string{"--unknown", "needle"},
			dir:            empty,
			stderrPrefix:   "seek: ",
			stderrContains: []string{"--unknown"},
			code:           2,
		},
		{
			name:           "removed rerank flag",
			args:           []string{"--rerank", "needle"},
			dir:            empty,
			stderrPrefix:   "seek: ",
			stderrContains: []string{"unknown flag: --rerank"},
			code:           2,
		},
		{
			name:       "no arguments",
			dir:        empty,
			wantStderr: "seek: missing query (try 'seek --help' for usage)\n",
			code:       2,
		},
		{
			name: "no match",
			args: []string{"no_match_process_marker", empty},
			dir:  empty,
			code: 1,
		},
		{
			name: "gcc query",
			args: []string{"gcc", empty},
			dir:  empty,
			code: 1,
		},
		{
			name: "misspelled gc query",
			args: []string{"garbage-colect", empty},
			dir:  empty,
			code: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := runCLIProcess(t, tc.dir, tc.args, nil)
			if result.code != tc.code {
				t.Fatalf("stdout=%q stderr=%q code=%d, want code=%d", result.stdout, result.stderr, result.code, tc.code)
			}
			if result.stdout != "" {
				t.Fatalf("stdout=%q, want empty", result.stdout)
			}
			if tc.wantStderr != "" && result.stderr != tc.wantStderr {
				t.Fatalf("stderr=%q, want %q", result.stderr, tc.wantStderr)
			}
			if tc.stderrPrefix != "" {
				if !strings.HasPrefix(result.stderr, tc.stderrPrefix) {
					t.Fatalf("stderr=%q, want prefix %q", result.stderr, tc.stderrPrefix)
				}
				if strings.TrimSpace(strings.TrimPrefix(result.stderr, tc.stderrPrefix)) == "" {
					t.Fatalf("stderr=%q, want detail after %q", result.stderr, tc.stderrPrefix)
				}
			}
			for _, want := range tc.stderrContains {
				if !strings.Contains(result.stderr, want) {
					t.Errorf("stderr=%q, want %q", result.stderr, want)
				}
			}
			if tc.wantStderr == "" && tc.stderrPrefix == "" && len(tc.stderrContains) == 0 && result.stderr != "" {
				t.Fatalf("stderr=%q, want empty", result.stderr)
			}
		})
	}
}

func TestCLIProcess_MissingGitDoesNotReturnNoMatch(t *testing.T) {
	repo := initGitRepo(t, "note.txt", "missing_git_process_marker\n")
	result := runCLIProcess(t, repo, []string{"missing_git_process_marker"}, []string{
		"PATH=" + t.TempDir(),
	})
	want := "seek: Git is required but was not found\n" +
		"hint: install Git and ensure git is on PATH\n"
	if result.stdout != "" || result.stderr != want || result.code != 2 {
		t.Fatalf("stdout=%q stderr=%q code=%d, want empty stdout, stderr=%q, code=2", result.stdout, result.stderr, result.code, want)
	}
}

func TestCLIProcess_VerboseMissingGitKeepsHintAndDetail(t *testing.T) {
	repo := initGitRepo(t, "note.txt", "missing_git_verbose_process_marker\n")
	result := runCLIProcess(t, repo, []string{"--verbose", "missing_git_verbose_process_marker"}, []string{
		"PATH=" + t.TempDir(),
	})
	wantPrefix := "seek: Git is required but was not found\n" +
		"hint: install Git and ensure git is on PATH\n"
	if result.stdout != "" || result.code != 2 || !strings.HasPrefix(result.stderr, wantPrefix) {
		t.Fatalf("stdout=%q stderr=%q code=%d, want empty stdout, stderr prefix=%q, code=2", result.stdout, result.stderr, result.code, wantPrefix)
	}
	for _, want := range []string{"level=ERROR", `msg="Command failed"`, `error="git corpus root=`} {
		if !strings.Contains(result.stderr, want) {
			t.Fatalf("stderr=%q, want structured detail %q", result.stderr, want)
		}
	}
}

func TestCLIProcess_FolderCapExplainsLimitAndRemedy(t *testing.T) {
	folder := t.TempDir()
	const fileCount = maxFolderIndexedBytes/maxIndexedDocumentBytes + 1
	for i := int64(0); i < fileCount; i++ {
		writeSparseFile(t, filepath.Join(folder, fmt.Sprintf("f%03d.bin", i)), maxIndexedDocumentBytes)
	}

	resolvedFolder, err := filepath.EvalSymlinks(folder)
	if err != nil {
		t.Fatal(err)
	}
	result := runCLIProcess(t, t.TempDir(), []string{"needle", folder}, nil)
	want := "seek: cannot index " + strconv.Quote(filepath.Clean(resolvedFolder)) +
		": the total size of indexable files exceeds the 10 GiB folder limit\n" +
		"hint: pass smaller paths after the query; file: and -file: filters apply after indexing\n"
	if result.stdout != "" || result.stderr != want || result.code != 2 {
		t.Fatalf("stdout=%q stderr=%q code=%d, want empty stdout, stderr=%q, code=2", result.stdout, result.stderr, result.code, want)
	}
}

func TestCLIProcess_VerboseFlagParseErrors(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		structured bool
		wantDetail string
	}{
		{
			name:       "verbose",
			args:       []string{"--verbose", "--unknown", "needle"},
			structured: true,
			wantDetail: "--unknown",
		},
		{
			name:       "verbose false",
			args:       []string{"--verbose=false", "--unknown", "needle"},
			wantDetail: "--unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := runCLIProcess(t, t.TempDir(), tc.args, nil)
			if result.stdout != "" || result.code != 2 {
				t.Fatalf("stdout=%q stderr=%q code=%d, want empty stdout and code 2", result.stdout, result.stderr, result.code)
			}
			if got := strings.Contains(result.stderr, "level=ERROR"); got != tc.structured {
				t.Fatalf("structured=%v, want %v; stderr=%q", got, tc.structured, result.stderr)
			}
			if !strings.Contains(result.stderr, tc.wantDetail) {
				t.Fatalf("stderr=%q, want detail %q", result.stderr, tc.wantDetail)
			}
			if !tc.structured && !strings.HasPrefix(result.stderr, "seek: ") {
				t.Fatalf("plain stderr=%q, want Seek prefix", result.stderr)
			}
		})
	}
}

func TestCLIProcess_MissingAutoDetectedCtagsIsActionable(t *testing.T) {
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "note.txt"), []byte("ctags_auto_process_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := runCLIProcess(t, t.TempDir(), []string{"ctags_auto_process_marker", folder}, []string{
		"PATH=" + t.TempDir(),
	})
	want := "seek: Universal Ctags is not available\n" +
		"hint: install Universal Ctags or set CTAGS_COMMAND=/path/to/ctags\n"
	if result.stdout != "" || result.stderr != want || result.code != 2 {
		t.Fatalf("stdout=%q stderr=%q code=%d, want empty stdout, stderr=%q, code=2", result.stdout, result.stderr, result.code, want)
	}
}

func TestCLIProcess_StaleIndexCtagsWarningIsConcise(t *testing.T) {
	folder := t.TempDir()
	file := filepath.Join(folder, "note.txt")
	if err := os.WriteFile(file, []byte("stale_ctags_marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	first := runCLIProcessWithCache(t, cacheDir, t.TempDir(), []string{"stale_ctags_marker", folder}, nil)
	if first.code != 0 || !strings.Contains(first.stdout, "stale_ctags_marker") || first.stderr != "" {
		t.Fatalf("initial stdout=%q stderr=%q code=%d, want clean indexed match", first.stdout, first.stderr, first.code)
	}
	if err := os.WriteFile(file, []byte("stale_ctags_marker\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(t.TempDir(), "ctags")
	if err := os.WriteFile(command, []byte("not executable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := runCLIProcessWithCache(t, cacheDir, t.TempDir(), []string{"stale_ctags_marker", folder}, []string{
		"CTAGS_COMMAND=" + command,
	})
	if result.code != 0 || !strings.Contains(result.stdout, "stale_ctags_marker") {
		t.Fatalf("stdout=%q stderr=%q code=%d, want successful stale match", result.stdout, result.stderr, result.code)
	}
	for _, want := range []string{
		"level=WARN",
		"Index update failed; using the existing index",
		"cannot use Universal Ctags command",
		"set CTAGS_COMMAND to an executable Universal Ctags path",
	} {
		if !strings.Contains(result.stderr, want) {
			t.Errorf("stderr missing %q: %s", want, result.stderr)
		}
	}
	for _, detail := range []string{folder, cacheDir, "permission denied", "Index update failure detail"} {
		if strings.Contains(result.stderr, detail) {
			t.Errorf("stderr exposes detail %q: %s", detail, result.stderr)
		}
	}
}

func TestCLIProcess_GCRemainsSubcommand(t *testing.T) {
	for _, command := range []string{"gc", "garbage-collect"} {
		t.Run(command, func(t *testing.T) {
			result := runCLIProcess(t, t.TempDir(), []string{command, "--help"}, nil)
			if result.code != 0 || result.stderr != "" ||
				!strings.Contains(result.stdout, "Delete search indexes that have not been used") ||
				!strings.Contains(result.stdout, "run cleanup even if it ran recently") ||
				strings.Contains(result.stdout, ".last-gc") ||
				!strings.Contains(result.stdout, "--dry-run") {
				t.Fatalf("stdout=%q stderr=%q code=%d, want gc help on stdout", result.stdout, result.stderr, result.code)
			}
		})
	}
}

func TestCLIProcess_GCDryRunDoesNotCreateCache(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "missing-cache")
	result := runCLIProcessWithCache(
		t,
		cacheDir,
		t.TempDir(),
		[]string{"gc", "--dry-run"},
		nil,
	)
	if result.code != 0 || result.stderr != "" ||
		!strings.Contains(result.stdout, "no corpora") {
		t.Fatalf("stdout=%q stderr=%q code=%d, want empty-cache plan", result.stdout, result.stderr, result.code)
	}
	if _, err := os.Lstat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("dry-run process created missing cache root: %v", err)
	}
}

func TestCLIProcess_LexicalOnlySkipsSemanticCacheAndModel(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// alpha beta\n")
	cacheDir := t.TempDir()
	result := runCLIProcessWithCache(
		t,
		cacheDir,
		t.TempDir(),
		[]string{"--verbose", "--lexical-only", "alpha beta", folder},
		nil,
	)
	if result.code != 0 || !strings.Contains(result.stdout, "## app.go") {
		t.Fatalf(
			"lexical-only stdout=%q stderr=%q code=%d",
			result.stdout,
			result.stderr,
			result.code,
		)
	}
	for _, marker := range []string{
		"Built semantic index",
		"semantic inference",
		"USearch",
		"LateOn",
	} {
		if strings.Contains(result.stderr, marker) {
			t.Errorf("lexical-only stderr contains %q: %s", marker, result.stderr)
		}
	}
	semanticPaths := cachePathsMatching(t, cacheDir, func(name string) bool {
		return name == joinedGenerationFile ||
			strings.HasPrefix(name, semanticGenerationPrefix) ||
			name == "semantic" || name == "reranker"
	})
	if len(semanticPaths) != 0 {
		t.Fatalf("lexical-only command created semantic cache paths: %v", semanticPaths)
	}
}

// TestCLIProcess_QueryShapeDecidesSemanticCache checks the user-visible rule
// the README states: Seek builds meaning-based data only for a search that can
// read it. An exact word cannot reach it, a description can.
//
// This sits at process level beside the --lexical-only test because it makes
// the same kind of claim: a command either leaves semantic data in the cache or
// it does not. The in-process tests cover why; this covers what a user sees.
func TestCLIProcess_QueryShapeDecidesSemanticCache(t *testing.T) {
	requireTools(t)
	folder := t.TempDir()
	writeFileAt(t, folder, "app.go", "package sample\n// alpha beta gamma describes the sample\n")

	// wantCode guards this table against a vacuous pass: a case that expects no
	// results and no vectors would also hold if the command crashed, so every
	// case pins the documented exit code. 0 is a match, 1 is no match, and 2 is
	// an error that must never appear here.
	for _, testCase := range []struct {
		name        string
		query       string
		wantCode    int
		wantResults bool
		wantVectors bool
	}{
		{name: "exact word", query: "alpha", wantCode: 0, wantResults: true, wantVectors: false},
		{name: "symbol lookup", query: "sym:Missing", wantCode: 1, wantResults: false, wantVectors: false},
		{name: "description", query: "describes the sample", wantCode: 0, wantResults: true, wantVectors: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			result := runCLIProcessWithCache(
				t,
				cacheDir,
				t.TempDir(),
				[]string{testCase.query, folder},
				nil,
			)
			if result.code != testCase.wantCode {
				t.Fatalf(
					"exit code=%d, want %d: stdout=%q stderr=%q",
					result.code,
					testCase.wantCode,
					result.stdout,
					result.stderr,
				)
			}
			if testCase.wantResults != strings.Contains(result.stdout, "## app.go") {
				t.Fatalf(
					"results=%t, want %t: stdout=%q stderr=%q",
					!testCase.wantResults,
					testCase.wantResults,
					result.stdout,
					result.stderr,
				)
			}
			found := cachePathsMatching(t, cacheDir, func(name string) bool {
				return strings.HasPrefix(name, semanticGenerationPrefix)
			})
			if testCase.wantVectors && len(found) == 0 {
				t.Fatal("a search that can read meaning-based data built none")
			}
			if !testCase.wantVectors && len(found) != 0 {
				t.Fatalf("a search that cannot read meaning-based data built %v", found)
			}
		})
	}
}

// cachePathsMatching returns every path under cacheDir, relative to it, whose
// base name the predicate accepts. Both semantic-cache assertions in this file
// walk the same tree and differ only in which names count.
func cachePathsMatching(tb testing.TB, cacheDir string, match func(name string) bool) []string {
	tb.Helper()
	var found []string
	err := filepath.WalkDir(cacheDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !match(entry.Name()) {
			return nil
		}
		relative, relErr := filepath.Rel(cacheDir, path)
		if relErr != nil {
			return relErr
		}
		found = append(found, relative)
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return found
}

func runCLIProcess(t *testing.T, dir string, args, extraEnv []string) cliProcessResult {
	t.Helper()
	return runCLIProcessWithCache(t, t.TempDir(), dir, args, extraEnv)
}

func runCLIProcessWithCache(t *testing.T, cacheDir, dir string, args, extraEnv []string) cliProcessResult {
	t.Helper()
	commandArgs := append([]string{"-test.run=^TestCLIProcessHelper$", "--"}, args...)
	cmd := exec.Command(os.Args[0], commandArgs...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	overrides := []string{
		cliProcessHelperEnv + "=" + cliProcessTestMarker,
		"SEEK_CACHE_DIR=" + cacheDir,
		"CTAGS_COMMAND=",
	}
	overrides = append(overrides, extraEnv...)
	cmd.Env = append(cmd.Environ(), overrides...)
	runErr := cmd.Run()
	result := cliProcessResult{stdout: stdout.String(), stderr: stderr.String()}
	if runErr == nil {
		return result
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("run CLI helper: %v", runErr)
	}
	result.code = exitErr.ExitCode()
	return result
}
