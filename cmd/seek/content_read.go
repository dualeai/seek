package main

import (
	"bytes"
	"context"
	"io"

	"github.com/sourcegraph/zoekt/index"
)

// binaryProbeBytes is how much of a document Seek reads before it decides that
// the document is binary. Zoekt keeps only the name of a document with a NUL
// byte anywhere (index.SkipReasonBinary). So a NUL in this prefix decides it,
// and Seek does not read the rest. Git checks a prefix of the same size.
const binaryProbeBytes = 8000

// readContent reads a document of size bytes from r. When the first
// binaryProbeBytes hold a NUL byte, it stops the read there. It then returns
// binary=true, no content, and a nil error. consumed is the number of bytes
// that it read from r. Like io.ReadFull, it returns io.EOF or
// io.ErrUnexpectedEOF with the bytes read when r ends early.
func readContent(r io.Reader, size int64) (content []byte, binary bool, consumed int64, err error) {
	head := make([]byte, min(size, binaryProbeBytes))
	n, err := io.ReadFull(r, head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, true, int64(n), nil
	}
	if err != nil || int64(n) == size {
		return head[:n], false, int64(n), err
	}
	content = make([]byte, size)
	copy(content, head)
	rest, err := io.ReadFull(r, content[n:])
	return content[:n+rest], false, int64(n + rest), err
}

// seekNoBytesReason marks the documents that Seek indexes by name because it
// has none of their bytes. These are cloud-only files, blobs that a partial
// clone does not have, and files that Seek cannot read or that grow during the
// read. Zoekt never sets this reason, because TrigramMax equals the content
// limit (indexBuildOptions). So the name-only notice can tell these documents
// from Zoekt's own too-small ones (under 3 bytes).
const seekNoBytesReason = index.SkipReasonTooManyTrigrams

// sendNameOnly sends a document that keeps only its name, with no content and
// no readSemaphore weight, or gives up when ctx ends. Zoekt stores its
// NOT-INDEXED marker for reason, and the name stays searchable.
func sendNameOnly(ctx context.Context, out chan<- fileContent, name string, reason index.SkipReason) {
	select {
	case out <- fileContent{name: name, skipReason: reason}:
	case <-ctx.Done():
	}
}
