package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/sourcegraph/zoekt/index"
)

// missingBlobWarned holds the repositories whose missing-blob warning this
// command already wrote. The lexical and semantic builds read the same tree,
// so one command warns once per repository.
var missingBlobWarned sync.Map

// nativeGitBatch is one ordered, buffered Git object session. Each request
// group ends with an explicit flush. A full scan reuses one instance for its
// tree stream. Delta preparation and content reads can use separate instances.
// Methods are synchronous and are not safe for concurrent use.
type nativeGitBatch struct {
	ctx      context.Context
	execCmd  *exec.Cmd
	stdin    io.WriteCloser
	writer   *bufio.Writer
	reader   *bufio.Reader
	stderr   boundedGitStderr
	ids      []gitObjectID
	finished bool
	repoDir  string
	missing  int
}

// startNativeGitBatch starts `git cat-file --batch-command --buffer` in
// repoDir.
func startNativeGitBatch(ctx context.Context, repoDir string) (*nativeGitBatch, error) {
	cmd := newGitCommand(ctx, repoDir, "cat-file", "--batch-command", "--buffer")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open git cat-file --batch-command stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open git cat-file --batch-command stdout: %w", err)
	}
	batch := &nativeGitBatch{
		ctx:     ctx,
		execCmd: cmd,
		stdin:   stdin,
		writer:  bufio.NewWriterSize(stdin, 64*1024),
		reader:  bufio.NewReaderSize(stdout, 64*1024),
		repoDir: repoDir,
	}
	cmd.Stderr = &batch.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, gitCommandError(ctx, "start git cat-file --batch-command", err, &batch.stderr)
	}
	return batch, nil
}

// writeCommands writes while the caller reads replies. Git can fill its output
// pipe before it has read a full input group of 8,192 objects. So a write and
// then a read in series can deadlock, also with explicit protocol flushes.
func (b *nativeGitBatch) writeCommands(verb string, ids []gitObjectID) <-chan error {
	done := make(chan error, 1)
	go func() {
		var err error
		for _, id := range ids {
			if _, err = b.writer.WriteString(verb); err != nil {
				break
			}
			if err = b.writer.WriteByte(' '); err != nil {
				break
			}
			if _, err = b.writer.WriteString(id.String()); err != nil {
				break
			}
			if err = b.writer.WriteByte('\n'); err != nil {
				break
			}
		}
		if err == nil {
			_, err = b.writer.WriteString("flush\n")
		}
		if err == nil {
			err = b.writer.Flush()
		}
		done <- err
	}()
	return done
}

// abort waits for the request writer. The batch is already failing, so a
// writer error only goes to the debug log.
func (b *nativeGitBatch) abort(writeDone <-chan error) {
	if !b.finished {
		abortNativeGitChild(b.execCmd, b.stdin)
		b.finished = true
	}
	if writeDone == nil {
		return
	}
	if err := <-writeDone; err != nil {
		slog.Debug("Native Git batch writer failed during abort", "error", err)
	}
}

// check reads the size of the blob of each entry. In a partial clone, a
// missing blob gives a name-only info and counts toward the warning that close
// writes. In any other repository, a missing blob fails the batch.
func (b *nativeGitBatch) check(entries []gitTreeEntry) ([]gitBlobInfo, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	b.ids = b.ids[:0]
	for i := range entries {
		b.ids = append(b.ids, entries[i].oid)
	}
	writeDone := b.writeCommands("info", b.ids)
	infos := make([]gitBlobInfo, len(entries))
	for i, entry := range entries {
		header, err := readNativeGitBatchHeader(b.reader)
		if err != nil {
			b.abort(writeDone)
			if ctxErr := b.ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("read git cat-file info reply %d: %w", i, err)
		}
		size, err := parseNativeGitBatchHeader(header, entry.oid, nil)
		if errors.Is(err, errGitBlobMissing) && repoIsPartialClone(b.ctx, b.repoDir) {
			b.missing++
			infos[i] = gitBlobInfo{entry: entry, missing: true}
			continue
		}
		if err != nil {
			b.abort(writeDone)
			return nil, fmt.Errorf("git blob %s for %q: %w", entry.oid, entry.path, err)
		}
		infos[i] = gitBlobInfo{entry: entry, size: size, oversize: !documentContentFits(size)}
	}
	if err := <-writeDone; err != nil {
		b.abort(nil)
		if ctxErr := b.ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("write git cat-file info requests: %w", err)
	}
	return infos, nil
}

