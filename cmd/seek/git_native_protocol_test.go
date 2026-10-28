package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

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

// installFakeGit puts a shell script named git alone on PATH. FAKE_GIT_MODE
// selects its reply. A mode with a malformed reply exits 0 after it, unless its
// name ends in _hang. So only a reply check of Seek can fail the call. An
// unknown mode exits 64.
func installFakeGit(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := `#!/bin/sh
# The filter-driver read (git config --get-regexp) finds no driver, as in a
# repository with none: exit 1 and print nothing.
for arg do
  if [ "$arg" = config ]; then
    exit 1
  fi
done
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
	version_snapshot)
		printf 'git version 2.47.GIT\n'
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
    # Each hang loop ends with the test process. So a test that fails at the
    # go test timeout leaves no busy process.
    while kill -0 "$PPID" 2>/dev/null; do :; done
    ;;
  hang)
    printf '%s\n' "$$" > "$FAKE_GIT_PID_FILE"
    trap 'exit 130' INT TERM
    while kill -0 "$PPID" 2>/dev/null; do :; done
    ;;
  batch_check_wrong_id)
    read_object_command
    printf '1123456789abcdef0123456789abcdef01234567 blob 3\n'
    exit 0
    ;;
  batch_check_wrong_type)
    read_object_command
    printf '%s tree 3\n' "$object"
    exit 0
    ;;
  batch_check_invalid_size)
    read_object_command
    printf '%s blob -1\n' "$object"
    exit 0
    ;;
  batch_check_overflow)
    read_object_command
    printf '%s blob 999999999999999999999999999999999999\n' "$object"
    exit 0
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
  batch_check_missing)
    read_object_command
    printf '%s missing\n' "$object"
    exit 0
    ;;
  batch_check_extra)
    read_object_command
    printf '%s blob 3\nEXTRA\n' "$object"
    exit 0
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
    exit 0
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
    exit 0
    ;;
  batch_content_bad_delimiter)
    read_object_command
    printf '%s blob 3\nabcX' "$object"
    exit 0
    ;;
  batch_content_extra)
    read_object_command
    printf '%s blob 3\nabc\nEXTRA' "$object"
    exit 0
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
    exit 0
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
    while kill -0 "$PPID" 2>/dev/null; do :; done
    ;;
  diff_malformed)
    printf ':bad\0path\0'
    exit 0
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

// requireProcessGone fails the test when the fake Git process of pidLine
// still exists. Call it when the call that must kill and join the child
// returns. The join reaps the child, so a live or zombie child is a defect.
func requireProcessGone(t *testing.T, pidLine string) {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(pidLine))
	if err != nil {
		t.Fatalf("parse fake Git PID: %v", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := process.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("fake Git process %d is still running", pid)
	}
}

// readFakeGitPID returns the PID line that the fake Git wrote to pidFile.
func readFakeGitPID(t *testing.T, pidFile string) string {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read fake Git PID: %v", err)
	}
	return string(raw)
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
		{mode: "version_snapshot"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			installFakeGit(t)
			t.Setenv("FAKE_GIT_MODE", tc.mode)
			err := requireNativeGitVersion(t.Context())
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
		err := requireNativeGitVersion(t.Context())
		if !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("error=%v, want Git executable failure", err)
		}
	})
}

func TestGitHonorsNoLazyFetch(t *testing.T) {
	for version, want := range map[string]bool{
		"git version 2.36.0":           false,
		"git version 2.39.3":           false,
		"git version 2.39.4":           true,
		"git version 2.40.1":           false,
		"git version 2.40.2":           true,
		"git version 2.43.0":           false,
		"git version 2.43.4":           true,
		"git version 2.44.1":           true,
		"git version 2.45.0":           true,
		"git version 2.56.0":           true,
		"git version 2.43.4.windows.1": true,
		"git version 2.43.GIT":         false,
		"git version 2.47.GIT":         true,
		"git version 3.0.0":            true,
		"git version garbage":          false,
	} {
		if got := gitHonorsNoLazyFetch([]byte(version + "\n")); got != want {
			t.Errorf("%s: got %t, want %t", version, got, want)
		}
	}
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
		"GIT_NO_LAZY_FETCH=0",
	})
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/bin", "LC_ALL=C", "LANG=C", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("environment %q lacks %q", joined, want)
		}
	}
	for _, unwanted := range []string{"fr_FR", "GIT_DIR=", "GIT_LITERAL_PATHSPECS=", "GIT_NO_LAZY_FETCH=0"} {
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
	err := readNativeGitTree(t.Context(), t.TempDir(), nativeTestOID, func([]gitTreeEntry) error { return nil })
	if err == nil || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("error=%v, want Git executable failure", err)
	}
}

func TestNativeGitTreeChecksLateExitAndBoundedStderr(t *testing.T) {
	installFakeGit(t)
	t.Setenv("FAKE_GIT_MODE", "valid_then_fail")
	seen := 0
	err := readNativeGitTree(t.Context(), t.TempDir(), nativeTestOID, func(entries []gitTreeEntry) error {
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
	err := readNativeGitTree(t.Context(), t.TempDir(), nativeTestOID, func([]gitTreeEntry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "truncated Git tree record") {
		t.Fatalf("error=%v", err)
	}
}

func TestNativeGitTreeParseFailureKillsAndJoinsChild(t *testing.T) {
	installFakeGit(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("FAKE_GIT_PID_FILE", pidFile)
	t.Setenv("FAKE_GIT_MODE", "malformed_then_hang")
	err := readNativeGitTree(t.Context(), t.TempDir(), nativeTestOID, func([]gitTreeEntry) error { return nil })
	if err == nil {
		t.Fatal("malformed tree succeeded")
	}
	requireProcessGone(t, readFakeGitPID(t, pidFile))
}

func TestNativeGitTreeCancellationAndDeadline(t *testing.T) {
	for _, test := range []struct {
		name string
		// context gives the context and the call that ends it.
		context func() (context.Context, func())
		want    error
	}{
		{
			name: "cancellation",
			context: func() (context.Context, func()) {
				ctx, cancel := context.WithCancel(context.Background())
				return ctx, cancel
			},
			want: context.Canceled,
		},
		{
			name: "deadline",
			context: func() (context.Context, func()) {
				ctx := newExpiringContext()
				return ctx, ctx.expire
			},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			installFakeGit(t)
			pidFile := filepath.Join(t.TempDir(), fmt.Sprintf("%s.pid", test.name))
			t.Setenv("FAKE_GIT_PID_FILE", pidFile)
			t.Setenv("FAKE_GIT_MODE", "hang")
			ctx, end := test.context()
			defer end()
			pidLine := stopWhenFakeGitStarts(t, pidFile, end)
			err := readNativeGitTree(ctx, t.TempDir(), nativeTestOID, func([]gitTreeEntry) error { return nil })
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
			requireProcessGone(t, pidLine())
		})
	}
}

// stopWhenFakeGitStarts makes pidFile a FIFO and calls stop when the fake Git
// writes its PID line to it. So a test cancels the child only after it runs,
// however slow the host is. The returned function gives that line. The test
// holds the FIFO open for reading and writing. So the fake Git never waits for
// a reader, and the cleanup ends a read that no fake Git ends.
func stopWhenFakeGitStarts(t *testing.T, pidFile string, stop func()) (pidLine func() string) {
	t.Helper()
	if err := syscall.Mkfifo(pidFile, 0o600); err != nil {
		t.Fatal(err)
	}
	// Linux and macOS open a FIFO for reading and writing with no wait.
	fifo, err := os.OpenFile(pidFile, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		line, _ := bufio.NewReader(fifo).ReadString('\n')
		lines <- line
		stop()
	}()
	t.Cleanup(func() {
		_, _ = fifo.Write([]byte("\n"))
		<-exited
		_ = fifo.Close()
	})
	return func() string {
		select {
		case line := <-lines:
			return line
		default:
			t.Fatal("the fake Git wrote no PID, so the test did not stop it")
			return ""
		}
	}
}

// expiringContext expires with context.DeadlineExceeded when the test calls
// expire, as a timeout does, but at a point that the test chooses.
type expiringContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newExpiringContext() *expiringContext {
	return &expiringContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringContext) expire()               { c.once.Do(func() { close(c.done) }) }
func (c *expiringContext) Done() <-chan struct{} { return c.done }

func (c *expiringContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func fakeTreeEntry(oid string) gitTreeEntry {
	return gitTreeEntry{mode: "100644", oid: gitObjectID(oid), path: "file.go"}
}

// TestScanNativeGitDeltaChangesSplitsChunksAndAcceptsDuplicates scans a diff
// whose new and old blobs fill more than one batch chunk, all with one blob ID
// on each side. Each new blob info keeps its own path across the chunk
// boundary, and the old blobs count their held bytes.
func TestScanNativeGitDeltaChangesSplitsChunksAndAcceptsDuplicates(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	for name, content := range map[string]string{"old": "abc", "new": "abcde"} {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldOID := gitObjectID(gitOutputIn(t, repoDir, "hash-object", "-w", "old"))
	newOID := gitObjectID(gitOutputIn(t, repoDir, "hash-object", "-w", "new"))
	// One more path than half a chunk: the new and old blobs fill two chunks.
	diff := make([]gitDiffEntry, gitObjectBatchSize/2+1)
	for i := range diff {
		diff[i] = gitDiffEntry{oldMode: "100644", newMode: "100644", oldOID: oldOID, newOID: newOID, path: fmt.Sprintf("f%05d.go", i)}
	}
	infos, oldBytes, err := scanNativeGitDeltaChanges(t.Context(), repoDir, diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != len(diff) {
		t.Fatalf("new blob infos=%d, want %d", len(infos), len(diff))
	}
	for i, info := range infos {
		if info.size != 5 || info.entry.oid != newOID || info.entry.path != diff[i].path {
			t.Fatalf("new blob info %d=%+v", i, info)
		}
	}
	if want := int64(3 * len(diff)); oldBytes != want {
		t.Fatalf("old bytes=%d, want %d", oldBytes, want)
	}
}

// startFakeNativeGitBatch starts a batch session on the fake Git of mode.
func startFakeNativeGitBatch(t *testing.T, ctx context.Context, mode string) *nativeGitBatch {
	t.Helper()
	installFakeGit(t)
	t.Setenv("FAKE_GIT_MODE", mode)
	batch, err := startNativeGitBatch(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { batch.abort(nil) })
	return batch
}

// TestNativeGitBatchCheckReplyFailures gives the batch a malformed reply. A bad
// info reply fails check; extra output or a failed exit fails close. Either
// way the call joins the child.
func TestNativeGitBatchCheckReplyFailures(t *testing.T) {
	for _, test := range []struct {
		mode    string
		atClose bool
		want    string
	}{
		{mode: "batch_check_wrong_id", want: "returned object 1123456789abcdef0123456789abcdef01234567"},
		{mode: "batch_check_wrong_type", want: `returned type "tree"`},
		{mode: "batch_check_invalid_size", want: `invalid Git blob size "-1"`},
		{mode: "batch_check_overflow", want: "invalid Git blob size"},
		{mode: "batch_check_missing", want: "missing locally"},
		{mode: "batch_check_extra", atClose: true, want: "unexpected trailing Git output"},
		{mode: "batch_check_nonzero", atClose: true, want: "exit status 9"},
		{mode: "batch_check_long_header", want: "header exceeds 512 bytes"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			batch := startFakeNativeGitBatch(t, t.Context(), test.mode)
			_, err := batch.check([]gitTreeEntry{fakeTreeEntry(nativeTestOID)})
			if test.atClose {
				if err != nil {
					t.Fatalf("check=%v, want the failure at close", err)
				}
				err = batch.close()
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			// Wait sets ProcessState: a nil value is a child that no call
			// joined.
			if batch.execCmd.ProcessState == nil {
				t.Fatal("the failed batch did not join its child")
			}
			// A missing blob in a repository that is not a partial clone is
			// damage: the error names the object and its path.
			if test.mode == "batch_check_missing" && (!errors.Is(err, errGitBlobMissing) ||
				!strings.Contains(err.Error(), nativeTestOID) || !strings.Contains(err.Error(), fakeTreeEntry(nativeTestOID).path)) {
				t.Fatalf("error=%q, want errGitBlobMissing with the object ID and path", err)
			}
		})
	}
}

// TestNativeGitBatchCheckExactLimitAndOversize checks blobs of exactly the
// document limit and one byte more: only the second is oversize.
func TestNativeGitBatchCheckExactLimitAndOversize(t *testing.T) {
	swapForTest(t, &documentContentLimit, 100<<20)
	batch := startFakeNativeGitBatch(t, t.Context(), "batch_check_limits")
	infos, err := batch.check([]gitTreeEntry{
		fakeTreeEntry(nativeTestOID),
		fakeTreeEntry(strings.Repeat("a", 40)),
	})
	if err == nil {
		err = batch.close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].size != 104857600 || infos[0].oversize ||
		infos[1].size != 104857601 || !infos[1].oversize {
		t.Fatalf("blob limits=%+v", infos)
	}
}

// TestNativeGitBatchCheckReadsAndWritesConcurrently checks a full chunk
// against a fake Git that fills its output pipe before it reads all requests:
// check must read while it writes.
func TestNativeGitBatchCheckReadsAndWritesConcurrently(t *testing.T) {
	batch := startFakeNativeGitBatch(t, t.Context(), "batch_check_blocked_pipes")
	entries := make([]gitTreeEntry, gitObjectBatchSize)
	for i := range entries {
		entries[i] = fakeTreeEntry(nativeTestOID)
	}
	infos, err := batch.check(entries)
	if err == nil {
		err = batch.close()
	}
	if err != nil || len(infos) != gitObjectBatchSize {
		t.Fatalf("infos=%d error=%v", len(infos), err)
	}
}

// TestNativeGitBatchCheckCancellationKillsChild expires the context while the
// fake Git hangs: check returns the context error, and the child is gone.
func TestNativeGitBatchCheckCancellationKillsChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "batch.pid")
	t.Setenv("FAKE_GIT_PID_FILE", pidFile)
	ctx := newExpiringContext()
	defer ctx.expire()
	pidLine := stopWhenFakeGitStarts(t, pidFile, ctx.expire)
	batch := startFakeNativeGitBatch(t, ctx, "hang")
	_, err := batch.check([]gitTreeEntry{fakeTreeEntry(nativeTestOID)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline", err)
	}
	requireProcessGone(t, pidLine())
}

func TestReadNativeGitBlobsReadsExactContent(t *testing.T) {
	requireTools(t)
	repoDir := initEmptyGitRepo(t)
	// A binary blob larger than the probe comes first. The reader must drop
	// the rest of it and still read the text blob after it exactly.
	binary := append([]byte{0}, bytes.Repeat([]byte("b"), 3*binaryProbeBytes)...)
	text := []byte("abc\nwith content\n")
	var infos []gitBlobInfo
	for _, blob := range []struct {
		name    string
		content []byte
	}{{"binary", binary}, {"text", text}} {
		if err := os.WriteFile(filepath.Join(repoDir, blob.name), blob.content, 0o644); err != nil {
			t.Fatal(err)
		}
		oid := gitOutputIn(t, repoDir, "hash-object", "-w", blob.name)
		entry := fakeTreeEntry(oid)
		entry.path = blob.name
		infos = append(infos, gitBlobInfo{entry: entry, size: int64(len(blob.content))})
	}
	var got []fileContent
	err := readNativeGitBlobs(t.Context(), repoDir, infos, func(document fileContent) error {
		document.content = append([]byte(nil), document.content...)
		got = append(got, document)
		if document.weight > 0 {
			readSemaphore.Release(document.weight)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("documents = %d, want 2", len(got))
	}
	if got[0].skipReason != index.SkipReasonBinary || len(got[0].content) != 0 || got[0].weight != 0 {
		t.Fatalf("binary blob = %+v, want a name-only binary document", got[0])
	}
	if string(got[1].content) != string(text) {
		t.Fatalf("text content=%q, want %q", got[1].content, text)
	}
}

func TestReadNativeGitBlobsDoesNotStartChildForOversize(t *testing.T) {
	swapForTest(t, &documentContentLimit, 100<<20)
	t.Setenv("PATH", t.TempDir())
	info := gitBlobInfo{entry: fakeTreeEntry(nativeTestOID), size: documentContentLimit + 1, oversize: true}
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

// TestReadNativeGitBlobReplyFailures reads a 3-byte blob from a malformed
// contents reply. Each reply fails the read with the error of its check.
func TestReadNativeGitBlobReplyFailures(t *testing.T) {
	for _, test := range []struct {
		mode string
		want string
	}{
		{mode: "batch_content_truncated", want: "body: unexpected EOF"},
		{mode: "batch_content_bad_delimiter", want: "delimiter: got byte 0x58"},
		{mode: "batch_content_extra", want: "unexpected trailing Git output"},
		{mode: "batch_content_nonzero", want: "exit status 11"},
		{mode: "batch_content_wrong_size", want: "size changed from 3 to 4"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			installFakeGit(t)
			t.Setenv("FAKE_GIT_MODE", test.mode)
			info := gitBlobInfo{entry: fakeTreeEntry(nativeTestOID), size: 3}
			err := readNativeGitBlobs(t.Context(), t.TempDir(), []gitBlobInfo{info}, func(document fileContent) error {
				if document.weight > 0 {
					readSemaphore.Release(document.weight)
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
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
	count := 0
	err := readNativeGitBlobs(t.Context(), t.TempDir(), infos, func(document fileContent) error {
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
	ctx := newExpiringContext()
	defer ctx.expire()
	pidLine := stopWhenFakeGitStarts(t, pidFile, ctx.expire)
	info := gitBlobInfo{entry: fakeTreeEntry(nativeTestOID), size: 3}
	err := readNativeGitBlobs(ctx, t.TempDir(), []gitBlobInfo{info}, func(fileContent) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline", err)
	}
	requireProcessGone(t, pidLine())
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
		if entry.oldMode != test.oldMode || entry.newMode != test.newMode || entry.oldOID.String() != test.oldOID || entry.newOID.String() != test.newOID || entry.path != "a\tpath\n.go" {
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
	for _, test := range []struct {
		mode string
		want string
	}{
		{mode: "diff_valid_then_fail", want: "exit status 12"},
		{mode: "diff_malformed", want: "malformed Git raw diff header"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			installFakeGit(t)
			t.Setenv("FAKE_GIT_MODE", test.mode)
			_, err := readNativeGitDiff(t.Context(), t.TempDir(), gitObjectID(nativeTestOID), gitObjectID(strings.Repeat("a", 40)), 10)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
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
	requireProcessGone(t, readFakeGitPID(t, pidFile))
}

func TestReadNativeGitDiffCancellationKillsChild(t *testing.T) {
	installFakeGit(t)
	pidFile := filepath.Join(t.TempDir(), "diff-cancel.pid")
	t.Setenv("FAKE_GIT_PID_FILE", pidFile)
	t.Setenv("FAKE_GIT_MODE", "hang")
	ctx := newExpiringContext()
	defer ctx.expire()
	pidLine := stopWhenFakeGitStarts(t, pidFile, ctx.expire)
	_, err := readNativeGitDiff(ctx, t.TempDir(), gitObjectID(nativeTestOID), gitObjectID(strings.Repeat("a", 40)), 10)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline", err)
	}
	requireProcessGone(t, pidLine())
}
