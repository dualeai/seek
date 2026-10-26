package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"syscall"
	"testing"
)

func TestFormatCLIError(t *testing.T) {
	plan := node{
		root:     "/work/src",
		cacheDir: "/cache/corpus",
		indexDir: "/cache/corpus/index",
	}
	parseCause := errors.New("parser detail")
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "query syntax",
			err:  &querySyntaxError{query: "foo or", cause: parseCause},
			want: "seek: invalid query \"foo or\": parser detail",
		},
		{
			name: "missing path",
			err:  newPathOperandError(pathOperandRead, "missing", fs.ErrNotExist),
			want: "seek: path does not exist: \"missing\"",
		},
		{
			name: "folder root removed during the search",
			err:  folderCorpusError(plan, fmt.Errorf("read folder root: %w", fs.ErrNotExist)),
			want: "seek: path does not exist: \"/work/src\"",
		},
		{
			name: "Git directory",
			err:  newPathOperandError(pathOperandResolve, "repo/.git", errGitDirectoryOperand),
			want: "seek: cannot search a Git directory: \"repo/.git\"",
		},
		{
			name: "unreadable path",
			err:  newPathOperandError(pathOperandRead, "private", fs.ErrPermission),
			want: "seek: cannot read path \"private\": permission denied",
		},
		{
			name: "git not found",
			err:  gitCorpusError("/work/repo", "/cache/index", &gitUnavailableError{cause: errors.New("not found")}),
			want: "seek: Git is required but was not found\n" +
				"hint: install Git and ensure git is on PATH",
		},
		{
			name: "ctags not found",
			err:  &ctagsUnavailableError{},
			want: "seek: Universal Ctags is not available\n" +
				"hint: install Universal Ctags or set CTAGS_COMMAND=/path/to/ctags",
		},
		{
			name: "configured ctags not found",
			err:  &ctagsUnavailableError{command: "/missing/ctags", cause: fs.ErrNotExist},
			want: "seek: cannot use Universal Ctags command \"/missing/ctags\"\n" +
				"hint: set CTAGS_COMMAND to an executable Universal Ctags path",
		},
		{
			name: "generic fallback",
			err:  errors.New("boom"),
			want: "seek: boom",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatCLIError(tc.err)
			if got != tc.want {
				t.Fatalf("formatCLIError()=\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}

func TestReportCLIError(t *testing.T) {
	var out bytes.Buffer
	reportCLIError(&out, errNoMatch, false)
	if out.Len() != 0 {
		t.Fatalf("no-match output=%q, want empty", out.String())
	}

	reportCLIError(&out, errors.New("boom"), false)
	if got, want := out.String(), "seek: boom\n"; got != want {
		t.Fatalf("normal output=%q, want %q", got, want)
	}
}

func TestReportCLIError_VerboseKeepsUserMessageAndAddsDetail(t *testing.T) {
	logs := captureTestLogs(t, slog.LevelDebug)

	var out bytes.Buffer
	err := gitCorpusError(
		"/work/repo",
		"/cache/index",
		&gitUnavailableError{cause: errors.New("technical detail")},
	)
	reportCLIError(&out, err, true)
	wantMessage := "seek: Git is required but was not found\n" +
		"hint: install Git and ensure git is on PATH\n"
	if !strings.HasPrefix(out.String(), wantMessage) {
		t.Fatalf("verbose output=%q, want user message prefix %q", out.String(), wantMessage)
	}
	if !bytes.Contains(out.Bytes(), []byte("level=ERROR")) ||
		!bytes.Contains(out.Bytes(), []byte(`msg="Command failed"`)) ||
		!bytes.Contains(out.Bytes(), []byte(`error="git corpus root=`)) {
		t.Fatalf("verbose output=%q, want structured original error", out.String())
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("default logger records=%+v, want reporter bound to its writer", records)
	}
}

func TestReportCLIError_VerboseNoMatchIsSilent(t *testing.T) {
	logs := captureTestLogs(t, slog.LevelDebug)

	var out bytes.Buffer
	reportCLIError(&out, errNoMatch, true)
	if out.Len() != 0 {
		t.Fatalf("no-match output=%q, want empty", out.String())
	}
	if records := logs.Records(); len(records) != 0 {
		t.Fatalf("no-match records=%+v, want none", records)
	}
}

func TestReportStaleIndexWarning_CtagsHidesTechnicalContextAtWarn(t *testing.T) {
	logs := captureTestLogs(t, slog.LevelDebug)

	err := gitCorpusError(
		"/secret/repo",
		"/secret/cache/index",
		&ctagsUnavailableError{command: "/configured/ctags", cause: fs.ErrPermission},
	)
	reportStaleIndexWarning(err)

	records := logs.Records()
	if len(records) != 2 {
		t.Fatalf("records=%+v, want one warning and one debug record", records)
	}
	warnRecord, debugRecord := records[0], records[1]
	if warnRecord.Level != slog.LevelWarn || warnRecord.Message != "Index update failed; using the existing index" {
		t.Fatalf("warning=%+v, want concise stale-index warning", warnRecord)
	}
	warnAttrs := testLogAttrs(warnRecord)
	if got := warnAttrs["cause"]; got != `cannot use Universal Ctags command "/configured/ctags"` {
		t.Errorf("warning cause=%v", got)
	}
	if got := warnAttrs["hint"]; got != "set CTAGS_COMMAND to an executable Universal Ctags path" {
		t.Errorf("warning hint=%v", got)
	}
	if _, found := warnAttrs["error"]; found {
		t.Errorf("warning exposes technical error: %+v", warnAttrs)
	}
	if debugRecord.Level != slog.LevelDebug || debugRecord.Message != "Index update failure detail" {
		t.Fatalf("debug=%+v, want update failure detail", debugRecord)
	}
	debugErr, ok := testLogAttrs(debugRecord)["error"].(error)
	if !ok {
		t.Fatalf("debug error=%T, want error", testLogAttrs(debugRecord)["error"])
	}
	if !strings.Contains(debugErr.Error(), `git corpus root="/secret/repo" index="/secret/cache/index": `) {
		t.Errorf("debug error=%v, want Git corpus context", debugErr)
	}
	ctagsErr, ok := errors.AsType[*ctagsUnavailableError](debugErr)
	if !ok || ctagsErr.command != "/configured/ctags" || !errors.Is(debugErr, fs.ErrPermission) {
		t.Errorf("debug error=%v, want configured Ctags permission error", debugErr)
	}
}

func TestReportStaleIndexWarning_GenericHidesTechnicalContextAtWarn(t *testing.T) {
	logs := captureTestLogs(t, slog.LevelDebug)

	err := gitCorpusError(
		"/secret/repo",
		"/secret/cache/index",
		errors.New("private backend detail"),
	)
	reportStaleIndexWarning(err)

	records := logs.Records()
	if len(records) != 2 {
		t.Fatalf("records=%+v, want one warning and one debug record", records)
	}
	warnRecord, debugRecord := records[0], records[1]
	if warnRecord.Level != slog.LevelWarn || warnRecord.Message != "Index update failed; using the existing index" {
		t.Fatalf("warning=%+v, want concise stale-index warning", warnRecord)
	}
	if got := testLogAttrs(warnRecord)["hint"]; got != "run with --verbose for details" {
		t.Fatalf("warning hint=%v, want verbose guidance", got)
	}
	if debugRecord.Level != slog.LevelDebug || debugRecord.Message != "Index update failure detail" {
		t.Fatalf("debug=%+v, want update failure detail", debugRecord)
	}
	if got := testLogAttrs(debugRecord)["error"]; got != err {
		t.Fatalf("debug error=%v, want original error", got)
	}
}

func TestReportStaleIndexWarning_CancellationDoesNotWarn(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			logs := captureTestLogs(t, slog.LevelWarn)

			reportStaleIndexWarning(gitCorpusError("/repo", "/cache/index", cause))
			if records := logs.Records(); len(records) != 0 {
				t.Fatalf("cancellation records=%+v, want none", records)
			}
		})
	}
}

// TestKeptFilesWarningNamesTheFile checks the warning for dirty files kept by
// name. It is one warning with their count, the first path, and the cause
// without the path. It does not say "using the existing index".
func TestKeptFilesWarningNamesTheFile(t *testing.T) {
	recorder := captureTestLogs(t, slog.LevelWarn)
	reportStaleIndexWarning(gitCorpusError("/repo", "/index", &dirtyFilesKept{files: []string{"b.txt", "c.txt"}, cause: fmt.Errorf("read /repo/b.txt: %w", &fs.PathError{Op: "open", Path: "/repo/b.txt", Err: syscall.EIO})}))
	records := recorder.Records()
	if len(records) != 1 || !strings.Contains(records[0].Message, "by name only") {
		t.Fatalf("records=%v", records)
	}
	attrs := map[string]string{}
	records[0].Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()
		return true
	})
	if attrs["files"] != "2" || attrs["first"] != "b.txt" || attrs["cause"] != syscall.EIO.Error() {
		t.Fatalf("attrs=%v", attrs)
	}
}