// read passes one document per info to consume, in order: the content, or the
// name only for a missing, oversize, or binary blob. The read weight of a
// document goes to consume when it returns nil; read releases it on an error.
func (b *nativeGitBatch) read(infos []gitBlobInfo, consume func(fileContent) error) error {
	b.ids = b.ids[:0]
	for i := range infos {
		if !infos[i].nameOnly() {
			b.ids = append(b.ids, infos[i].entry.oid)
		}
	}
	if len(b.ids) == 0 {
		for _, info := range infos {
			if err := consume(info.nameOnlyDocument()); err != nil {
				return err
			}
		}
		return nil
	}
	writeDone := b.writeCommands("contents", b.ids)
	replyIndex := 0
	for _, info := range infos {
		if info.nameOnly() {
			if err := consume(info.nameOnlyDocument()); err != nil {
				b.abort(writeDone)
				return err
			}
			continue
		}
		header, err := readNativeGitBatchHeader(b.reader)
		if err != nil {
			b.abort(writeDone)
			if ctxErr := b.ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("read git cat-file contents reply %d: %w", replyIndex, err)
		}
		replyIndex++
		if _, err := parseNativeGitBatchHeader(header, info.entry.oid, &info.size); err != nil {
			b.abort(writeDone)
			return err
		}
		weight := readWeight(info.size)
		if err := readSemaphore.Acquire(b.ctx, weight); err != nil {
			b.abort(writeDone)
			return err
		}
		content, binary, consumed, err := readContent(b.reader, info.size)
		if binary {
			// The stream still carries the rest of the blob: drop it unread.
			_, err = io.CopyN(io.Discard, b.reader, info.size-consumed)
		}
		if err != nil {
			readSemaphore.Release(weight)
			b.abort(writeDone)
			return nativeGitReadError(b.ctx, fmt.Sprintf("read Git blob %s body", info.entry.oid), err)
		}
		delimiter, err := b.reader.ReadByte()
		if err != nil || delimiter != '\n' {
			readSemaphore.Release(weight)
			b.abort(writeDone)
			if err == nil {
				err = fmt.Errorf("got byte 0x%02x", delimiter)
			}
			return nativeGitReadError(b.ctx, fmt.Sprintf("read Git blob %s delimiter", info.entry.oid), err)
		}
		document := fileContent{name: info.entry.path, content: content, weight: weight}
		if binary {
			// Zoekt keeps only the name of a binary blob; nothing holds weight.
			readSemaphore.Release(weight)
			document = fileContent{name: info.entry.path, skipReason: index.SkipReasonBinary}
		}
		if err := consume(document); err != nil {
			readSemaphore.Release(document.weight)
			b.abort(writeDone)
			return err
		}
	}
	if err := <-writeDone; err != nil {
		b.abort(nil)
		if ctxErr := b.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("write git cat-file contents requests: %w", err)
	}
	return nil
}

// close ends the session. It writes the missing-blob warning once per
// repository and command.
func (b *nativeGitBatch) close() error {
	if b.finished {
		return nil
	}
	b.finished = true
	if b.missing > 0 {
		if _, warned := missingBlobWarned.LoadOrStore(b.repoDir, struct{}{}); !warned {
			slog.Warn("Git objects are missing locally; their files are searchable by name only",
				"repo", b.repoDir, "files", b.missing)
		}
	}
	closeErr := b.stdin.Close()
	eofErr := readExactEOF(b.reader)
	waitErr := gitCommandError(b.ctx, "git cat-file --batch-command", b.execCmd.Wait(), &b.stderr)
	if waitErr != nil {
		return waitErr
	}
	if closeErr != nil {
		return fmt.Errorf("close git cat-file --batch-command input: %w", closeErr)
	}
	if eofErr != nil {
		return nativeGitReadError(b.ctx, "finish git cat-file --batch-command output", eofErr)
	}
	return nil
}
