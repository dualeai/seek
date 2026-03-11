# Welcome to seek

Single Go binary that wraps [zoekt](https://github.com/sourcegraph/zoekt) for local code search. Indexes git repos (committed + uncommitted files) and searches via trigram index.

See @README for project overview and @Makefile for available commands.

## Commands

```bash
make build        # Build binary
make test         # Run all tests (static + unit)
make test-static  # go vet + golangci-lint
make test-unit    # go test -race
make lint         # golangci-lint --fix
make install      # Download deps + install golangci-lint
make upgrade      # Update Go dependencies
```

## Architecture

All source in `cmd/seek/`:

- `main.go` — CLI entry point, startup sequence, signal handling
- `git.go` — git operations (repo root, repo state via porcelain v2)
- `indexer.go` — state hashing, file locking, committed + uncommitted indexing
- `searcher.go` — query parsing, search execution via zoekt
- `formatter.go` — output formatting (file-grouped, score-sorted, symbol metadata)
- `lock.go` — platform-specific file locking (flock)
- `signal.go` — platform-specific git cancel signal (SIGTERM)
- `clone_darwin.go` — macOS clonefile (CoW) for uncommitted file staging
- `clone_linux.go` — Linux ioctl FICLONE (CoW) for uncommitted file staging
- `clone_other.go` — no-op fallback for other platforms

## Key decisions

- Uses zoekt as a Go library (not CLI) to avoid process spawns
- Single atomic `git status --porcelain=v2 --branch --no-renames -z` eliminates TOCTOU between HEAD and status
- State hash = xxHash64("v3\0" + raw porcelain v2 output) for cache invalidation
- Uncommitted files staged via clonefile (CoW) → hardlink → copy fallback, indexed with index.NewBuilder
- Non-blocking flock with fallback to stale index for parallel agent safety
- Output format: `## file (lang) [uncommitted]` headers with indented line matches
