# seek

Indexed code search for git repositories. Finds matches in <400ms regardless of repo size, including uncommitted files.

AI coding agents like [Claude Code](https://claude.com/product/claude-code), [Codex](https://openai.com/codex/), [Cursor](https://www.cursor.com/), and [Amp](https://ampcode.com/) default to grep or ripgrep for code search. On large repos, this [burns tokens on irrelevant matches](https://milvus.io/blog/why-im-against-claude-codes-grep-only-retrieval-it-just-burns-too-many-tokens.md) and [scales linearly with corpus size](https://www.moderne.ai/blog/from-grep-to-moderne-trigrep-code-search-for-agents). seek replaces that with a trigram index powered by [zoekt](https://github.com/sourcegraph/zoekt) (the engine behind [Sourcegraph](https://sourcegraph.com/)) -- build once in ~15s, search in <400ms every time after. Single binary, no server, works as a tool call in any agent loop, an [MCP server](https://modelcontextprotocol.io/), or a shell alias.

<!-- Status -->
[![CI](https://github.com/dualeai/seek/actions/workflows/ci.yml/badge.svg)](https://github.com/dualeai/seek/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

## Highlights

- **Sub-second search on large repos** -- grep is O(corpus) per query; seek is O(matches) after a one-time index build
- **Searches uncommitted files** -- modified, staged, and untracked files are indexed alongside committed code. Agents see the same code you do
- **Single binary, no server** -- `seek "pattern"` returns results and exits. Fits anywhere grep does, but with an index behind it
- **Symbol-aware** -- find definitions with `sym:`, powered by universal-ctags. Agents jump to definitions instead of sifting through every mention
- **Safe for parallel use** -- multiple agents search concurrently without corrupting the index. Essential when tools like Claude Code or Codex [spawn parallel sub-agents](https://openai.com/index/unrolling-the-codex-agent-loop/)

## Install

```bash
brew install dualeai/tap/seek
```

Or with Go:

```bash
go install github.com/dualeai/seek/cmd/seek@latest
```

Or download a pre-built binary from [GitHub Releases](https://github.com/dualeai/seek/releases).

### Prerequisites

[universal-ctags](https://github.com/universal-ctags/ctags) is required for symbol indexing:

```bash
brew install universal-ctags       # macOS
sudo apt-get install universal-ctags  # Linux
```

## Quick Start

```bash
cd your-git-repo
seek "handleRequest"
```

```
## src/server.go (Go)
  15 [function] func handleRequest(w http.ResponseWriter, r *http.Request) {
  42 go handleRequest(w, r)

## lib/middleware.py (Python) [uncommitted]
  10 async def handleRequest(ctx):
```

Results are grouped by file, sorted by relevance. `[uncommitted]` marks files with local changes. `[function]` shows symbol metadata from ctags.

## Query Syntax

| Query | What it does |
|-------|-------------|
| `seek "CoreRouter"` | Substring search across content and file names |
| `seek "sym:CoreRouter"` | Symbol search (function/class/method definitions) |
| `seek "file:router/src"` | Filter results to paths matching `router/src` |
| `seek "-file:test"` | Exclude paths matching `test` |
| `seek "lang:python error"` | Filter by language |
| `seek "content:async def.*handler"` | Regex search |
| `seek "handleError file:api -file:test"` | Combined: substring + path filter + exclusion |

All [zoekt query syntax](https://github.com/sourcegraph/zoekt/blob/main/doc/query_syntax.md) is supported.

## How It Works

1. **State check** -- a single `git status` call captures HEAD SHA and dirty files, hashed for cache invalidation
2. **Index** -- if the cache is stale, builds a trigram index of committed files and stages uncommitted files for separate indexing
3. **Search** -- loads index shards, runs the query, deduplicates results (uncommitted version wins over committed)

The index is stored in `.seek-cache/` at the repo root. First run takes ~15s (dominated by indexing), subsequent searches are <400ms.

### Parallel Safety

When multiple processes search the same repo concurrently:

| Scenario | Behavior |
|----------|----------|
| Index is fresh | All processes search in parallel, no contention |
| Index is stale | First process re-indexes, others use stale index with a warning |
| No index yet | First process indexes, others wait up to 60s |

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success (including zero results) |
| 1 | Error (indexing failed, ctags missing, invalid query) |
| 2 | Usage error (no pattern provided) |

## Security

- [Security Policy](SECURITY.md) -- vulnerability reporting and response timeline
- [SBOM](https://github.com/dualeai/seek/releases) -- CycloneDX Software Bill of Materials attached to each release
- [GitHub Attestations](https://github.com/dualeai/seek/attestations) -- verify build provenance with `gh attestation verify`

## Contributing

Contributions are welcome. Please open an issue to discuss changes before submitting a pull request.

```bash
git clone https://github.com/dualeai/seek.git
cd seek
make install       # Download deps + install linter
make build         # Build binary (requires Go 1.24+)
make test          # Static analysis + unit tests
make lint          # golangci-lint --fix
```

## License

[Apache-2.0](LICENSE)
