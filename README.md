# seek

Ranked local search for AI coding agents. Search the current Git worktree,
selected paths, external folders, or exact files. Single binary, no server, no
API key.

AI coding agents need local context from code, docs, specs, generated files,
notes, and neighboring checkouts. `seek` lets them query the files you point at
and get the best matches first, with symbols and surrounding context included.
It works as a tool call in any agent loop or as a shell command.

<!-- Status -->
[![CI](https://github.com/dualeai/seek/actions/workflows/ci.yml/badge.svg)](https://github.com/dualeai/seek/actions/workflows/ci.yml)
[![CodSpeed](https://img.shields.io/endpoint?url=https://codspeed.io/badge.json)](https://codspeed.io/dualeai/seek?utm_source=badge)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

## Quick Start

```bash
cd your-project

seek 'handleRequest'                 # current Git worktree
seek 'handleRequest' ./src ./cmd     # selected paths
seek 'TODO' ../notes                 # folder outside Git
seek 'needle' ./src/server.go        # exact file
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
```

Results are grouped by file and sorted by relevance. Each match includes 3
lines of surrounding context. Symbol tags like `[func]`, `[class]`, and
`[function]` show metadata from ctags.

## Highlights

- **Search the local working set** -- current Git worktree by default; pass
  paths to search selected files, selected folders, or folders outside Git
- **Best match first** -- results are ranked by BM25 relevance, not file-path
  order
- **Find definitions, not mentions** -- `sym:` search uses universal-ctags for
  functions, classes, methods, and types
- **Context included** -- every match includes 3 surrounding lines, so agents
  can act without an extra read step
- **Filters that cut noise** -- `lang:python`, `file:api`, `-file:test`, and
  `content:regex` narrow a query in one step
- **Sees Git changes** -- modified, staged, and untracked files are indexed
  alongside committed code and tagged `[uncommitted]`
- **Safe for parallel agents** -- concurrent searches use flock-based locking
- **Fast after the first index** -- measured on kubernetes (29k files): cold
  index ~8s, warm search ~145ms including dirty-file re-indexing

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

**Git 2.31+** is required for native [git worktree](https://git-scm.com/docs/git-worktree) support. On older Git versions, normal repos still work.

### Agent Integration

Paste this prompt into your AI coding agent (Claude Code, Codex, Cursor, Amp, etc.) to install seek and configure it for your project. The agent will install the binary, test it, and write usage instructions to your agent config file so that future sessions use seek automatically.

<details>
<summary>Bootstrap prompt -- click to expand, then copy-paste into your agent</summary>

```
Install and configure `seek` for this project. seek is ranked local search for
AI coding agents. It searches the current Git worktree by default, and can also
search selected paths, external folders, and exact files.

Step 1 -- Install

  curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh

If curl is unavailable: go install github.com/dualeai/seek/cmd/seek@latest

universal-ctags is required (used for indexing and symbol search):
  macOS:  brew install universal-ctags
  Linux:  sudo apt-get install universal-ctags

Verify: seek --version

Step 2 -- Test

Run in this project:

  seek 'main'

You should see ranked results with file paths, language labels, line numbers,
and surrounding context.

Step 3 -- Learn the tool

Usage: seek [flags] '<query>' [path...]

The first positional argument is the query. Optional paths after the query
restrict the search to those files or directories. With no paths, seek searches
the current Git worktree. Use single quotes to avoid shell
interpretation of |, (, ).

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
  seek 'handleRequest' ./src ./cmd                  # search paths
  seek 'TODO' ../notes                              # search outside Git
  seek 'needle' ./src/server.go                     # exact file only
  seek 'content:async def.*handler lang:python'     # regex + language
  seek '(lang:go or lang:python) ValidationError'   # multi-language
  seek 'type:file config'                           # find files by name

Output: ranked by BM25 relevance, grouped by file, 3 lines of context.
Symbol lines tagged [func], [class], etc. Modified files tagged [uncommitted].

Exit codes: 0 = matches found, 1 = no matches, 2 = error.

Pitfalls:
  - Query filters stay in ONE argument: seek 'sym:Foo file:bar'
  - Single quotes to prevent shell expanding |, (, )
  - Flags must come before the query: seek -n 5 'Foo' ./src
  - Tokens after the query are filesystem paths, not extra query filters
  - Multi-word queries are AND'd substrings, not phrase match: seek 'foo bar'
    matches files containing both "foo" and "bar" independently
  - Large output: use -n to limit files (seek -n 5 'q') or -m to limit
    matches per file (seek -n 5 -m 3 'q')

Step 4 -- Discover project-specific examples

Run a few searches of varying complexity against this project to find examples
that show where seek helps. Try:
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
  1. Prefer seek when an agent needs ranked local context
  2. Key patterns: sym:, file:, -file:, lang:, content:, paths after the query
  3. The project-specific examples you found in Step 4 (not generic ones)
  4. Pitfalls: query filters in one argument, flags before query, paths after
     query, single quotes
  5. Install command as fallback if seek is not found
  6. When spawning sub-agents that don't inherit the config, pass them a
     one-liner: "Use seek 'pattern' [path...] for code search. Keep query
     filters in one quoted string. Never use grep/rg."
```

</details>

## Usage

```bash
seek [flags] "<query>" [path...]
```

The query is the first positional argument. Optional paths after the query
restrict the search to those files or directories.

- No paths: search the current Git worktree.
- Paths inside the current repo: search only those files or folders.
- External Git repos: use the Git-aware pipeline from the user cache, including
  Git ignore semantics.
- Other external files or folders: index and search them from the user cache as
  standard filesystem corpora.
- Exact non-Git external files search only that file, not sibling files.

Flags must come before the query:

```bash
seek -n 5 -m 3 "handleRequest" ./src
```

Path operands must exist and cannot be symlinks. Invalid paths exit with code
2.
Filters such as `file:api` still live inside the query string.

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

### Flags

| Flag | What it does |
|------|-------------|
| `seek -n 5 "query"` | Display at most 5 files (`--limit`) |
| `seek -m 3 "query"` | Display at most 3 matches per file (`--max-matches`) |
| `seek -n 5 -m 3 "query"` | Top 5 files, max 3 matches each |
| `seek -v "query"` | Enable debug logging (`--verbose`) |

Flags compose with query filters and paths. For example,
`seek -n 3 "sym:handleRequest file:api" ./src` returns the top 3 matching files
under `./src` containing a `handleRequest` definition under paths matching
`api`.

## What seek adds over grep / ripgrep

[ripgrep](https://github.com/BurntSushi/ripgrep) is an excellent tool. seek builds on top of what grep and ripgrep do well, adding capabilities that matter when agents search repeatedly:

| | ripgrep | seek |
|---|---|---|
| **Search model** | Linear scan every query | Trigram index after one-time build |
| **Relevance ranking** | Results in file-path order | Sorted by score, best matches first |
| **Symbol metadata** | None | `[func]`, `[class]`, `[function]`, etc. via ctags |
| **Context lines** | None by default | 3 lines of surrounding code with every match |
| **Uncommitted awareness** | No committed vs. uncommitted distinction | Indexes both separately, tags `[uncommitted]` files |
| **Language detection** | `--type` filter (extension-based) | Labels each file `(Go)`, `(Python)` via [go-enry](https://github.com/go-enry/go-enry) |
| **Parallel agents** | No coordination | flock-based, safe for concurrent use |

seek works alongside ripgrep -- use ripgrep for ad-hoc regex, seek when you want ranked, filtered, context-rich results.

## How It Works

1. **Plan search roots** -- no paths means the current Git worktree. Paths are
   split into current-repo scopes and external file/folder roots.
2. **State check** -- Git roots use `git status`, HEAD SHA, and dirty file
   metadata. Standard folders use a bounded metadata walk.
3. **Index** -- Git roots use the Git-aware pipeline for committed and
   uncommitted files. Standard folders use a bounded folder/file pipeline.
4. **Search** -- loads Zoekt shards for every requested root, runs one query,
   merges results, deduplicates per root, sorts by score, then applies limits.

Indexes are stored centrally in the user cache, never inside searched folders:

- macOS: `~/Library/Caches/seek/corpora/<id>/`
- Linux: `${XDG_CACHE_HOME:-~/.cache}/seek/corpora/<id>/`
- Shards live in `index/`; `.state`, `.head`, and `.lock` live next to it.

Standard folder indexing walks regular files and only prunes `.git` metadata
directories. Dependency, build, cache, and vendor directories are not skipped by
name in standard folder mode. Git ignore semantics are handled only for Git
roots, including external Git roots. Files larger than 100 MiB are skipped, and
standard folder indexing stops at 1,000,000 candidate files or 5 GiB of indexed
bytes.

Field benchmarks on Apple M1 Max (covers Git repos AND non-Git folder
indexing, with PR-scale dirty re-index at 1% and 10% mutation):

| Kind | Workload | Files | Cold index | Warm search | Dirty 1% | Dirty 10% |
|------|----------|-------|------------|-------------|----------|-----------|
| git | spf13/cobra | 66 | 470ms | 80ms | 120ms | 130ms |
| git | prometheus/prometheus | 1,635 | 1.8s | 90ms | 150ms | 380ms |
| git | kubernetes/kubernetes | 30,507 | 11.0s | 180ms | 700ms | 4.3s |
| folder | synthetic-10k | 10,000 | 6.0s | 100ms | 250ms | 750ms |
| folder | synthetic-100k | 100,000 | 33.5s | 250ms | 1.3s | 7.8s |

Single sample per workload; expect ~10–20% run-to-run variance.

Cold index runs once. Every subsequent search hits the warm or dirty path.
Dirty 1%/10% mutate that fraction of files in place before re-indexing —
approximates a small PR vs a large refactor.

Reproduce in one shot (self-contained — clones repos, synthesizes folder
fixtures, emits Markdown table to stdout):

```bash
./cicd/bench-field.sh                    # all workloads (~10 min, requires linux clone)
./cicd/bench-field.sh --no-linux         # ~3 min
./cicd/bench-field.sh --keep             # retain workdir for re-runs
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

Follows the POSIX `grep` / `ripgrep` convention, so `seek` composes naturally in
shell pipelines and conditionals.

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
