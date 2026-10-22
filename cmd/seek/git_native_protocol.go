package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	"github.com/sourcegraph/zoekt/index"
)

// This command protocol is the only reader for committed Git data. The full
// and the delta committed builds both use it with captured full object IDs.
// There is no go-git or zoekt/gitindex fallback. Git reads
// the objects; index.Builder writes the resulting documents to staging.
//
// Full scans use ls-tree, then one buffered batch-command session. `info`
// gets blob sizes before `contents` asks for approved bodies. This keeps
// oversize bodies out of the content stream. Each protocol window handles at
// most gitObjectBatchSize objects. A delta uses one session to size the new
// blobs and read the old ones, and a second session to read the new bodies. A
// malformed or short reply, a write error, or a failed child rejects the
// staged build.
const (
	gitObjectBatchSize = 8192
	gitBatchHeaderMax  = 512
	gitStderrMax       = 32 * 1024
	// cat-file --batch-command needs Git 2.36. A partial clone also needs a Git
	// that honors GIT_NO_LAZY_FETCH (gitHonorsNoLazyFetch).
	minNativeGitMajor = 2
	minNativeGitMinor = 36
)

// errGitBlobMissing reports a blob that the repository does not hold locally.
// In a partial clone, Seek never fetches it (GIT_NO_LAZY_FETCH) and indexes its
// name only; in another repository, it is an error.
var errGitBlobMissing = errors.New("git blob is missing locally")

var errNativeGitDeltaTooLarge = errors.New("native Git delta has too many changed paths")

type gitObjectID string

type gitTreeEntry struct {
	mode string
	oid  gitObjectID
	path string
}

type gitBlobInfo struct {
	entry    gitTreeEntry
	size     int64
	oversize bool
	missing  bool
}

// nameOnly reports a blob whose content Seek does not read.
func (info gitBlobInfo) nameOnly() bool {
	return info.oversize || info.missing
}

// nameOnlyDocument is the index document of a blob whose content Seek does not
// read: Seek's no-bytes reason for a missing blob, the too-large reason for
// an oversize one.
func (info gitBlobInfo) nameOnlyDocument() fileContent {
	if info.missing {
		return fileContent{name: info.entry.path, skipReason: seekNoBytesReason}
	}
	return fileContent{name: info.entry.path, skipReason: index.SkipReasonTooLarge}
}

// gitDiffEntry is one raw diff record. Rename detection is off, so the old and
// the new side have the same path.
type gitDiffEntry struct {
	oldMode string
	newMode string
	oldOID  gitObjectID
	newOID  gitObjectID
	path    string
}

func parseGitObjectID(raw []byte) (gitObjectID, error) {
	if !validGitObjectID(raw) {
		if len(raw) != 40 && len(raw) != 64 {
			return "", fmt.Errorf("git object ID has %d bytes, want 40 or 64", len(raw))
		}
		return "", fmt.Errorf("git object ID is not full lowercase hexadecimal: %q", raw)
	}
	return gitObjectID(raw), nil
}

func validGitObjectID(raw []byte) bool {
	if len(raw) != 40 && len(raw) != 64 {
		return false
	}
	for _, c := range raw {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func parseGitDecimal(raw []byte) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	var n int64
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
		digit := int64(c - '0')
		if n > (maxInt64-digit)/10 {
			return 0, false
		}
		n = n*10 + digit
	}
	return n, true
}

func (id gitObjectID) String() string { return string(id) }

func requireNativeGitVersion(ctx context.Context) error {
	out, err := gitVersion(ctx)
	if err != nil {
		return err
	}
	if gitVersionOutputAtLeast(out, minNativeGitMajor, minNativeGitMinor) {
		return nil
	}
	return fmt.Errorf(
		"direct Git indexing requires Git %d.%d or later",
		minNativeGitMajor,
		minNativeGitMinor,
	)
}

// boundedGitStderr keeps both ends of Git stderr. Git often puts the cause at
// the start and cleanup details at the end.
type boundedGitStderr struct {
	head  []byte
	tail  []byte
	total int
}

func (b *boundedGitStderr) Write(p []byte) (int, error) {
	n := len(p)
	b.total += n
	half := gitStderrMax / 2
	if len(b.head) < half {
		take := min(half-len(b.head), len(p))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		b.tail = append(b.tail, p...)
		if len(b.tail) > half {
			b.tail = append(b.tail[:0], b.tail[len(b.tail)-half:]...)
		}
	}
	return n, nil
}

