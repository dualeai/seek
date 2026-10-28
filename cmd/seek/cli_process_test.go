package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cliProcessHelperEnv set to cliProcessTestMarker makes the test binary run
// as the CLI (TestCLIProcessHelper).
const (
	cliProcessHelperEnv  = "SEEK_TEST_CLI_PROCESS_HELPER"
	cliProcessTestMarker = "1"
)

// cliProcessResult is the output and the exit code of one CLI process.
type cliProcessResult struct {
	stdout string
	stderr string
	code   int
}

// TestCLIProcessHelper is the CLI process that cliProcessCommand starts. With
// cliProcessHelperEnv set to cliProcessTestMarker, it runs main with the
// arguments after "--". In a normal test run it returns at once.
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
			// Seek removed --rerank, so it must stay an unknown flag.
			name:           "unknown flag fallback",
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

// TestCLIProcess_MissingGitSkipsTheNode runs with no Git on PATH: the Git
// node fails and the search skips it, as it does any failed node. One warning
// names the root and gives the hint; no folder content shows; the exit is 1.
func TestCLIProcess_MissingGitSkipsTheNode(t *testing.T) {
	repo := initGitRepo(t, "note.txt", "missing_git_process_marker\n")
	result := runCLIProcess(t, repo, []string{"missing_git_process_marker"}, []string{
		"PATH=" + t.TempDir(),
	})
	if result.stdout != "" || result.code != 1 || strings.Count(result.stderr, "\n") != 1 {
		t.Fatalf("stdout=%q stderr=%q code=%d, want one warning and code 1", result.stdout, result.stderr, result.code)
	}
	// No path is the same as ".", so the warning names the resolved root.
	for _, want := range []string{"level=WARN", "root=" + resolvedPath(t, repo), "Git is required but was not found", "install Git and ensure git is on PATH"} {
		if !strings.Contains(result.stderr, want) {
			t.Fatalf("stderr=%q, want %q", result.stderr, want)
		}
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
				!strings.Contains(result.stdout, "Delete the least recently used search indexes") ||
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
	if result.code != 0 || !strings.Contains(result.stdout, "## "+filepath.Join(folder, "app.go")) {
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

// TestCLIProcess_QueryShapeDecidesSemanticCache checks the user-visible rule:
// Seek builds meaning-based data for every search that can read it. One word,
// a symbol filter and a description reach it; a query of negations only gives
// the model no text, so it builds none.
//
// The test runs the real process, as the --lexical-only test does. Both check
// whether a command leaves semantic data in the cache. The in-process tests
// check why; this test checks what a user sees. The symbol lookup expects the
// model to add no file. So the folder has a fixed model path
// (stableModelRoot), and the score is the same in each run.
func TestCLIProcess_QueryShapeDecidesSemanticCache(t *testing.T) {
	requireTools(t)
	folder := stableModelRoot(t)
	writeFileAt(t, folder, "app.go", "package sample\n// alpha beta gamma describes the sample\n")

	// wantCode keeps a case from a false pass. A case with no results and no
	// vectors would also pass if the command crashed, so each case sets the
	// exit code. 0 is a match, 1 is no match, and 2 is an error that must not
	// occur here.
	for _, testCase := range []struct {
		name        string
		query       string
		wantCode    int
		wantResults bool
		wantVectors bool
	}{
		{name: "exact word", query: "alpha", wantCode: 0, wantResults: true, wantVectors: true},
		{name: "symbol lookup", query: "sym:Missing", wantCode: 1, wantResults: false, wantVectors: true},
		{name: "description", query: "describes the sample", wantCode: 0, wantResults: true, wantVectors: true},
		{name: "negations only", query: "-file:missing", wantCode: 0, wantResults: true, wantVectors: false},
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
			if testCase.wantResults != strings.Contains(result.stdout, "## "+filepath.Join(folder, "app.go")) {
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

// runCLIProcess runs args in dir as a CLI process with a new empty cache.
func runCLIProcess(t *testing.T, dir string, args, extraEnv []string) cliProcessResult {
	t.Helper()
	return runCLIProcessWithCache(t, t.TempDir(), dir, args, extraEnv)
}

// runCLIProcessWithCache runs args in dir as a CLI process with cacheDir as
// its cache and extraEnv last in its environment.
func runCLIProcessWithCache(t *testing.T, cacheDir, dir string, args, extraEnv []string) cliProcessResult {
	t.Helper()
	cmd := cliProcessCommand(t.Context(), cacheDir, dir, args, extraEnv)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	return cliProcessResult{stdout: stdout.String(), stderr: stderr.String(), code: cliProcessExitCode(t, runErr)}
}

// runCLIProcessOnLine runs the CLI process as runCLIProcessWithCache does and
// reads its stderr line by line. At the first line that match accepts, it
// calls act with the process, once. The second result reports whether that
// line came. The process writes the debug line of a lock wait (pollLock) only
// with --verbose; lockWaitLine matches it.
func runCLIProcessOnLine(t *testing.T, cacheDir, dir string, args []string, match func(line string) bool, act func(*os.Process)) (cliProcessResult, bool) {
	t.Helper()
	cmd := cliProcessCommand(t.Context(), cacheDir, dir, args, nil)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	seen := false
	scanner := bufio.NewScanner(stderrPipe)
	for scanner.Scan() {
		stderr.WriteString(scanner.Text() + "\n")
		if !seen && match(scanner.Text()) {
			seen = true
			act(cmd.Process)
		}
	}
	waitErr := cmd.Wait()
	return cliProcessResult{stdout: stdout.String(), stderr: stderr.String(), code: cliProcessExitCode(t, waitErr)}, seen
}

// lockWaitLine matches the stderr line of a lock wait of a CLI process. The
// line holds reason (lockWaitBuild, lockWaitReaders, or lockWaitPublish) and
// the path of the lock, so a wait on another lock does not match.
func lockWaitLine(reason, lock string) func(line string) bool {
	return func(line string) bool {
		return strings.Contains(line, reason) && strings.Contains(line, lock)
	}
}

// cliProcessCommand returns the command of the CLI helper process: it runs
// args in dir, with cacheDir as its cache and extraEnv last in its
// environment.
func cliProcessCommand(ctx context.Context, cacheDir, dir string, args, extraEnv []string) *exec.Cmd {
	commandArgs := append([]string{"-test.run=^TestCLIProcessHelper$", "--"}, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], commandArgs...)
	cmd.Dir = dir
	overrides := []string{
		cliProcessHelperEnv + "=" + cliProcessTestMarker,
		"SEEK_CACHE_DIR=" + cacheDir,
		"CTAGS_COMMAND=",
	}
	overrides = append(overrides, extraEnv...)
	cmd.Env = append(cmd.Environ(), overrides...)
	return cmd
}

// cliProcessExitCode returns the exit code of a CLI process from the error of
// its Run or Wait. Any error other than an exit status fails the test.
func cliProcessExitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("run CLI helper: %v", err)
	}
	return exitErr.ExitCode()
}

// TestCLIProcess_ContentRulesOnStdoutAndStderr checks the output rules on the
// real process. Content past Zoekt's default trigram cap matches, and stdout
// ends with one newline. A file whose content is not in the index gets one
// warning on stderr with Zoekt's reason, and the exit status is 1. The
// in-process tests check which operands get a warning.
func TestCLIProcess_ContentRulesOnStdoutAndStderr(t *testing.T) {
	requireTools(t)
	const warning = "File content is not searchable"
	folder := t.TempDir()
	writeFileAt(t, folder, "nul.bin", "hello\x00tail\n")
	high := string(highTrigramContent(t, "cli_high_trigram_marker"))
	writeFileAt(t, folder, "high.txt", high)
	nul := filepath.Join(folder, "nul.bin")

	t.Run("high-trigram file matches", func(t *testing.T) {
		result := runCLIProcess(t, t.TempDir(), []string{"--lexical-only", "cli_high_trigram_marker", filepath.Join(folder, "high.txt")}, nil)
		if result.code != 0 || !strings.Contains(result.stdout, "cli_high_trigram_marker") {
			t.Fatalf("code=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
		}
		if !strings.HasSuffix(result.stdout, "\n") || strings.HasSuffix(result.stdout, "\n\n") {
			t.Fatalf("stdout must end with exactly one newline: %q", result.stdout)
		}
	})
	t.Run("no match warns with the reason", func(t *testing.T) {
		result := runCLIProcess(t, t.TempDir(), []string{"--lexical-only", "tail", nul}, nil)
		if result.code != 1 || result.stdout != "" ||
			strings.Count(result.stderr, warning) != 1 ||
			!strings.Contains(result.stderr, "NOT-INDEXED: contains binary content") {
			t.Fatalf("code=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
		}
	})
}
