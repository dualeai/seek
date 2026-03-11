# seek

Fast local code search powered by [zoekt](https://github.com/sourcegraph/zoekt) trigram indexing. Indexes git repos (committed + uncommitted files) and searches in <400ms.

[![CI](https://github.com/dualeai/seek/actions/workflows/ci.yml/badge.svg)](https://github.com/dualeai/seek/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](https://opensource.org/licenses/Apache-2.0)

## Install

```bash
go install github.com/dualeai/seek/cmd/seek@latest
```

Or with Homebrew:

```bash
brew install dualeai/tap/seek
```

Or download a pre-built binary from [GitHub Releases](https://github.com/dualeai/seek/releases).

### Prerequisites

- **git** — for repository detection and status
- **universal-ctags** — for symbol indexing
  ```bash
  brew install universal-ctags       # macOS
  sudo apt-get install universal-ctags  # Linux
  ```

## Usage

```bash
seek "CoreRouter"                          # Substring search
seek "sym:CoreRouter"                      # Symbol search (ctags)
seek "file:router/src"                     # Path filter
seek "-file:test"                          # Exclude paths
seek "lang:python error"                   # Language filter
seek "content:async def.*handler"          # Regex
seek "ValidationError file:router -file:test"  # Combined
```

All [zoekt query syntax](https://github.com/sourcegraph/zoekt/blob/main/doc/query_syntax.md) is supported.

## How it works

1. **State check** — Hashes `git rev-parse HEAD` + `git status --porcelain` to detect changes
2. **Index** — If stale, locks `.zoekt-index/` and runs zoekt's trigram indexer on committed files (via `gitindex.IndexGitRepo`) and uncommitted files (via `index.NewBuilder`)
3. **Search** — Loads shards from `.zoekt-index/`, parses query, returns results sorted by relevance

### Output format

```
## src/router.go (Go)
  15 [function] func CoreRouter() {
  42 router := CoreRouter()

## lib/utils.py (Python) [uncommitted]
  10 def helper():
```

Files are grouped by path, sorted by score. Lines include symbol metadata when available. `[uncommitted]` marks files with local changes.

### Concurrency

Multiple agents can search in parallel safely:

- **Index fresh** — All agents skip indexing, search concurrently
- **Index stale** — First agent indexes, others search stale index with a warning
- **No index yet** — First agent indexes, others block up to 60s
- Uses `flock` for lock coordination; `--no-optional-locks` on `git status` prevents git index contention

## Performance

| Metric | Shell scripts | seek |
|--------|--------------|------|
| Warm search | ~800ms | <400ms |
| Process spawns | ~8 | ~2 |
| Runtime deps | git, ctags, jq, flock, md5sum | git, ctags |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success (including zero results) |
| 1 | Error (indexing failed, ctags missing, invalid pattern) |
| 2 | Usage error (no pattern provided) |

## Build from source

```bash
git clone https://github.com/dualeai/seek.git
cd seek
go build -ldflags="-s -w" -o seek ./cmd/seek
```

## License

[Apache-2.0](https://opensource.org/licenses/Apache-2.0)