func (b *boundedGitStderr) String() string {
	if b.total <= gitStderrMax {
		return strings.TrimSpace(string(append(append([]byte(nil), b.head...), b.tail...)))
	}
	var out bytes.Buffer
	out.Grow(len(b.head) + len(b.tail) + 64)
	out.Write(bytes.TrimSpace(b.head))
	out.WriteString("\n... Git stderr truncated ...\n")
	out.Write(bytes.TrimSpace(b.tail))
	return strings.TrimSpace(out.String())
}

func nativeGitReadError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func waitNativeGitChild(ctx context.Context, cmd *exec.Cmd, operation string, stderr *boundedGitStderr) error {
	return gitCommandError(ctx, operation, cmd.Wait(), stderr)
}

func abortNativeGitChild(cmd *exec.Cmd, stdin io.Closer) {
	if stdin != nil {
		_ = stdin.Close()
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func readExactEOF(reader io.Reader) error {
	var one [1]byte
	n, err := reader.Read(one[:])
	switch {
	case n != 0:
		return fmt.Errorf("unexpected trailing Git output")
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return err
	default:
		return fmt.Errorf("git output did not reach EOF")
	}
}

func parseNativeGitTreeRecord(record []byte) (gitTreeEntry, error) {
	if len(record) == 0 || record[len(record)-1] != 0 {
		return gitTreeEntry{}, fmt.Errorf("git tree record has no final NUL")
	}
	record = record[:len(record)-1]
	tab := bytes.IndexByte(record, '\t')
	if tab < 0 || tab == len(record)-1 {
		return gitTreeEntry{}, fmt.Errorf("malformed Git tree record")
	}
	header := record[:tab]
	path := record[tab+1:]
	modeRaw, typeRaw, oidRaw, ok := splitGitHeader(header)
	if !ok {
		return gitTreeEntry{}, fmt.Errorf("malformed Git tree header %q", header)
	}
	var mode string
	wantType := "blob"
	switch string(modeRaw) {
	case "100644":
		mode = "100644"
	case "100755":
		mode = "100755"
	case "120000":
		mode = "120000"
	case "160000":
		mode = "160000"
		wantType = "commit"
	default:
		return gitTreeEntry{}, fmt.Errorf("unsupported Git tree mode %q", modeRaw)
	}
	if string(typeRaw) != wantType {
		return gitTreeEntry{}, fmt.Errorf("git tree mode %s has type %q, want %q", modeRaw, typeRaw, wantType)
	}
	oid, err := parseGitObjectID(oidRaw)
	if err != nil {
		return gitTreeEntry{}, fmt.Errorf("git tree object: %w", err)
	}
	return gitTreeEntry{mode: mode, oid: oid, path: string(path)}, nil
}

func readNativeGitTreeRecords(
	ctx context.Context,
	repoDir string,
	args []string,
	consume func([]gitTreeEntry) error,
) error {
	cmd := newGitCommand(ctx, repoDir, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open git ls-tree stdout: %w", err)
	}
	var stderr boundedGitStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return gitCommandError(ctx, "start git ls-tree", err, &stderr)
	}

	reader := bufio.NewReaderSize(stdout, 64*1024)
	chunk := make([]gitTreeEntry, 0, gitObjectBatchSize)
	var longRecord []byte
	for {
		record, readErr := reader.ReadSlice(0)
		if errors.Is(readErr, bufio.ErrBufferFull) {
			longRecord = append(longRecord, record...)
			continue
		}
		if len(longRecord) > 0 {
			longRecord = append(longRecord, record...)
			record = longRecord
			longRecord = nil
		}
		if len(record) > 0 {
			if errors.Is(readErr, io.EOF) {
				abortNativeGitChild(cmd, nil)
				return nativeGitReadError(ctx, "read git ls-tree", fmt.Errorf("truncated Git tree record"))
			}
			entry, parseErr := parseNativeGitTreeRecord(record)
			if parseErr != nil {
				abortNativeGitChild(cmd, nil)
				return parseErr
			}
			chunk = append(chunk, entry)
			if len(chunk) == gitObjectBatchSize {
				if err := consume(chunk); err != nil {
					abortNativeGitChild(cmd, nil)
					return err
				}
				chunk = make([]gitTreeEntry, 0, gitObjectBatchSize)
			}
		}
		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, io.EOF):
			if len(chunk) > 0 {
				if err := consume(chunk); err != nil {
					abortNativeGitChild(cmd, nil)
					return err
				}
			}
			return waitNativeGitChild(ctx, cmd, "git ls-tree", &stderr)
		default:
			abortNativeGitChild(cmd, nil)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("read git ls-tree: %w", readErr)
		}
	}
}

