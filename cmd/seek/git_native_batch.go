package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/sourcegraph/zoekt/index"
)

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
}

func startNativeGitBatch(ctx context.Context, repoDir string) (*nativeGitBatch, error) {
	cmd := nativeGitCmd(ctx, repoDir, "cat-file", "--batch-command", "--buffer")
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
	}
	cmd.Stderr = &batch.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nativeGitCommandError(ctx, "start git cat-file --batch-command", err, &batch.stderr)
	}
	return batch, nil
}

// writeCommands writes while the caller reads replies. Git can fill its output
// pipe before it has read a full 8,192-object input group, so serial write then
// read can deadlock even with explicit protocol flushes.
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

func (b *nativeGitBatch) abort(writeDone <-chan error) {
	if b.finished {
		if writeDone != nil {
			<-writeDone
		}
		return
	}
	abortNativeGitChild(b.execCmd, b.stdin)
	b.finished = true
	if writeDone != nil {
		<-writeDone
	}
}

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
		if err != nil {
			b.abort(writeDone)
			return nil, err
		}
		infos[i] = gitBlobInfo{entry: entry, size: size, oversize: size > maxIndexedDocumentBytes}
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

func (b *nativeGitBatch) read(infos []gitBlobInfo, consume func(fileContent) error) error {
	b.ids = b.ids[:0]
	for i := range infos {
		if !infos[i].oversize {
			b.ids = append(b.ids, infos[i].entry.oid)
		}
	}
	if len(b.ids) == 0 {
		for _, info := range infos {
			if err := consume(fileContent{name: info.entry.path, skipReason: index.SkipReasonTooLarge}); err != nil {
				return err
			}
		}
		return nil
	}
	writeDone := b.writeCommands("contents", b.ids)
	replyIndex := 0
	for _, info := range infos {
		if info.oversize {
			if err := consume(fileContent{name: info.entry.path, skipReason: index.SkipReasonTooLarge}); err != nil {
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
		if err := readSemaphore.Acquire(b.ctx, info.size); err != nil {
			b.abort(writeDone)
			return err
		}
		content := make([]byte, int(info.size))
		if _, err := io.ReadFull(b.reader, content); err != nil {
			readSemaphore.Release(info.size)
			b.abort(writeDone)
			return nativeGitReadError(b.ctx, fmt.Sprintf("read Git blob %s body", info.entry.oid), err)
		}
		delimiter, err := b.reader.ReadByte()
		if err != nil || delimiter != '\n' {
			readSemaphore.Release(info.size)
			b.abort(writeDone)
			if err == nil {
				err = fmt.Errorf("got byte 0x%02x", delimiter)
			}
			return nativeGitReadError(b.ctx, fmt.Sprintf("read Git blob %s delimiter", info.entry.oid), err)
		}
		document := fileContent{name: info.entry.path, content: content, weight: info.size}
		if err := consume(document); err != nil {
			readSemaphore.Release(info.size)
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

func (b *nativeGitBatch) close() error {
	if b == nil || b.finished {
		return nil
	}
	b.finished = true
	closeErr := b.stdin.Close()
	eofErr := readExactEOF(b.reader)
	waitErr := waitNativeGitChild(b.ctx, b.execCmd, "git cat-file --batch-command", &b.stderr)
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
