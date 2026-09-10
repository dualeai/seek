package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt/ignore"
	"github.com/sourcegraph/zoekt/index"
)

const nativeTestOID = "0123456789abcdef0123456789abcdef01234567"

var (
	parsedTreeEntrySink gitTreeEntry
	parsedBatchSizeSink int64
)

func TestNativeGitRecordParserAllocations(t *testing.T) {
	treeRecord := []byte("100644 blob " + nativeTestOID + "\tpath/to/file.go\x00")
	if allocs := testing.AllocsPerRun(1000, func() {
		entry, err := parseNativeGitTreeRecord(treeRecord)
		if err != nil {
			t.Fatal(err)
		}
		parsedTreeEntrySink = entry
	}); allocs > 2 {
		t.Fatalf("tree parser allocations=%g, want at most 2", allocs)
	}

	batchHeader := []byte(nativeTestOID + " blob 12345")
	if allocs := testing.AllocsPerRun(1000, func() {
		size, err := parseNativeGitBatchHeader(batchHeader, gitObjectID(nativeTestOID), nil)
		if err != nil {
			t.Fatal(err)
		}
		parsedBatchSizeSink = size
	}); allocs != 0 {
		t.Fatalf("batch parser allocations=%g, want 0", allocs)
	}
}

func installFakeGit(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Git fixture requires a POSIX shell")
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
read_object_command() {
  IFS=' ' read -r verb object
}
case "$FAKE_GIT_MODE" in
	version_old)
		printf 'git version 2.35.9\n'
		exit 0
		;;
	version_minimum)
		printf 'git version 2.36.0\n'
		exit 0
		;;
	version_newer)
		printf 'git version 3.0.1\n'
		exit 0
		;;
	batch_session_valid)
		read_object_command
		test "$verb" = info || exit 65
		id=$object
		IFS= read -r flush
		test "$flush" = flush || exit 66
		printf '%s blob 3\n' "$id"
		read_object_command
		test "$verb" = contents || exit 67
		test "$object" = "$id" || exit 68
		IFS= read -r flush
		test "$flush" = flush || exit 69
		printf '%s blob 3\nabc\n' "$id"
		exit 0
		;;
  valid_then_fail)
    printf '100644 blob 0123456789abcdef0123456789abcdef01234567\tfile.go\0'
    printf 'FIRST_GIT_ERROR\n' >&2
    i=0
    while [ "$i" -lt 40000 ]; do printf x >&2; i=$((i + 1)); done
    printf '\nLAST_GIT_ERROR\n' >&2
    exit 7
    ;;
  truncated)
    printf '100644 blob 0123456789abcdef0123456789abcdef01234567\tfile.go'
    exit 0
    ;;
  malformed_then_hang)
    printf '%s\n' "$$" > "$FAKE_GIT_PID_FILE"
    printf 'bad\0'
    trap 'exit 130' INT TERM
    while :; do :; done
    ;;
  hang)
    printf '%s\n' "$$" > "$FAKE_GIT_PID_FILE"
    trap 'exit 130' INT TERM
    while :; do :; done
    ;;
  batch_check_wrong_id)
    read_object_command
    printf '1123456789abcdef0123456789abcdef01234567 blob 3\n'
    ;;
  batch_check_wrong_type)
    read_object_command
    printf '%s tree 3\n' "$object"
    ;;
  batch_check_invalid_size)
    read_object_command
    printf '%s blob -1\n' "$object"
    ;;
  batch_check_overflow)
    read_object_command
    printf '%s blob 999999999999999999999999999999999999\n' "$object"
    ;;
  batch_check_limits)
    read_object_command
    first=$object
    read_object_command
    second=$object
    printf '%s blob 104857600\n' "$first"
    printf '%s blob 104857601\n' "$second"
    exit 0
    ;;
	batch_check_oversize)
		read_object_command
		IFS= read -r flush
		test "$flush" = flush || exit 70
		printf '%s blob 104857601\n' "$object"
		exit 0
		;;
  batch_check_missing)
    read_object_command
    printf '%s missing\n' "$object"
    ;;
  batch_check_extra)
    read_object_command
    printf '%s blob 3\nEXTRA\n' "$object"
    ;;
  batch_check_nonzero)
    read_object_command
    printf '%s blob 3\n' "$object"
    printf 'late batch failure\n' >&2
    exit 9
    ;;
  batch_check_long_header)
    read_object_command
    i=0
    while [ "$i" -lt 600 ]; do printf x; i=$((i + 1)); done
    printf '\n'
    ;;
  batch_check_blocked_pipes)
    i=0
    while [ "$i" -lt 8192 ]; do
      printf '0123456789abcdef0123456789abcdef01234567 blob 3\n'
      i=$((i + 1))
    done
    while IFS= read -r line; do :; done
    exit 0
    ;;
  batch_content_truncated)
    read_object_command
    printf '%s blob 3\nab' "$object"
    ;;
  batch_content_bad_delimiter)
    read_object_command
    printf '%s blob 3\nabcX' "$object"
    ;;
  batch_content_extra)
    read_object_command
    printf '%s blob 3\nabc\nEXTRA' "$object"
    ;;
  batch_content_nonzero)
    read_object_command
    printf '%s blob 3\nabc\n' "$object"
    printf 'late content failure\n' >&2
    exit 11
    ;;
  batch_content_wrong_size)
    read_object_command
    printf '%s blob 4\nabcd\n' "$object"
    ;;
  batch_content_blocked_pipes)
    i=0
    while [ "$i" -lt 8192 ]; do
      printf '0123456789abcdef0123456789abcdef01234567 blob 3\nabc\n'
      i=$((i + 1))
    done
    while IFS= read -r line; do :; done
    exit 0
    ;;
  diff_valid_then_fail)
    printf ':000000 100644 0000000000000000000000000000000000000000 0123456789abcdef0123456789abcdef01234567 A\0added.go\0'
    printf 'late diff failure\n' >&2
    exit 12
    ;;
  diff_valid_then_hang)
    printf '%s\n' "$$" > "$FAKE_GIT_PID_FILE"
    printf ':000000 100644 0000000000000000000000000000000000000000 0123456789abcdef0123456789abcdef01234567 A\0added.go\0'
    trap 'exit 130' INT TERM
    while :; do :; done
    ;;
  diff_malformed)
    printf ':bad\0path\0'
    ;;
