# seek

Ranked, symbol-aware code search for AI agents. Single binary, no server, no API key. ~150ms per search on kubernetes (29k files) -- including re-indexing dirty files.

AI coding agents like [Claude Code](https://claude.com/product/claude-code), [Codex](https://openai.com/codex/), [Cursor](https://www.cursor.com/), and [Amp](https://ampcode.com/) search your code dozens of times per session. With grep or ripgrep, every query returns [unranked results in file-path order](https://milvus.io/blog/why-im-against-claude-codes-grep-only-retrieval-it-just-burns-too-many-tokens.md), forcing agents to read through noise to find what matters. seek gives them the best match first -- ranked by [BM25 relevance](https://en.wikipedia.org/wiki/Okapi_BM25), filtered by language or path, with symbol annotations and surrounding context included. Powered by [zoekt](https://github.com/sourcegraph/zoekt) (the engine behind [Sourcegraph](https://sourcegraph.com/)), it works as a tool call in any agent loop, an [MCP](https://modelcontextprotocol.io/) tool, or a shell alias.

<!-- Status -->
[![CI](https://github.com/dualeai/seek/actions/workflows/ci.yml/badge.svg)](https://github.com/dualeai/seek/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

### Example: dozens of grep matches vs. 1 ranked result

```bash
# ripgrep: dozens of matches, no way to tell which is the definition
$ rg "formatResults"
cmd/seek/main.go:175:   fmt.Print(formatResults(results))
cmd/seek/formatter_test.go:12:  result := formatResults(nil)
cmd/seek/formatter_test.go:34:  result := formatResults(files)
cmd/seek/formatter_test.go:57:  result := formatResults(files)
# ... more matches across the codebase

# seek: the definition, with symbol annotation and context
$ seek "sym:formatResults"
## cmd/seek/formatter.go (Go)
  11
  12 // formatResults formats zoekt FileMatch results into the output format.
  13 // Files are deduplicated (uncommitted wins), sorted by score descending.
  14 [func] func formatResults(files []zoekt.FileMatch) string {
  15     if len(files) == 0 {
  16         return ""
  17     }
```

## Highlights

- **Best match first** -- results ranked by BM25 relevance, not file-path order. Agents get the answer at the top, not buried in the noise
- **Find definitions, not mentions** -- `sym:` search powered by universal-ctags. `seek "sym:handleRequest"` returns the function definition, not every call site
- **Context included** -- 3 lines of surrounding code with every match. Agents understand the code without a follow-up Read call
- **Filters that cut noise** -- `lang:python`, `file:api`, `-file:test` let agents narrow results in a single query. No grep-then-grep-again loops
- **Searches uncommitted files** -- modified, staged, and untracked files are indexed alongside committed code, tagged `[uncommitted]`. Agents see the same code you do
- **Safe for parallel agents** -- multiple agents search concurrently via flock-based locking. Essential when tools like Claude Code or Codex [spawn parallel sub-agents](https://openai.com/index/unrolling-the-codex-agent-loop/)
- **~150ms search** -- trigram index means O(matches) per query, not O(corpus). Measured on kubernetes (29k files): cold index ~21s, every search after ~150ms including dirty-file re-indexing

## Install

```bash
curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh
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

### Agent Integration

Paste this prompt into your AI coding agent (Claude Code, Codex, Cursor, Amp, etc.) to install seek and configure it for your project. The agent will install the binary, test it, and write usage instructions to your agent config file so that future sessions use seek automatically.

<details>
<summary>Bootstrap prompt -- click to expand, then copy-paste into your agent</summary>

```
Install and configure `seek` for this repository. seek is a ranked code search
tool that replaces grep/ripgrep for codebase exploration -- results sorted by
relevance, symbol-aware, with context included.

Step 1 -- Install

  curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh

If curl is unavailable: go install github.com/dualeai/seek/cmd/seek@latest

universal-ctags is required (used for indexing and symbol search):
  macOS:  brew install universal-ctags
  Linux:  sudo apt-get install universal-ctags

Verify: seek --version

Step 2 -- Test

Run in this repo:

  seek 'main'

You should see ranked results with file paths, language labels, line numbers,
and surrounding context.

Step 3 -- Learn the tool

Usage: seek '<query>'

One positional argument. All filters go inside a single quoted string.
Use single quotes to avoid shell interpretation of |, (, ).

Filters (combine with spaces inside the quotes):
  sym:Name        Find definitions (functions, classes, methods) via ctags
  file:path       Include paths matching substring
  -file:path      Exclude paths matching substring
  lang:python     Filter by detected language
  content:regex   Regex on file content only (bare words match content + filenames)
  type:file       Return matching file names only
  case:yes        Force case-sensitive search
  or, ()          Boolean logic (space = implicit AND)

Examples:
  seek 'sym:handleRequest'                          # find definition
  seek 'handleRequest file:api -file:test'          # scoped search
  seek 'content:async def.*handler lang:python'     # regex + language
  seek '(lang:go or lang:python) ValidationError'   # multi-language
  seek 'type:file config'                           # find files by name

Output: ranked by BM25 relevance, grouped by file, 3 lines of context.
Symbol lines tagged [func], [class], etc. Modified files tagged [uncommitted].

Exit codes: 0 = matches found, 1 = no matches, 2 = error.

Pitfalls:
  - ONE argument: seek 'sym:Foo file:bar' not seek 'sym:Foo' 'file:bar'
  - Single quotes to prevent shell expanding |, (, )
  - Multi-word queries are AND'd substrings, not phrase match: seek 'foo bar'
    matches files containing both "foo" and "bar" independently
  - Large output: redirect to file (seek 'q' > /tmp/seek.txt) then read it

Step 4 -- Discover project-specific examples

Run a few searches of varying complexity against this repo to find real examples
that showcase seek's value. Try:
  - A sym: search for a key class or function in the project
  - A scoped search using file: and -file:test
  - A lang: or content: filtered search
  - A type:file search for a common config or entry point

Keep 3-4 queries that returned useful, ranked results. You will use these as
examples in the config file (not the generic examples from Step 3).

Step 5 -- Configure this project

Add seek instructions to this project's agent config so future sessions and
team members use seek automatically:
  - Claude Code     -> CLAUDE.md
  - OpenAI Codex    -> AGENTS.md
  - Cursor          -> .cursor/rules or .cursorrules
  - Other           -> your agent's instruction file

Write concise, task-oriented instructions (not this entire prompt). Include:
  1. Prefer seek over grep/ripgrep for code search
  2. Key patterns: sym:, file:, -file:, lang:, content:
  3. The project-specific examples you found in Step 4 (not generic ones)
  4. Pitfalls: single argument, single quotes
  5. Install command as fallback if seek is not found
  6. When spawning sub-agents that don't inherit the config, pass them a
     one-liner: "Use seek 'pattern' for code search. All filters in ONE
     quoted string. Never use grep/rg."
```

</details>

## Quick Start

```bash
cd your-git-repo
seek "handleRequest"
```

```
## src/server.go (Go)
  12
  13 // handleRequest processes incoming HTTP requests.
  14 // It validates auth and delegates to the appropriate handler.
  15 [func] func handleRequest(w http.ResponseWriter, r *http.Request) {
  16     ctx := r.Context()
  17     log.Info("handling request")
  18     validateAuth(ctx, r)

  40     }
  41     // dispatch to handler
  42     go handleRequest(w, r)
  43     return nil
  44 }

## lib/middleware.py (Python) [uncommitted]
   7
   8 logger = logging.getLogger(__name__)
   9
  10 async def handleRequest(ctx):
  11     """Process incoming request."""
  12     await validate(ctx)
  13     return Response(200)
```

Results are grouped by file, sorted by relevance. Each match includes 3 lines of surrounding context. `[uncommitted]` marks files with local changes. Symbol tags like `[func]`, `[class]`, `[function]` show metadata from ctags (kind varies by language).

## Query Syntax

### Search

| Query | What it does |
|-------|-------------|
| `seek "CoreRouter"` | Substring search across content and file names |
| `seek "content:async def.*handler"` | Search only file content (not file names) |
| `seek "regex:foo.*bar"` | Explicit regex search |

### Symbols

| Query | What it does |
|-------|-------------|
| `seek "sym:CoreRouter"` | Symbol search (definitions via ctags -- functions, classes, methods, types, etc.) |

### Filters

| Query | What it does |
|-------|-------------|
| `seek "file:router/src"` | Filter results to paths matching `router/src` |
| `seek "lang:python error"` | Filter by language |
| `seek "case:yes FooBar"` | Case-sensitive search (`yes`, `no`, `auto`) |
| `seek "type:file config"` | Return matching file names only (no content matches) |

### Boolean Logic

| Query | What it does |
|-------|-------------|
| `seek "-file:test"` | Exclude paths matching `test` |
| `seek "foo or bar"` | Match either term |
| `seek "(foo or bar) lang:go"` | Group expressions with parentheses |
| `seek "handleError file:api -file:test"` | Combined: substring + path filter + exclusion |

All [zoekt query syntax](https://github.com/sourcegraph/zoekt/blob/main/doc/query_syntax.md) is supported. Searches are ranked using [BM25](https://en.wikipedia.org/wiki/Okapi_BM25) scoring for relevance.

## What seek adds over grep / ripgrep

[ripgrep](https://github.com/BurntSushi/ripgrep) is an excellent tool. seek builds on top of what grep and ripgrep do well, adding capabilities that matter when agents search repeatedly:

| | ripgrep | seek |
|---|---|---|
| **Search model** | Linear scan -- O(corpus) per query | Trigram index -- O(matches) after one-time build |
| **Relevance ranking** | Results in file-path order | Sorted by score, best matches first |
| **Symbol metadata** | None | `[func]`, `[class]`, `[function]`, etc. via ctags |
| **Context lines** | None by default | 3 lines of surrounding code with every match |
| **Uncommitted awareness** | No committed vs. uncommitted distinction | Indexes both separately, tags `[uncommitted]` files |
| **Language detection** | `--type` filter (extension-based) | Labels each file `(Go)`, `(Python)` via [go-enry](https://github.com/go-enry/go-enry) |
| **Parallel agents** | No coordination | flock-based, safe for concurrent use |

seek works alongside ripgrep -- use ripgrep for ad-hoc regex, seek when you want ranked, filtered, context-rich results.

## How It Works

1. **State check** -- `git status` captures HEAD SHA and dirty files, hashed for cache invalidation
2. **Index** -- if the cache is stale, builds a trigram index of committed files and reads uncommitted files directly into memory for separate indexing
3. **Search** -- loads index shards, runs the query, deduplicates results (uncommitted version wins over committed)

The index is stored in `.seek-cache/` at the repo root. Benchmarks on Apple M1 Max:

| Repo | Files | Cold index | Warm search | Dirty re-index |
|------|-------|------------|-------------|----------------|
| spf13/cobra | 66 | 0.2s | 74ms | 88ms |
| prometheus/prometheus | 1,583 | 2.2s | 82ms | 94ms |
| kubernetes/kubernetes | 29,179 | 21s | 140ms | 151ms |
| torvalds/linux | 93,016 | 67s | 303ms | 326ms |

Cold index runs once. Every subsequent search hits the warm or dirty path. Reproduce with:

```bash
git clone --depth=1 https://github.com/kubernetes/kubernetes /tmp/k8s
make test-bench-repo SEEK_BENCH_REPO=/tmp/k8s
```

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
| 0 | Success (one or more matches) |
| 1 | No match (query ran successfully, zero results) |
| 2 | Error (usage error, indexing failed, invalid query) |

Follows the POSIX `grep` / `ripgrep` convention, so `seek` composes naturally in shell pipelines and conditionals.

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
make build         # Build binary (requires Go 1.25+)
make test          # Static analysis + unit tests
make lint          # golangci-lint --fix
```

## License

[Apache-2.0](LICENSE)
