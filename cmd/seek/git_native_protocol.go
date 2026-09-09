package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sourcegraph/zoekt/index"
)

const (
	gitObjectBatchSize = 8192
	gitBatchHeaderMax  = 512
	gitStderrMax       = 32 * 1024
)

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
}

type gitDiffEntry struct {
	oldMode string
	newMode string
	oldOID  gitObjectID
	newOID  gitObjectID
	status  byte
	oldPath string
	newPath string
}

func parseGitObjectID(raw []byte) (gitObjectID, error) {
	if len(raw) != 40 && len(raw) != 64 {
		return "", fmt.Errorf("git object ID has %d bytes, want 40 or 64", len(raw))
	}
	for _, c := range raw {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("git object ID is not full lowercase hexadecimal: %q", raw)
		}
	}
	return gitObjectID(raw), nil
}

func (id gitObjectID) String() string { return string(id) }

func nativeGitCmd(ctx context.Context, repoDir string, args ...string) *exec.Cmd {
	base := []string{"--literal-pathspecs", "--no-pager", "--no-replace-objects"}
	cmd := gitCmd(ctx, append(base, args...)...)
	cmd.Dir = repoDir
	return cmd
}

// boundedGitStderr keeps both ends of Git stderr. Git often puts the cause at
// the start and cleanup details at the end.
type boundedGitStderr struct {
	head      []byte
	tail      []byte
	total     int
	truncated bool
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
	b.truncated = b.total > gitStderrMax
	return n, nil
}

func (b *boundedGitStderr) String() string {
	if !b.truncated {
		return strings.TrimSpace(string(append(append([]byte(nil), b.head...), b.tail...)))
	}
	var out bytes.Buffer
	out.Grow(len(b.head) + len(b.tail) + 64)
	out.Write(bytes.TrimSpace(b.head))
	out.WriteString("\n... Git stderr truncated ...\n")
	out.Write(bytes.TrimSpace(b.tail))
	return strings.TrimSpace(out.String())
}

func nativeGitCommandError(ctx context.Context, operation string, err error, stderr *boundedGitStderr) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err == nil {
		return nil
	}
	message := stderr.String()
	if message == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w: %s", operation, err, message)
}

func nativeGitReadError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func waitNativeGitChild(ctx context.Context, cmd *exec.Cmd, operation string, stderr *boundedGitStderr) error {
	return nativeGitCommandError(ctx, operation, cmd.Wait(), stderr)
}

