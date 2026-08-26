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
				!strings.Contains(result.stdout, "Evict per-corpus caches older than the TTL") ||
				!strings.Contains(result.stdout, "--dry-run") {
				t.Fatalf("stdout=%q stderr=%q code=%d, want gc help on stdout", result.stdout, result.stderr, result.code)
			}
		})
	}
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