esac
exit 64
`
	path := filepath.Join(binDir, "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
}

func TestNativeGitBatchSessionFramesInfoContentsAndFlush(t *testing.T) {
	installFakeGit(t)
	t.Setenv("FAKE_GIT_MODE", "batch_session_valid")
	batch, err := startNativeGitBatch(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry := fakeTreeEntry(nativeTestOID)
	infos, err := batch.check([]gitTreeEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	var content []byte
	if err := batch.read(infos, func(document fileContent) error {
		content = append([]byte(nil), document.content...)
		readSemaphore.Release(document.weight)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := batch.close(); err != nil {
		t.Fatal(err)
	}
	if string(content) != "abc" {
		t.Fatalf("content=%q, want abc", content)
	}
}

func requireProcessGone(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read fake Git PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse fake Git PID: %v", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	deadline := time.Now().Add(time.Second)
	for {
		err = process.Signal(syscall.Signal(0))
		if err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake Git process %d is still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseGitObjectID(t *testing.T) {
	for _, valid := range []string{
		nativeTestOID,
		strings.Repeat("a", 64),
	} {
		if got, err := parseGitObjectID([]byte(valid)); err != nil || got.String() != valid {
			t.Fatalf("parse %q: got=%q err=%v", valid, got, err)
		}
	}
	for _, invalid := range []string{
		strings.Repeat("a", 39),
		strings.Repeat("a", 41),
		strings.Repeat("A", 40),
		strings.Repeat("g", 40),
	} {
		if _, err := parseGitObjectID([]byte(invalid)); err == nil {
			t.Fatalf("parse %q succeeded", invalid)
		}
	}
}

func TestRequireNativeGitVersion(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		wantErr bool
	}{
		{mode: "version_old", wantErr: true},
		{mode: "version_minimum"},
		{mode: "version_newer"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			installFakeGit(t)
			t.Setenv("FAKE_GIT_MODE", tc.mode)
			err := requireNativeGitVersion(t.Context(), t.TempDir())
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, want error=%t", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "Git 2.36 or later") {
				t.Fatalf("error=%q does not state the Git floor", err)
			}
		})
	}
	t.Run("unavailable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		err := requireNativeGitVersion(t.Context(), t.TempDir())
		if !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("error=%v, want Git executable failure", err)
		}
	})
}

func TestParseNativeGitTreeRecordModesAndBytePaths(t *testing.T) {
	for _, test := range []struct {
		mode   string
		object string
		path   string
	}{
		{"100644", "blob", "regular.go"},
		{"100755", "blob", "-executable\tname\n.sh"},
		{"120000", "blob", "link"},
		{"160000", "commit", "submodule"},
	} {
		record := []byte(test.mode + " " + test.object + " " + nativeTestOID + "\t" + test.path + "\x00")
		entry, err := parseNativeGitTreeRecord(record)
		if err != nil {
			t.Fatalf("parse %q: %v", record, err)
		}
		if entry.mode != test.mode || entry.oid.String() != nativeTestOID || entry.path != test.path {
			t.Fatalf("entry=%+v", entry)
		}
	}
}

func TestParseNativeGitTreeRecordRejectsMalformedInput(t *testing.T) {
	for _, record := range [][]byte{
		[]byte("100644 blob " + nativeTestOID + "\tfile.go"),
		[]byte("100644 blob " + nativeTestOID + "\x00"),
		[]byte("100644  blob " + nativeTestOID + "\tfile.go\x00"),
		[]byte("100644 tree " + nativeTestOID + "\tfile.go\x00"),
		[]byte("040000 tree " + nativeTestOID + "\tdir\x00"),
		[]byte("100644 blob " + strings.ToUpper(nativeTestOID) + "\tfile.go\x00"),
		[]byte("100644 blob " + nativeTestOID + "\t\x00"),
	} {
		if _, err := parseNativeGitTreeRecord(record); err == nil {
			t.Fatalf("parse %q succeeded", record)
		}
	}
}

func TestSanitizedGitEnvSetsStableNonInteractiveValues(t *testing.T) {
	env := sanitizedGitEnv([]string{
		"PATH=/bin",
		"LC_ALL=fr_FR.UTF-8",
		"LANG=fr_FR.UTF-8",
		"GIT_TERMINAL_PROMPT=1",
		"GIT_DIR=/wrong",
		"GIT_LITERAL_PATHSPECS=0",
	})
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/bin", "LC_ALL=C", "LANG=C", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("environment %q lacks %q", joined, want)
		}
	}
	for _, unwanted := range []string{"fr_FR", "GIT_DIR=", "GIT_LITERAL_PATHSPECS="} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("environment %q contains %q", joined, unwanted)
		}
	}
}

func TestBoundedGitStderrKeepsStartAndEnd(t *testing.T) {
	var stderr boundedGitStderr
	input := "FIRST\n" + strings.Repeat("x", gitStderrMax*2) + "\nLAST"
	if _, err := stderr.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	got := stderr.String()
	if !strings.Contains(got, "FIRST") || !strings.Contains(got, "LAST") || !strings.Contains(got, "truncated") {
		t.Fatalf("bounded stderr lost its ends: %q", got)
	}
	if len(got) > gitStderrMax+128 {
		t.Fatalf("bounded stderr length=%d", len(got))
	}
}

func TestNativeGitTreeStartFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := readNativeGitTreeRecords(t.Context(), t.TempDir(), []string{"ls-tree"}, func([]gitTreeEntry) error { return nil })
	if err == nil || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("error=%v, want Git executable failure", err)
	}
}

func TestNativeGitTreeChecksLateExitAndBoundedStderr(t *testing.T) {
	installFakeGit(t)
	t.Setenv("FAKE_GIT_MODE", "valid_then_fail")
	seen := 0
	err := readNativeGitTreeRecords(t.Context(), t.TempDir(), []string{"ls-tree"}, func(entries []gitTreeEntry) error {
		seen += len(entries)
		return nil
	})
	if seen != 1 || err == nil {
		t.Fatalf("seen=%d error=%v", seen, err)
	}
	for _, text := range []string{"FIRST_GIT_ERROR", "LAST_GIT_ERROR", "truncated", "exit status 7"} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("error %q lacks %q", err, text)
		}
	}
}

func TestNativeGitTreeRejectsEarlyEOF(t *testing.T) {
	installFakeGit(t)
	t.Setenv("FAKE_GIT_MODE", "truncated")
	err := readNativeGitTreeRecords(t.Context(), t.TempDir(), []string{"ls-tree"}, func([]gitTreeEntry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "truncated Git tree record") {
		t.Fatalf("error=%v", err)
	}
}

func TestNativeGitTreeParseFailureKillsAndJoinsChild(t *testing.T) {
	installFakeGit(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("FAKE_GIT_PID_FILE", pidFile)
	t.Setenv("FAKE_GIT_MODE", "malformed_then_hang")
	err := readNativeGitTreeRecords(t.Context(), t.TempDir(), []string{"ls-tree"}, func([]gitTreeEntry) error { return nil })
	if err == nil {
		t.Fatal("malformed tree succeeded")
	}
	requireProcessGone(t, pidFile)
}

func TestNativeGitTreeCancellationAndDeadline(t *testing.T) {
	for _, test := range []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		want    error
	}{
		{
			name: "cancellation",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(500*time.Millisecond, cancel)
				return ctx, cancel
			},
			want: context.Canceled,
		},
		{
			name: "deadline",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 500*time.Millisecond)
			},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			installFakeGit(t)
			pidFile := filepath.Join(t.TempDir(), fmt.Sprintf("%s.pid", test.name))
			t.Setenv("FAKE_GIT_PID_FILE", pidFile)
			t.Setenv("FAKE_GIT_MODE", "hang")
			ctx, cancel := test.context()
			defer cancel()
			err := readNativeGitTreeRecords(ctx, t.TempDir(), []string{"ls-tree"}, func([]gitTreeEntry) error { return nil })
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
			requireProcessGone(t, pidFile)
		})
	}
}

func fakeTreeEntry(oid string) gitTreeEntry {
	return gitTreeEntry{mode: "100644", oid: gitObjectID(oid), path: "file.go"}
}

func TestCheckNativeGitBlobsUsesBoundedChunksAndAcceptsDuplicates(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	path := filepath.Join(repoDir, "blob")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	oid := gitOutputIn(t, repoDir, "hash-object", "-w", "blob")
	entries := make([]gitTreeEntry, gitObjectBatchSize+1)
	for i := range entries {
		entries[i] = fakeTreeEntry(oid)
	}
	var chunks []int
	err := checkNativeGitBlobs(t.Context(), repoDir, entries, func(infos []gitBlobInfo) error {
		chunks = append(chunks, len(infos))
		for _, info := range infos {
			if info.size != 3 || info.entry.oid.String() != oid {
				t.Fatalf("blob info=%+v", info)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(chunks) != "[8192 1]" {
		t.Fatalf("batch chunks=%v", chunks)
	}
}

func TestCheckNativeGitBlobReplyFailures(t *testing.T) {
	for _, mode := range []string{
		"batch_check_wrong_id",
		"batch_check_wrong_type",
		"batch_check_invalid_size",
		"batch_check_overflow",
		"batch_check_missing",
		"batch_check_extra",
		"batch_check_nonzero",
		"batch_check_long_header",
	} {
		t.Run(mode, func(t *testing.T) {
			installFakeGit(t)
			t.Setenv("FAKE_GIT_MODE", mode)
			err := checkNativeGitBlobs(
				t.Context(),
				t.TempDir(),
				[]gitTreeEntry{fakeTreeEntry(nativeTestOID)},
				func([]gitBlobInfo) error { return nil },
			)
			if err == nil {
				t.Fatal("malformed cat-file info reply succeeded")
			}
		})
	}
}

func TestCheckNativeGitBlobExactLimitAndOversize(t *testing.T) {
	installFakeGit(t)
	t.Setenv("FAKE_GIT_MODE", "batch_check_limits")
	secondOID := strings.Repeat("a", 40)
	var infos []gitBlobInfo
	err := checkNativeGitBlobs(
		t.Context(),
		t.TempDir(),
		[]gitTreeEntry{
			fakeTreeEntry(nativeTestOID),
			fakeTreeEntry(secondOID),
		},
		func(chunk []gitBlobInfo) error {
			infos = append(infos, chunk...)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].size != maxIndexedDocumentBytes || infos[0].oversize ||
		infos[1].size != maxIndexedDocumentBytes+1 || !infos[1].oversize {
		t.Fatalf("blob limits=%+v", infos)
	}
}

func TestScanNativeGitDeltaChangesCountsOversizeWithoutBytes(t *testing.T) {
	installFakeGit(t)
	t.Setenv("FAKE_GIT_MODE", "batch_check_oversize")
	base := gitIndexBudget{candidates: 7, indexedBytes: 99}
	diff := []gitDiffEntry{{
		newMode: "100644",
		newOID:  gitObjectID(nativeTestOID),
		newPath: "oversize.bin",
	}}
	infos, got, consistent, err := scanNativeGitDeltaChanges(t.Context(), t.TempDir(), &ignore.Matcher{}, diff, base)
	if err != nil || !consistent {
		t.Fatalf("scan oversize delta: consistent=%t error=%v", consistent, err)
	}
	want := gitIndexBudget{candidates: 8, indexedBytes: 99}
	if got != want || len(infos) != 1 || !infos[0].oversize || infos[0].entry.path != "oversize.bin" {
		t.Fatalf("budget=%+v infos=%+v, want budget=%+v and one oversize file", got, infos, want)
	}
}

func TestCheckNativeGitBlobsReadsAndWritesConcurrently(t *testing.T) {
	installFakeGit(t)
	t.Setenv("FAKE_GIT_MODE", "batch_check_blocked_pipes")
	entries := make([]gitTreeEntry, gitObjectBatchSize)
	for i := range entries {
		entries[i] = fakeTreeEntry(nativeTestOID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count := 0
	err := checkNativeGitBlobs(ctx, t.TempDir(), entries, func(infos []gitBlobInfo) error {
		count += len(infos)
		return nil
	})
	if err != nil || count != gitObjectBatchSize {
		t.Fatalf("infos=%d error=%v", count, err)
	}
}

func TestCheckNativeGitBlobCancellationKillsChild(t *testing.T) {
	installFakeGit(t)
	pidFile := filepath.Join(t.TempDir(), "batch.pid")
	t.Setenv("FAKE_GIT_PID_FILE", pidFile)
	t.Setenv("FAKE_GIT_MODE", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := checkNativeGitBlobs(
		ctx,
		t.TempDir(),
		[]gitTreeEntry{fakeTreeEntry(nativeTestOID)},
		func([]gitBlobInfo) error { return nil },
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline", err)
	}
	requireProcessGone(t, pidFile)
}

func TestReadNativeGitBlobsReadsExactContent(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	content := []byte("abc\nwith content\x00")
	path := filepath.Join(repoDir, "blob")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	oid := gitOutputIn(t, repoDir, "hash-object", "-w", "blob")
	info := gitBlobInfo{entry: fakeTreeEntry(oid), size: int64(len(content))}
	var got []byte
	err := readNativeGitBlobs(t.Context(), repoDir, []gitBlobInfo{info}, func(document fileContent) error {
		got = append([]byte(nil), document.content...)
		if document.weight > 0 {
			readSemaphore.Release(document.weight)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("content=%q, want %q", got, content)
	}
}

func TestReadNativeGitBlobsDoesNotStartChildForOversize(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	info := gitBlobInfo{entry: fakeTreeEntry(nativeTestOID), size: maxIndexedDocumentBytes + 1, oversize: true}
	called := false
	err := readNativeGitBlobs(t.Context(), t.TempDir(), []gitBlobInfo{info}, func(document fileContent) error {
		called = true
		if document.name != "file.go" || document.content != nil || document.skipReason != index.SkipReasonTooLarge {
			t.Fatalf("oversize document=%+v", document)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("called=%t error=%v", called, err)
	}
}

func TestReadNativeGitBlobReplyFailures(t *testing.T) {
	for _, mode := range []string{
		"batch_content_truncated",
		"batch_content_bad_delimiter",
		"batch_content_extra",
		"batch_content_nonzero",
		"batch_content_wrong_size",
	} {
		t.Run(mode, func(t *testing.T) {
			installFakeGit(t)
			t.Setenv("FAKE_GIT_MODE", mode)
			info := gitBlobInfo{entry: fakeTreeEntry(nativeTestOID), size: 3}
			err := readNativeGitBlobs(t.Context(), t.TempDir(), []gitBlobInfo{info}, func(document fileContent) error {
				if document.weight > 0 {
					readSemaphore.Release(document.weight)
				}
				return nil
			})
			if err == nil {
				t.Fatal("malformed batch reply succeeded")
			}
		})
	}
}

func TestReadNativeGitBlobsReadsAndWritesConcurrently(t *testing.T) {
	installFakeGit(t)
	t.Setenv("FAKE_GIT_MODE", "batch_content_blocked_pipes")
	infos := make([]gitBlobInfo, gitObjectBatchSize)
	for i := range infos {
		infos[i] = gitBlobInfo{entry: fakeTreeEntry(nativeTestOID), size: 3}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count := 0
	err := readNativeGitBlobs(ctx, t.TempDir(), infos, func(document fileContent) error {
		count++
		readSemaphore.Release(document.weight)
		return nil
	})
	if err != nil || count != gitObjectBatchSize {
		t.Fatalf("documents=%d error=%v", count, err)
	}
}

func TestReadNativeGitBlobCancellationKillsChild(t *testing.T) {
	installFakeGit(t)
	pidFile := filepath.Join(t.TempDir(), "content.pid")
	t.Setenv("FAKE_GIT_PID_FILE", pidFile)
	t.Setenv("FAKE_GIT_MODE", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	info := gitBlobInfo{entry: fakeTreeEntry(nativeTestOID), size: 3}
	err := readNativeGitBlobs(ctx, t.TempDir(), []gitBlobInfo{info}, func(fileContent) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline", err)
	}
	requireProcessGone(t, pidFile)
}

func TestParseNativeGitDiffHeader(t *testing.T) {
	zero := strings.Repeat("0", 40)
	for _, test := range []struct {
		status  byte
		oldMode string
		newMode string
		oldOID  string
		newOID  string
	}{
		{'A', "000000", "100644", zero, nativeTestOID},
		{'D', "100755", "000000", nativeTestOID, zero},
		{'M', "100644", "100755", nativeTestOID, nativeTestOID},
		{'T', "100644", "120000", nativeTestOID, strings.Repeat("a", 40)},
	} {
		header := []byte(fmt.Sprintf(":%s %s %s %s %c", test.oldMode, test.newMode, test.oldOID, test.newOID, test.status))
		entry, err := parseNativeGitDiffHeader(header, []byte("a\tpath\n.go"), 40)
		if err != nil {
			t.Fatal(err)
		}
		if entry.oldMode != test.oldMode || entry.newMode != test.newMode || entry.oldOID.String() != test.oldOID || entry.newOID.String() != test.newOID || entry.oldPath != "a\tpath\n.go" {
			t.Fatalf("entry=%+v", entry)
		}
	}
	for _, header := range []string{
		":000000 100644 " + zero + " " + nativeTestOID + " R100",
		":000000 100644 " + zero + " " + nativeTestOID + " D",
		":100644 000000 " + nativeTestOID + " " + zero + " A",
		":040000 100644 " + nativeTestOID + " " + nativeTestOID + " M",
		"100644 100644 " + nativeTestOID + " " + nativeTestOID + " M",
	} {
		if _, err := parseNativeGitDiffHeader([]byte(header), []byte("path"), 40); err == nil {
			t.Fatalf("parse diff header %q succeeded", header)
		}
	}
}

func TestReadNativeGitDiffChecksLateExitAndMalformedOutput(t *testing.T) {
	for _, mode := range []string{"diff_valid_then_fail", "diff_malformed"} {
		t.Run(mode, func(t *testing.T) {
			installFakeGit(t)
			t.Setenv("FAKE_GIT_MODE", mode)
			_, err := readNativeGitDiff(t.Context(), t.TempDir(), gitObjectID(nativeTestOID), gitObjectID(strings.Repeat("a", 40)), 10)
			if err == nil {
				t.Fatal("invalid diff succeeded")
			}
		})
	}
}

func TestReadNativeGitDiffCapKillsChild(t *testing.T) {
	installFakeGit(t)
	pidFile := filepath.Join(t.TempDir(), "diff.pid")
	t.Setenv("FAKE_GIT_PID_FILE", pidFile)
	t.Setenv("FAKE_GIT_MODE", "diff_valid_then_hang")
	_, err := readNativeGitDiff(t.Context(), t.TempDir(), gitObjectID(nativeTestOID), gitObjectID(strings.Repeat("a", 40)), 0)
	if !errors.Is(err, errNativeGitDeltaTooLarge) {
		t.Fatalf("error=%v, want delta cap", err)
	}
	requireProcessGone(t, pidFile)
}

func TestReadNativeGitDiffCancellationKillsChild(t *testing.T) {
	installFakeGit(t)
	pidFile := filepath.Join(t.TempDir(), "diff-cancel.pid")
	t.Setenv("FAKE_GIT_PID_FILE", pidFile)
	t.Setenv("FAKE_GIT_MODE", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := readNativeGitDiff(ctx, t.TempDir(), gitObjectID(nativeTestOID), gitObjectID(strings.Repeat("a", 40)), 10)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline", err)
	}
	requireProcessGone(t, pidFile)
}
