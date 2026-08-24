# seek

Ranked local search for AI coding agents. `seek` searches your current repo by
default, plus any files or folders you point it at, and returns the best
matches first with definitions and context. Single binary, no server, no API
key.

Built for repeated searches while coding: compact output, fast re-runs after
the first index, and safe use by several agents at once. Works as a tool call
or as a regular shell command.

<!-- Status -->
[![CI](https://github.com/dualeai/seek/actions/workflows/ci.yml/badge.svg)](https://github.com/dualeai/seek/actions/workflows/ci.yml)
[![CodSpeed](https://img.shields.io/endpoint?url=https://codspeed.io/badge.json)](https://codspeed.io/dualeai/seek?utm_source=badge)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

## Quick Start

```bash
cd your-project

seek 'handleRequest'                 # current repo
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
nearby lines. Tags like `[func]` and `[class]` mark definitions. Terminal output
uses color; piped output is plain text, so agents and CI get clean results.
Across multiple folders or repos, file headers use absolute paths so each match
is easy to open.

## Highlights

- **Search what you point at** -- current repo by default; pass files,
  folders, selected paths, or another repo when you need a narrower search.
- **Best match first** -- ranked by relevance, not file-path order
- **Find definitions, not mentions** -- `sym:` searches functions, classes,
  methods, and other symbols
- **Compact output** -- no padding, long lines shortened around the match,
  plain when piped, color on a terminal
- **Context included** -- 3 surrounding lines per match, no extra read step
- **Filters that cut noise** -- `lang:python`, `file:api`, `-file:test`,
  `content:regex` in one query
- **Sees local changes** -- committed files and local edits are searchable
  together; changed files are refreshed between searches
- **Safe for parallel agents** -- several agents can search at once without
  corrupting the index
- **Fast after the first index** -- one-time build, then warm searches in
  milliseconds (benchmarks below)

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

[universal-ctags](https://github.com/universal-ctags/ctags) is required for
`sym:` definition search:

```bash
brew install universal-ctags       # macOS
sudo apt-get install universal-ctags  # Linux
```

**Git 2.31+** is required for [`git worktree`](https://git-scm.com/docs/git-worktree)
setups. On older Git versions, normal repos still work.

### Agent Integration

Install the plugin. It ships two things: a **skill** that teaches the agent
seek's query syntax, and a **router hook** that turns the agent's own
`grep` / `rg` / `git grep` calls into seek searches.

**Claude Code**

```sh
claude plugin marketplace add dualeai/seek
claude plugin install seek-router@seek --scope user
```

**OpenAI Codex**

```sh
codex plugin marketplace add dualeai/seek
codex plugin add seek-router
```

Codex reviews hooks before running them: open `/hooks` and trust `seek-router`
once. Claude Code has no equivalent step.

Try it without installing: `claude --plugin-dir ./plugins/seek-router`.

#### What the router does

A shell search is rewritten in place to the seek equivalent, and seek runs in
the agent's own shell:

```
grep -r "parseToken" --include="*.go" . 2>/dev/null | head -20
  ->  seek -n 20 'content:parseToken'
```

Routed: `grep`, `egrep`, `rg`, `ag`, `ack`, `git grep`. A trailing
`2>/dev/null` or `| head -N` is peeled off first, since neither changes which
files match; `head -N` tightens seek's file cap when it asks for less.

Left alone: pipe filters (`ls | grep foo`, `go test ./... | grep FAIL`),
counts, redirects, compound commands, and any flag the router cannot map. When
in doubt it does nothing and your original command runs.

#### Ranked, not exhaustive

seek ranks by relevance and caps the number of files, so it answers "where is
this" rather than "every occurrence". For a rename, a refactor, or counting
call sites, bypass the router:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

Set `SEEK_ROUTER=off` in the environment to disable routing for a whole
session. The router also stays out of the way when seek is not installed.

#### Without the plugin

Add a short note to your agent's instruction file (`CLAUDE.md`, `AGENTS.md`,
`.cursor/rules`) naming seek and its main filters:

```
Use `seek` for code search, not grep/rg. Usage: seek [flags] '<query>' [path...]
Filters stay in ONE quoted argument: sym:Name (definitions), content:REGEX,
file:path, -file:path, lang:go, type:file. Paths come after the query.
Examples: seek 'sym:ParseToken'   seek 'content:TODO lang:go -file:test' ./cmd
```

## Usage

```bash
seek [flags] "<query>" [path...]
```

The query comes first. Paths after the query choose where to search.

- No paths: search the current Git repo.
- Folders inside Git repos: use Git ignore and include local changes.
- Exact files: search only that file, not sibling files, even when the file is
  inside a Git repo.
- Folders outside Git: search that folder with filesystem rules.
- Nested Git repos inside selected folders are searched once with their own Git
  rules.
- Files or folders ignored by Git stay ignored when you search that repo or a
  folder inside it. Passing an exact ignored file or folder still searches it.

Flags must come before the query:

```bash
seek -n 5 -m 3 "handleRequest" ./src
```

Paths must exist. Symlinks passed on the command line are resolved to their
targets. Broken symlinks and invalid paths exit with code 2. Symlinks found
while walking folders are skipped.
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
| `seek "sym:CoreRouter"` | Find definitions such as functions, classes, methods, and types |

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

More [query syntax](https://github.com/sourcegraph/zoekt/blob/main/doc/query_syntax.md) is supported. Results are ranked by relevance.

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

## What seek adds over ripgrep

[ripgrep](https://github.com/BurntSushi/ripgrep) is excellent for one-off text
search. seek adds the parts agents usually need when they search repeatedly:

| | ripgrep | seek |
|---|---|---|
| **Search model** | Scans files every query | Builds an index once, then reuses it |
| **Relevance ranking** | Results in file-path order | Best matches first |
| **Definitions** | Text matches only | Symbol tags such as `[func]` and `[class]` |
| **Context lines** | None by default | 3 lines of surrounding code with every match |
| **Local changes** | No separate local-change label | Includes and labels local changes |
| **Language detection** | `--type` filter (extension-based) | Labels each file `(Go)`, `(Python)` via [go-enry](https://github.com/go-enry/go-enry) |
| **Parallel agents** | Each command scans on its own | Several agents can use the same index safely |

Use ripgrep for quick raw regex searches. Use seek when you want ranked,
filtered results with context.

## How It Works

1. **Choose where to search** -- no paths means the current Git repo. Exact
   files search only that file. Folders outside Git use normal filesystem
   rules.
2. **Check what changed** -- Git repos use `git status` and the current commit.
   Folders use file size and modification time.
3. **Update the index** -- Git repos keep committed files and local changes
   separate. Folders index regular files directly.
4. **Search** -- reads the index for every selected repo or folder, runs one
   query, merges duplicate results, sorts by relevance, then applies limits.

Indexes are stored centrally in the user cache, never inside searched folders:

- macOS: `~/Library/Caches/seek/corpora/<id>/`
- Linux: `${XDG_CACHE_HOME:-~/.cache}/seek/corpora/<id>/`
- Index files live in `index/`; `.state`, `.head`, and `.lock` live next to it.

Folder searches read regular files and skip `.git` folders. They do not skip
dependency, build, cache, or vendor folders by name. Git ignore rules apply only
inside Git repos. Files larger than 100 MiB are skipped, and folder scans stop
at 1,000,000 candidate files or 10 GiB of indexed bytes.

### Cache maintenance

The cache cleans itself: after each run, seek garbage-collects corpora that
have not been used for 14 days. A corpus counts as used every time it is
searched or indexed. The automatic pass runs at most once per day and is
disabled when the cache lives on a network filesystem.

Environment knobs:

- `SEEK_GC_MAX_AGE` -- eviction TTL (default `14d`; accepts `36h`, `7d`, ...)
- `SEEK_GC_INTERVAL` -- minimum delay between automatic passes (default `24h`)

Manual control:

```sh
seek gc --dry-run --sort=size   # what is eating my disk? (no changes made)
seek gc --force                 # run now, ignore the daily throttle
seek gc --all                   # evict every corpus not actively in use
```

`--sort` orders the table by `name` (default), `age` (oldest first), or
`size` (largest first).

### Benchmarks

Latest field benchmarks, generated on Apple M1 Max / macOS with
`./cicd/bench-field.sh --keep` on 2026-06-21:

| Kind | Workload | Files | Cold index | Warm search | Dirty 1% | Dirty 10% |
|------|----------|-------|------------|-------------|----------|-----------|
| git | spf13/cobra | 66 | 1.1s | 210ms | 260ms | 270ms |
| git | prometheus/prometheus | 1,635 | 2.8s | 250ms | 360ms | 680ms |
| git | kubernetes/kubernetes | 30,507 | 24.8s | 1.4s | 2.1s | 8.8s |
| git | torvalds/linux | 94,541 | 231.6s | 2.4s | 9.8s | 85.7s |
| folder | synthetic-10k | 10,000 | 18.1s | 190ms | 410ms | 1.8s |
| folder | synthetic-100k | 100,000 | 81.6s | 650ms | 2.7s | 17.3s |

Single sample per workload; expect about 10-20% run-to-run variance.

Cold index is the first search. Warm search reuses the index. Dirty 1% and
Dirty 10% measure searches after changing that share of files.

To reproduce the table, run:

```bash
./cicd/bench-field.sh                    # all workloads (includes Linux clone)
./cicd/bench-field.sh --no-linux         # skip the largest clone
./cicd/bench-field.sh --keep             # retain workdir for re-runs
SEEK_BIN=./seek ./cicd/bench-field.sh    # benchmark an explicit binary
```

### Parallel Safety

When multiple `seek` commands search the same repo at the same time:

| Scenario | Behavior |
|----------|----------|
| Index is fresh | All commands search at the same time |
| Index is stale | First command updates it; others use the old index with a warning |
| No index yet | First command builds it; others wait up to 60s |

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success (one or more matches) |
| 1 | No match (query ran successfully, zero results) |
| 2 | Error (usage error, indexing failed, invalid query) |

Uses the same exit-code pattern as `grep` and `ripgrep`, so `seek` works well
in scripts.

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