func readNativeGitTree(
	ctx context.Context,
	repoDir string,
	snapshot gitSnapshot,
	consume func([]gitTreeEntry) error,
) error {
	args := []string{"ls-tree", "-r", "-z", "--full-tree", "--no-abbrev", snapshot.commitOID.String()}
	return readNativeGitTreeRecords(ctx, repoDir, args, consume)
}

func readNativeGitBatchHeader(reader *bufio.Reader) ([]byte, error) {
	header, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(header) > gitBatchHeaderMax+1 {
		return nil, fmt.Errorf("git cat-file header exceeds %d bytes", gitBatchHeaderMax)
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if len(header) == 0 || header[len(header)-1] != '\n' {
		return nil, io.ErrUnexpectedEOF
	}
	return header[:len(header)-1], nil
}

// splitGitHeader splits a Git header "a b c" into its three fields. It reports
// false when a field is empty or the header does not have exactly two spaces.
func splitGitHeader(header []byte) (a, b, c []byte, ok bool) {
	a, rest, first := bytes.Cut(header, []byte{' '})
	b, c, second := bytes.Cut(rest, []byte{' '})
	return a, b, c, first && second && len(a) > 0 && len(b) > 0 && len(c) > 0 && bytes.IndexByte(c, ' ') < 0
}

func parseNativeGitBatchHeader(header []byte, want gitObjectID, wantSize *int64) (int64, error) {
	if oid, ok := bytes.CutSuffix(header, []byte(" missing")); ok && string(want) == string(oid) {
		return 0, errGitBlobMissing
	}
	oidRaw, typeRaw, sizeRaw, ok := splitGitHeader(header)
	if !ok {
		return 0, fmt.Errorf("malformed Git cat-file reply %q", header)
	}
	if !validGitObjectID(oidRaw) {
		return 0, fmt.Errorf("git cat-file reply object is not a full lowercase object ID: %q", oidRaw)
	}
	if string(want) != string(oidRaw) {
		return 0, fmt.Errorf("git cat-file returned object %s, want %s", oidRaw, want)
	}
	if string(typeRaw) != "blob" {
		return 0, fmt.Errorf("git cat-file returned type %q for %s, want blob", typeRaw, want)
	}
	size, ok := parseGitDecimal(sizeRaw)
	if !ok {
		return 0, fmt.Errorf("invalid Git blob size %q for %s", sizeRaw, want)
	}
	if wantSize != nil && size != *wantSize {
		return 0, fmt.Errorf("git blob %s size changed from %d to %d", want, *wantSize, size)
	}
	return size, nil
}

// readNativeGitBlobs emits entries in input order and does not request the
// bodies of name-only blobs (oversize or missing). For each other document, it
// acquires readSemaphore weight before it calls consume. A successful call
// transfers release ownership to the consumer. A binary blob goes to consume
// by name only, with its weight already released. This function releases the
// current weight if reading or consuming fails.
func readNativeGitBlobs(
	ctx context.Context,
	repoDir string,
	infos []gitBlobInfo,
	consume func(fileContent) error,
) error {
	if len(infos) == 0 {
		return nil
	}
	if !slices.ContainsFunc(infos, func(info gitBlobInfo) bool { return !info.nameOnly() }) {
		for _, info := range infos {
			if err := consume(info.nameOnlyDocument()); err != nil {
				return err
			}
		}
		return nil
	}
	batch, err := startNativeGitBatch(ctx, repoDir)
	if err != nil {
		return err
	}
	for start := 0; start < len(infos); start += gitObjectBatchSize {
		end := min(start+gitObjectBatchSize, len(infos))
		if err := batch.read(infos[start:end], consume); err != nil {
			return err
		}
	}
	return batch.close()
}

func parseNativeGitDiffHeader(header, path []byte, oidLength int) (gitDiffEntry, error) {
	if len(header) == 0 || header[0] != ':' || len(path) == 0 {
		return gitDiffEntry{}, fmt.Errorf("malformed Git raw diff record")
	}
	fields := bytes.Split(header[1:], []byte{' '})
	if len(fields) != 5 || len(fields[4]) != 1 {
		return gitDiffEntry{}, fmt.Errorf("malformed Git raw diff header %q", header)
	}
	oldMode := string(fields[0])
	newMode := string(fields[1])
	validMode := func(mode string) bool {
		switch mode {
		case "000000", "100644", "100755", "120000", "160000":
			return true
		default:
			return false
		}
	}
	if !validMode(oldMode) || !validMode(newMode) {
		return gitDiffEntry{}, fmt.Errorf("unsupported Git raw diff modes %q and %q", oldMode, newMode)
	}
	oldOID, err := parseGitObjectID(fields[2])
	if err != nil || len(fields[2]) != oidLength {
		return gitDiffEntry{}, fmt.Errorf("invalid old Git raw diff object %q", fields[2])
	}
	newOID, err := parseGitObjectID(fields[3])
	if err != nil || len(fields[3]) != oidLength {
		return gitDiffEntry{}, fmt.Errorf("invalid new Git raw diff object %q", fields[3])
	}
	status := fields[4][0]
	switch status {
	case 'A':
		if oldMode != "000000" || newMode == "000000" {
			return gitDiffEntry{}, fmt.Errorf("invalid Git add modes %s -> %s", oldMode, newMode)
		}
	case 'D':
		if oldMode == "000000" || newMode != "000000" {
			return gitDiffEntry{}, fmt.Errorf("invalid Git delete modes %s -> %s", oldMode, newMode)
		}
	case 'M', 'T':
		if oldMode == "000000" || newMode == "000000" {
			return gitDiffEntry{}, fmt.Errorf("invalid Git %c modes %s -> %s", status, oldMode, newMode)
		}
	default:
		return gitDiffEntry{}, fmt.Errorf("unsupported Git raw diff status %q", fields[4])
	}
	return gitDiffEntry{
		oldMode: oldMode,
		newMode: newMode,
		oldOID:  oldOID,
		newOID:  newOID,
		path:    string(path),
	}, nil
}

func readNativeGitNULRecord(reader *bufio.Reader) ([]byte, bool, error) {
	var complete []byte
	for {
		part, err := reader.ReadSlice(0)
		if errors.Is(err, bufio.ErrBufferFull) {
			complete = append(complete, part...)
			continue
		}
		complete = append(complete, part...)
		switch {
		case err == nil:
			return complete[:len(complete)-1], true, nil
		case errors.Is(err, io.EOF) && len(complete) == 0:
			return nil, false, nil
		case errors.Is(err, io.EOF):
			return nil, false, io.ErrUnexpectedEOF
		default:
			return nil, false, err
		}
	}
}

// readNativeGitDiff returns raw changes from baseOID to targetOID. Rename
// detection is off, so a rename is one delete and one add. One entry more than
// maxEntries stops Git and returns errNativeGitDeltaTooLarge.
func readNativeGitDiff(
	ctx context.Context,
	repoDir string,
	baseOID gitObjectID,
	targetOID gitObjectID,
	maxEntries int,
) ([]gitDiffEntry, error) {
	if len(baseOID) != len(targetOID) {
		return nil, fmt.Errorf("git delta object formats differ")
	}
	cmd := newGitCommand(ctx, repoDir,
		"diff-tree",
		"-r",
		"--no-commit-id",
		"--raw",
		"-z",
		"--no-abbrev",
		"--no-renames",
		"--ignore-submodules=none",
		baseOID.String(),
		targetOID.String(),
		"--",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open git diff-tree stdout: %w", err)
	}
	var stderr boundedGitStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, gitCommandError(ctx, "start git diff-tree", err, &stderr)
	}
	reader := bufio.NewReaderSize(stdout, 64*1024)
	entries := make([]gitDiffEntry, 0, min(maxEntries, gitObjectBatchSize))
	for {
		header, ok, err := readNativeGitNULRecord(reader)
		if err != nil {
			abortNativeGitChild(cmd, nil)
			return nil, nativeGitReadError(ctx, "read Git raw diff header", err)
		}
		if !ok {
			if err := waitNativeGitChild(ctx, cmd, "git diff-tree", &stderr); err != nil {
				return nil, err
			}
			return entries, nil
		}
		path, ok, err := readNativeGitNULRecord(reader)
		if err != nil || !ok {
			abortNativeGitChild(cmd, nil)
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return nil, nativeGitReadError(ctx, "read Git raw diff path", err)
		}
		entry, err := parseNativeGitDiffHeader(header, path, len(targetOID))
		if err != nil {
			abortNativeGitChild(cmd, nil)
			return nil, err
		}
		entries = append(entries, entry)
		if len(entries) > maxEntries {
			abortNativeGitChild(cmd, nil)
			return nil, errNativeGitDeltaTooLarge
		}
	}
}