func abortNativeGitChild(cmd *exec.Cmd, stdin io.Closer) {
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd == nil {
		return
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
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
	header, path, ok := bytes.Cut(record, []byte{'\t'})
	if !ok || len(path) == 0 {
		return gitTreeEntry{}, fmt.Errorf("malformed Git tree record")
	}
	fields := bytes.Split(header, []byte{' '})
	if len(fields) != 3 || len(fields[0]) == 0 || len(fields[1]) == 0 || len(fields[2]) == 0 {
		return gitTreeEntry{}, fmt.Errorf("malformed Git tree header %q", header)
	}
	mode := string(fields[0])
	wantType := "blob"
	switch mode {
	case "100644", "100755", "120000":
	case "160000":
		wantType = "commit"
	default:
		return gitTreeEntry{}, fmt.Errorf("unsupported Git tree mode %q", mode)
	}
	if string(fields[1]) != wantType {
		return gitTreeEntry{}, fmt.Errorf("git tree mode %s has type %q, want %q", mode, fields[1], wantType)
	}
	oid, err := parseGitObjectID(fields[2])
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
	cmd := nativeGitCmd(ctx, repoDir, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open git ls-tree stdout: %w", err)
	}
	var stderr boundedGitStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nativeGitCommandError(ctx, "start git ls-tree", err, &stderr)
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
	scope *gitDirtyScope,
	consume func([]gitTreeEntry) error,
) error {
	args := []string{"ls-tree", "-r", "-z", "--full-tree", "--no-abbrev", snapshot.commitOID.String()}
	if scope != nil {
		args = append(args, "--")
		args = append(args, scope.includeDirs...)
		args = append(args, scope.includeFiles...)
	}
	return readNativeGitTreeRecords(ctx, repoDir, args, consume)
}

func readNativeGitRootIgnoreEntry(ctx context.Context, repoDir string, snapshot gitSnapshot) (*gitTreeEntry, error) {
	args := []string{"ls-tree", "-z", "--full-tree", "--no-abbrev", snapshot.commitOID.String(), "--", ".sourcegraph/ignore"}
	var entries []gitTreeEntry
	err := readNativeGitTreeRecords(ctx, repoDir, args, func(chunk []gitTreeEntry) error {
		entries = append(entries, chunk...)
		if len(entries) > 1 {
			return fmt.Errorf("git tree returned more than one root ignore entry")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	if entries[0].path != ".sourcegraph/ignore" || entries[0].mode == "160000" {
		return nil, fmt.Errorf("root .sourcegraph/ignore is not a blob")
	}
	return &entries[0], nil
}

func writeNativeGitRequests(stdin io.WriteCloser, ids []gitObjectID) <-chan error {
	done := make(chan error, 1)
	go func() {
		writer := bufio.NewWriterSize(stdin, 64*1024)
		var err error
		for _, id := range ids {
			if _, err = writer.WriteString(id.String()); err != nil {
				break
			}
			if err = writer.WriteByte('\n'); err != nil {
				break
			}
		}
		if err == nil {
			err = writer.Flush()
		}
		closeErr := stdin.Close()
		if err == nil {
			err = closeErr
		}
		done <- err
	}()
	return done
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

func parseNativeGitBatchHeader(header []byte, want gitObjectID, wantSize *int64) (int64, error) {
	fields := bytes.Split(header, []byte{' '})
	if len(fields) != 3 {
		return 0, fmt.Errorf("malformed Git cat-file reply %q", header)
	}
	oid, err := parseGitObjectID(fields[0])
	if err != nil {
		return 0, fmt.Errorf("git cat-file reply object: %w", err)
	}
	if oid != want {
		return 0, fmt.Errorf("git cat-file returned object %s, want %s", oid, want)
	}
	if string(fields[1]) != "blob" {
		return 0, fmt.Errorf("git cat-file returned type %q for %s, want blob", fields[1], want)
	}
	size, err := strconv.ParseInt(string(fields[2]), 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("invalid Git blob size %q for %s", fields[2], want)
	}
	if wantSize != nil && size != *wantSize {
		return 0, fmt.Errorf("git blob %s size changed from %d to %d", want, *wantSize, size)
	}
	return size, nil
}

func checkNativeGitBlobChunk(ctx context.Context, repoDir string, entries []gitTreeEntry) ([]gitBlobInfo, error) {
	ids := make([]gitObjectID, len(entries))
	for i := range entries {
		ids[i] = entries[i].oid
	}
	cmd := nativeGitCmd(ctx, repoDir, "cat-file", "--batch-check")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open git cat-file --batch-check stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open git cat-file --batch-check stdout: %w", err)
	}
	var stderr boundedGitStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nativeGitCommandError(ctx, "start git cat-file --batch-check", err, &stderr)
	}
	writeDone := writeNativeGitRequests(stdin, ids)
	reader := bufio.NewReaderSize(stdout, gitBatchHeaderMax+1)
	infos := make([]gitBlobInfo, len(entries))
	for i, entry := range entries {
		header, err := readNativeGitBatchHeader(reader)
		if err != nil {
			abortNativeGitChild(cmd, stdin)
			<-writeDone
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("read git cat-file --batch-check reply %d: %w", i, err)
		}
		size, err := parseNativeGitBatchHeader(header, entry.oid, nil)
		if err != nil {
			abortNativeGitChild(cmd, stdin)
			<-writeDone
			return nil, err
		}
		infos[i] = gitBlobInfo{entry: entry, size: size, oversize: size > maxIndexedDocumentBytes}
	}
	if err := <-writeDone; err != nil {
		abortNativeGitChild(cmd, stdin)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("write git cat-file --batch-check requests: %w", err)
	}
	if err := readExactEOF(reader); err != nil {
		abortNativeGitChild(cmd, nil)
		return nil, nativeGitReadError(ctx, "finish git cat-file --batch-check output", err)
	}
	if err := waitNativeGitChild(ctx, cmd, "git cat-file --batch-check", &stderr); err != nil {
		return nil, err
	}
	return infos, nil
}

func checkNativeGitBlobs(
	ctx context.Context,
	repoDir string,
	entries []gitTreeEntry,
	consume func([]gitBlobInfo) error,
) error {
	for start := 0; start < len(entries); start += gitObjectBatchSize {
		end := min(start+gitObjectBatchSize, len(entries))
		infos, err := checkNativeGitBlobChunk(ctx, repoDir, entries[start:end])
		if err != nil {
			return err
		}
		if err := consume(infos); err != nil {
			return err
		}
	}
	return nil
}

func readNativeGitBlobChunk(
	ctx context.Context,
	repoDir string,
	infos []gitBlobInfo,
	consume func(fileContent) error,
) error {
	ids := make([]gitObjectID, 0, len(infos))
	for i := range infos {
		if !infos[i].oversize {
			ids = append(ids, infos[i].entry.oid)
		}
	}
	if len(ids) == 0 {
		for _, info := range infos {
			if err := consume(fileContent{name: info.entry.path, skipReason: index.SkipReasonTooLarge}); err != nil {
				return err
			}
		}
		return nil
	}
	cmd := nativeGitCmd(ctx, repoDir, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open git cat-file --batch stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open git cat-file --batch stdout: %w", err)
	}
	var stderr boundedGitStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nativeGitCommandError(ctx, "start git cat-file --batch", err, &stderr)
	}
	writeDone := writeNativeGitRequests(stdin, ids)
	reader := bufio.NewReaderSize(stdout, 64*1024)
	replyIndex := 0
	for _, info := range infos {
		if info.oversize {
			if err := consume(fileContent{name: info.entry.path, skipReason: index.SkipReasonTooLarge}); err != nil {
				abortNativeGitChild(cmd, stdin)
				<-writeDone
				return err
			}
			continue
		}
		header, err := readNativeGitBatchHeader(reader)
		if err != nil {
			abortNativeGitChild(cmd, stdin)
			<-writeDone
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("read git cat-file --batch reply %d: %w", replyIndex, err)
		}
		replyIndex++
		if _, err := parseNativeGitBatchHeader(header, info.entry.oid, &info.size); err != nil {
			abortNativeGitChild(cmd, stdin)
			<-writeDone
			return err
		}
		if err := readSemaphore.Acquire(ctx, info.size); err != nil {
			abortNativeGitChild(cmd, stdin)
			<-writeDone
			return err
		}
		content := make([]byte, int(info.size))
		if _, err := io.ReadFull(reader, content); err != nil {
			readSemaphore.Release(info.size)
			abortNativeGitChild(cmd, stdin)
			<-writeDone
			return nativeGitReadError(ctx, fmt.Sprintf("read Git blob %s body", info.entry.oid), err)
		}
		delimiter, err := reader.ReadByte()
		if err != nil || delimiter != '\n' {
			readSemaphore.Release(info.size)
			abortNativeGitChild(cmd, stdin)
			<-writeDone
			if err == nil {
				err = fmt.Errorf("got byte 0x%02x", delimiter)
			}
			return nativeGitReadError(ctx, fmt.Sprintf("read Git blob %s delimiter", info.entry.oid), err)
		}
		document := fileContent{name: info.entry.path, content: content, weight: info.size}
		if err := consume(document); err != nil {
			readSemaphore.Release(info.size)
			abortNativeGitChild(cmd, stdin)
			<-writeDone
			return err
		}
	}
	if err := <-writeDone; err != nil {
		abortNativeGitChild(cmd, stdin)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("write git cat-file --batch requests: %w", err)
	}
	if err := readExactEOF(reader); err != nil {
		abortNativeGitChild(cmd, nil)
		return nativeGitReadError(ctx, "finish git cat-file --batch output", err)
	}
	return waitNativeGitChild(ctx, cmd, "git cat-file --batch", &stderr)
}

func readNativeGitBlobs(
	ctx context.Context,
	repoDir string,
	infos []gitBlobInfo,
	consume func(fileContent) error,
) error {
	for start := 0; start < len(infos); start += gitObjectBatchSize {
		end := min(start+gitObjectBatchSize, len(infos))
		if err := readNativeGitBlobChunk(ctx, repoDir, infos[start:end], consume); err != nil {
			return err
		}
	}
	return nil
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
	name := string(path)
	return gitDiffEntry{
		oldMode: oldMode,
		newMode: newMode,
		oldOID:  oldOID,
		newOID:  newOID,
		status:  status,
		oldPath: name,
		newPath: name,
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
	cmd := nativeGitCmd(ctx, repoDir,
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
		return nil, nativeGitCommandError(ctx, "start git diff-tree", err, &stderr)
	}
	reader := bufio.NewReaderSize(stdout, 64*1024)
	capacity := gitObjectBatchSize
	if maxEntries >= 0 {
		capacity = min(maxEntries, gitObjectBatchSize)
	}
	entries := make([]gitDiffEntry, 0, capacity)
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
		if maxEntries >= 0 && len(entries) > maxEntries {
			abortNativeGitChild(cmd, nil)
			return nil, errNativeGitDeltaTooLarge
		}
	}
}
