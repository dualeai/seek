# Welcome to seek

Single Go binary that wraps [zoekt](https://github.com/sourcegraph/zoekt) for local code search. Indexes git repos (committed + uncommitted files) and searches via trigram index.

## Commands

```bash
go build -ldflags="-s -w" -o seek .   # Build
go test ./...                          # Test
go vet ./...                           # Lint
```

## Architecture

- `main.go` — CLI entry point, startup sequence, signal handling
- `git.go` — git operations (repo root, HEAD, status, remote URL)
- `indexer.go` — state hashing, file locking, committed + uncommitted indexing
- `searcher.go` — query parsing, search execution via zoekt
- `formatter.go` — output formatting (file-grouped, score-sorted, symbol metadata)

## Key decisions

- Uses zoekt as a Go library (not CLI) to avoid process spawns
- State hash = MD5(git HEAD + git status porcelain) for cache invalidation
- Uncommitted files indexed via hardlinks + index.NewBuilder
- Non-blocking flock with fallback to stale index for parallel agent safety
- Output format: `## file (lang) [uncommitted]` headers with indented line matches
