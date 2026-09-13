# seek

Ranked local search for AI coding agents. `seek` searches the current Git repo
or the paths you pass, then returns relevant matches with definitions and
context. Plain multi-word descriptions use local model re-ranking by default.
When one stable corpus supports combined lexical and semantic retrieval, Seek
also adds semantic candidates. Seek bundles the code-search model; no server or
API key is needed.

Seek caches indexes for fast repeat searches and safe concurrent use. Run it as
a tool call or shell command.

<!-- Status -->
[![CI](https://github.com/dualeai/seek/actions/workflows/ci.yml/badge.svg)](https://github.com/dualeai/seek/actions/workflows/ci.yml)
[![CodSpeed](https://img.shields.io/endpoint?url=https://codspeed.io/badge.json)](https://codspeed.io/dualeai/seek?utm_source=badge)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

## Quick Start

```bash
cd your-project

seek 'handleRequest'                 # current repo
seek 'sym:handleRequest'             # definition, not calls
seek 'handleRequest' ./src ./cmd     # selected paths
seek 'TODO' ../notes                 # folder outside Git
seek 'needle' ./src/server.go        # exact file
seek 'request authentication flow'   # plain description of this repo
seek --lexical-only 'handleRequest'  # Zoekt only; skip all model work
```

By default, every supported search builds or updates both the Zoekt and
semantic index parts, even when an exact query uses only BM25 results. A first
search of a large repo can take tens of seconds or longer, use several GiB of
memory, and create about 1 GiB of cache data. Use `--lexical-only` when you need
a fast Zoekt-only search with no semantic index or model work.

The first command returns output like this:

```text
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

Results are grouped by file and sorted by relevance. By default, each match
includes up to 3 lines before and after it. Tags like `[func]` and `[class]`
mark definitions. Terminal output uses color; piped output is plain text, so
agents and CI get clean results. Across multiple folders or repos, file headers
use absolute paths so each match is easy to open.

Set `NO_COLOR` to a non-empty value to disable color. Set `CLICOLOR_FORCE` to
force color through a pipe. `NO_COLOR` takes precedence when both are set.

When you know the behavior but not the name, write a plain description:

```bash
seek 'request authentication flow'
```

## Highlights

- **Search what you point at** -- current repo by default; pass files,
  folders, selected paths, or another repo when you need a narrower search.
- **Best match first** -- ranked by relevance, not file-path order
- **Semantic search by default** -- an eligible plain description combines
  text and vector indexes with the bundled local model
- **Find definitions, not mentions** -- `sym:` searches functions, classes,
  methods, and other symbols
- **Compact output** -- no padding, long lines shortened around the match,
  plain when piped, color on a terminal
- **Context included** -- up to 3 lines before and after each match by default,
  no extra read step
- **Filters that cut noise** -- `lang:python`, `file:api`, `-file:test`,
  `content:regex` in one query
- **Sees local changes** -- committed files and local edits are searchable
  together; changed files are refreshed between searches
- **Safe for parallel agents** -- several agents can search at once without
  corrupting the index
- **Index reuse** -- repeat searches reuse the local Zoekt and semantic
  indexes; plain descriptions also run the local model
- **One default index** -- supported builds and updates maintain both index
  parts unless `--lexical-only` is set

## Install

```bash
curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh
```

Or download a pre-built binary from [GitHub Releases](https://github.com/dualeai/seek/releases).

### Prerequisites

[Universal Ctags](https://github.com/universal-ctags/ctags) is required to build
or update an index and to support `sym:` definition search. Put it on `PATH` or
set `CTAGS_COMMAND` to its executable:

```bash
brew install universal-ctags       # macOS
sudo apt-get install universal-ctags  # Linux
```

Git 2.36 or later is required for Git-backed searches. Seek uses the buffered
[`git cat-file --batch-command`](https://git-scm.com/docs/git-cat-file)
protocol added in Git 2.36 for normal repositories and `git worktree` setups.

The search model does not need a model download, an ONNX Runtime install, or an
ML service. The executable contains those resources. Release archives support
macOS 15 or newer on amd64 and arm64, and glibc-based Linux on amd64 and arm64.
The Linux archives are built on Ubuntu 24.04. Alpine and other musl systems are
not supported. The search model adds no system dependency. Seek still needs
Universal Ctags, and it needs Git for Git-backed searches.

### Agent Integration

Install the plugin. It ships two things: a **skill** that teaches the agent
seek's query syntax, and a **router hook** that rewrites supported static
`grep`, `rg`, `git grep`, `fd`, and `find` calls as seek searches.

#### Claude Code

```sh
claude plugin marketplace add dualeai/seek
claude plugin install seek-router@seek --scope user
```

#### OpenAI Codex

```sh
codex plugin marketplace add dualeai/seek
codex plugin add seek-router@seek
```

To update the plugin after a new release:

```sh
claude plugin update seek-router@seek  # Claude Code
codex plugin marketplace upgrade seek # OpenAI Codex
```

Start a new session in either client after each install or update. In Codex,
open `/hooks` and trust `seek-router` once. Claude Code does not require this
trust step.

The package does not use an Agent Plugins 1.0 root manifest because that
standard does not define portable hooks. See the
[compatibility note](plugins/seek-router/README.md#why-there-is-no-agent-plugins-manifest).

The router requires `seek`, `jq`, and a POSIX `awk`. Index updates also require
Universal Ctags on `PATH` or through `CTAGS_COMMAND`. The router stays inside
the plugin and uses the public seek CLI. If a dependency is missing, the hook
leaves the command unchanged.

Try it without installing: `claude --plugin-dir ./plugins/seek-router`.

#### What the router does

A shell search is rewritten in place to the seek equivalent, and seek runs in
the agent's own shell:

```text
grep -rn "parseToken" ./cmd
  -> seek -n 20 -m 3 'case:yes content:"parseToken"' './cmd'
```

The router has a strict adapter for each command. It supports recursive `grep`,
common `rg` location searches, literal `git grep`, ranked-path `fd`, and the
`find ROOT... -type f -name PATTERN` form in either predicate order. It also
converts a final `head` limit for file-name results. Unsupported flags and
dynamic shell syntax stay unchanged. See the
[full contract](plugins/seek-router/README.md#routing-contract).

The router keeps the source command's exact query form. It does not add
`--lexical-only`, so a routed exact search still builds or updates both index
parts while keeping strict BM25 order. Call `seek` directly with a plain
description of two or more words for model re-ranking. Run it with no path in a
clean Git worktree to also use combined lexical and semantic retrieval.

#### Ranked, not exhaustive

The router caps ranked results at 20 files and three matches per file. It
answers "where is this" rather than "every occurrence". For a rename, a
refactor, or a call-site count, bypass the router:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

Set `SEEK_ROUTER=off` in the environment to disable routing for a whole
session. The router also stays out of the way when seek is not installed.

#### Without the plugin

Add a short note to your agent's instruction file (`CLAUDE.md`, `AGENTS.md`,
`.cursor/rules`) naming seek and its main filters:

```text
Use `seek` for ranked code navigation. Usage: seek [flags] '<query>' [path...]
Filters stay in ONE quoted argument: sym:Name (definitions), content:REGEX,
file:path, -file:path, lang:go, type:file. Paths come after the query.
Examples: seek 'sym:ParseToken'   seek 'content:TODO lang:go -file:test' ./cmd
Use a plain description with two or more words for model re-ranking. With no
path in a clean Git worktree, or with one stable plain file or folder, it also
uses semantic retrieval:
seek 'request auth flow'
Every default search maintains Zoekt and semantic data for a supported corpus.
Use --lexical-only to skip semantic index and model work.
For all occurrences, counts, or renames, run: grep -rn 'PATTERN' .
If the seek router is installed, prefix this command with SEEK_ROUTER=off.
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
- Visible nested Git worktrees inside selected folders are searched once with
  their own Git rules.
- Files or folders ignored by Git stay ignored when you search that repo or a
  folder inside it. Passing an exact ignored file or folder still searches it.

Flags must come before the query:

```bash
seek -n 5 -m 3 "handleRequest" ./src
```

Paths must exist. Symlinks passed on the command line are resolved to their
targets. Broken symlinks and invalid paths exit with code 2. Symlinks found
while walking folders are skipped.

Path operands constrain the search results. They can also enable a scoped index
when a whole Git repo exceeds an index limit. Query filters such as `file:api`
and `-file:test` filter results after indexing. They do not reduce index limits.

## Query Syntax

### Search

| Query | What it does |
|-------|-------------|
| `seek "CoreRouter"` | Substring search across content and file names |
| `seek "content:async def.*handler"` | Search file content, not file names |
| `seek "regex:foo.*bar"` | Explicit regex search |

### Symbols

| Query | What it does |
|-------|-------------|
| `seek "sym:CoreRouter"` | Find function, class, method, and type definitions |

### Filters

| Query | What it does |
|-------|-------------|
| `seek "file:router/src"` | Filter results to paths matching `router/src` |
| `seek "lang:python error"` | Filter by language |
| `seek "case:yes FooBar"` | Case-sensitive search (`yes`, `no`, `auto`) |
| `seek "type:file config"` | Return file names without content matches |

### Boolean Logic

| Query | What it does |
|-------|-------------|
| `seek "-file:test"` | Exclude paths matching `test` |
| `seek "foo or bar"` | Match either term |
| `seek "(foo or bar) lang:go"` | Group expressions with parentheses |
| `seek "handleError file:api -file:test"` | Combine content and path filters |

More [query syntax](https://github.com/sourcegraph/zoekt/blob/df97bab6f7bb/doc/query_syntax.md)
is supported by the pinned Zoekt version. Results are ranked by relevance.

### Flags

| Flag | What it does |
|------|-------------|
| `seek -n 5 "query"` | Display at most 5 files (`--limit`) |
| `seek -m 3 "query"` | Display at most 3 matches per file (`--max-matches`) |
| `seek -A 5 "query"` | Show 5 lines after each match (`--after-context`) |
| `seek -C 5 "query"` | Show 5 lines on both sides (`--context`) |
| `seek -n 5 -m 3 "query"` | Top 5 files, max 3 matches each |
| `seek -v "query"` | Show debug logs and detailed errors (`--verbose`) |
| `seek --lexical-only "query"` | Skip semantic indexing, search, and model re-ranking |

`-n` and `-m` use `0` by default, which adds no display limit. `-A` and `-C`
accept values from 0 to 512 and cannot be used together.

Flags combine with filters and paths. For example,
`seek -n 3 "sym:handleRequest file:api" ./src` returns up to 3 files under
`./src` with `api` in the path that define `handleRequest`.

### Semantic search and code re-ranking

Use a plain description when you know the behavior but not its name or
location. Seek uses its bundled
[LateOn-Code-edge](https://huggingface.co/lightonai/LateOn-Code-edge/tree/4bcdf5ed93f791259eb130b577a240f753d68dd8)
model for semantic retrieval and final re-ranking.

```bash
seek 'find request parser'
seek -n 5 -m 2 -C 1 'validate search query syntax'
seek 'find request parser lang:go -file:_test\.go$'
seek 'validate cache generation file:^cmd/seek/'
```

Model re-ranking is on by default for plain queries with two or more words.
Such a description can also contain `lang:`, `file:`, and `-file:` filters. Seek
keeps these filters in strict, relaxed, and semantic retrieval. It sends only
the description text to the model. For semantic retrieval, Seek builds an
allowed-row bitmap and plans each shard separately. It skips shards with no
allowed rows, uses normal USearch for full shards, scores bounded sparse partial
shards exactly, and gives other partial shards to USearch with the bitmap
predicate. The predicate admits only matching keys to the candidate set; other
keys can still guide graph navigation. A filter that selects every row uses
normal semantic retrieval. Native USearch routes remain approximate. An
exact-only route scores every selected stored row. Eligibility follows Zoekt's
parsed and simplified query tree. An equivalent spelling or alias that Zoekt
reduces to the same supported nodes can use this route too.
Combined lexical and semantic retrieval also runs when the search has one
unscoped, clean Git worktree; one stable plain file; or one stable plain folder
with no nested Git worktree. A scoped Git path, dirty Git state, multiple
corpora, or a nested corpus uses lexical retrieval and can use model re-ranking
when the model and enough candidates are available. Verbose diagnostics and
cache names call the combined path the joined path.

Exact identifiers and phrases, one-word queries, and queries whose simplified
tree still contains `sym:`, another filter node, a Boolean alternative, or
general negation use strict BM25 retrieval and order. A filename regular
expression inside `file:` or `-file:` can use the filtered description route.
Exact queries still build or update both index parts by default for a supported
committed Git or folder corpus. Use `--lexical-only` to avoid that semantic
index work.

Seek collects up to 20 candidate files from strict lexical, relaxed lexical,
and semantic retrieval. The model scores each path with its best nearby code.
Seek combines the lexical and model orders with weighted reciprocal rank
fusion; lexical rank has twice the model weight. This can find a useful file
that has none of the query words. Normal all-word lexical matches remain,
subject to the display limits.

Use `--lexical-only` when you need the strict BM25 fast path. This flag skips
semantic index build, update, open, query, and model re-ranking work.

The `-n`, `-m`, `-C`, and `-A` flags still control displayed output. The model
can use nearby source context that these flags do not display.

The model runs locally and needs no download or service. Semantic indexing uses
more time and memory than BM25. Its first use is slower while Seek prepares the
embedded runtime and builds both index parts. Later searches reuse them.
Seek runs the Zoekt and semantic builders at the same time and gives them the
complete effective compute budget. During a build, it sets Go worker and model
call limits from `GOMAXPROCS` and available memory. It tests wider concurrency,
keeps a faster width, and reduces or restores work when resource limits change.
It does not use a fixed worker target. Set `GOMAXPROCS=N` to reduce Go-side
concurrency. This is not a hard process CPU limit: Core ML, native libraries,
and ctags can use compute outside the Go scheduler. On Apple silicon, Core ML
can put most model work on the GPU, so CPU use alone does not show total compute
use.

Fallback behavior is specific:

- `--lexical-only` uses strict Zoekt/BM25 only.
- If joined retrieval is ineligible or unavailable, Seek can use strict and
  relaxed lexical candidates with LateOn re-ranking.
- For an unfiltered description, if USearch cannot search a valid generation,
  Seek uses an exact scan only when its row and query-token work is bounded.
  A larger failure returns to the lexical and model re-rank path.
- For a filtered description, a filtered USearch error returns to the same
  filtered lexical and model re-rank path. Seek does not widen the filter.
- If the model or re-ranking fails, Seek returns the strict all-word BM25
  results.

Use `--verbose` to show the error that caused a fallback.

### Shell completion

Run `seek completion <shell> --help` to set up Bash, Zsh, fish, or PowerShell.

## What seek adds over ripgrep

[ripgrep](https://github.com/BurntSushi/ripgrep) is excellent for one-off text
search. seek adds the parts agents usually need when they search repeatedly:

| | ripgrep | seek |
|---|---|---|
| **Search method** | Scans files for each command | Builds, then reuses a local index |
| **Default directory scope** | Current directory | Current Git worktree; outside Git, pass a path |
| **Default order** | Unspecified; `--sort path` gives stable path order | BM25 for exact queries; fused lexical and model order for eligible descriptions |
| **Plain descriptions** | Literal (`-F`) or regular expression | Uses a local model by default and adds semantic retrieval for one eligible corpus |
| **Symbols** | No symbol index | `sym:` finds definitions and tags symbols |
| **Context** | None by default; use `-A`, `-B`, or `-C` | Up to 3 lines on each side by default |
| **Local changes** | Current file contents; no status label | Committed and working-tree content; changed results get `[uncommitted]` |
| **Languages** | Built-in and custom file-type globs | [go-enry](https://github.com/go-enry/go-enry) detection and `lang:` filters |
| **Concurrent use** | Stateless | Agents share locked indexes |

Use ripgrep for all matching lines, counts, multiline regular expressions,
optional PCRE2 features, or replacement output. Replacement changes output,
not files. Use seek for ranked navigation, definitions, compact context, or a
description of the code.

## How It Works

1. **Choose where to search** -- no paths means the current Git repo; outside
   Git, pass a path. Exact files search only that file. Folders outside Git use
   normal filesystem rules.
2. **Check what changed** -- Git repos use `git status` and the current commit.
   Folders use file size and modification time.
3. **Update both index parts** -- by default, a supported committed Git or
   folder corpus builds Zoekt and semantic data at the same time. An unborn Git
   worktree has no committed semantic source. `--lexical-only` skips all
   semantic index and model work and uses only Zoekt.
4. **Search** -- exact query forms use strict lexical retrieval. For one
   unscoped, stable corpus, an eligible description, with or without supported
   file and language filters, starts strict lexical, relaxed lexical, and
   semantic work together.
5. **Re-rank** -- for an eligible description, the same local model scores the
   bounded candidate union when the model and enough candidates are available.
6. **Format** -- applies the display limits and writes grouped results with the
   requested context.

### Git indexing

Seek has one reader for committed Git data. Normal full indexing, committed
delta indexing, and scoped full indexing all use Git plumbing with captured
full object IDs. `git ls-tree` lists files. During a full or scoped full scan,
one buffered `git cat-file --batch-command` session reads sizes and approved
blob bodies. `git diff-tree` lists changed paths for eligible deltas. A delta
uses the same checked protocol in separate bounded phases for its ignore file,
blob sizes, and blob bodies. These commands provide documents to
`zoekt/index.Builder`, which writes shards in the staging directory. There is
no go-git, `zoekt/gitindex`, or other committed-data fallback.

A normal whole-repository full build saves its candidate count, indexed byte
count, commit, and base shard count. Before a delta, Seek checks the target root
`.sourcegraph/ignore` file. Delta budget admission then checks only the old and
new blobs from `git diff-tree` and updates the saved totals. The 64-shard value
is a pre-admission threshold for shards added after the full base, not a hard
maximum for the resulting family. A large full index can therefore use deltas.
After the added-shard count goes above 64, the next update selects compaction.

Zoekt indexing does not reduce index quality to save memory. It sends every
supported file up to 100 MiB through the same content and symbol-analysis path.
A file larger than one normal shard still gets full content and ctags analysis.
Seek only limits large shard and ctags jobs to three at a time across active
corpora.

Git is the data format and object source. Its hosting service does not select
index behavior or metadata policy. An origin URL does not change repository
identity, rank, or links. A local `[zoekt]` section can set a repository name
and one opaque `web-url` explicitly.

This release replaces the go-git committed reader and adds `native-v1` to the
Git corpus identity. This is the migration barrier: a native delta cannot seed
shards made by the removed reader. The first committed build after the upgrade
is a clean full build. Dirty-file rules do not change, but dirty shards share
the new Git corpus cache and rebuild there. Folder caches do not change.

Indexes are stored centrally in the user cache, never inside searched folders.
Set `SEEK_CACHE_DIR` to use a different root for the complete Seek cache. When
it is not set, Seek uses these paths:

- macOS: `~/Library/Caches/seek/corpora/<id>/`
- Linux: `${XDG_CACHE_HOME:-$HOME/.cache}/seek/corpora/<id>/`
- Index files live in `index/`; `.state`, `.head`, `.git-committed-v1`, and
  `.lock` live next to it.

Joined corpora also store semantic generations in `index/` and a `.joined-v1`
attachment next to the lexical state. Normal corpus eviction removes both.
Seek rejects an older semantic format. The first search that enables semantic
indexing after an upgrade rebuilds it. Semantic rows keep the same canonical
file language as Zoekt. Seek rounds each normalized FP32 fine-vector component
to a signed 16-bit integer. This storage is lossy. It can change semantic scores
and the order of close semantic matches after an upgrade. Rows and USearch graph
shards keep their existing formats.
The old and new semantic generations exist at the same time during the rebuild.
Both need disk space during this period. After activation, Seek tries to remove
older generations. After a rollback, an older Seek binary can rebuild its
float32 format. Seek does not support alternating old and new binaries with one
cache. Each switch can cause another rebuild. Use `--lexical-only` when semantic
rebuild or recovery is not wanted.

The model stores its extracted runtime under
`<seek-cache>/reranker/<runtime-version>/<sha256>/`. The model and tokenizer
stay in the executable. Darwin arm64 uses the ONNX Runtime Core ML provider and
requests all Core ML compute units. If that provider cannot start, Seek uses
the CPU provider. Darwin amd64 and both Linux targets use the CPU provider.
Core ML can create its compiled model under `<seek-cache>/reranker/coreml/`.
The cache key includes the model bytes, loaded ONNX Runtime version, and Core ML
settings. This setting does not prove Neural Engine use; device placement must
be measured on the host.

Seek stores its extracted USearch library under
`<seek-cache>/semantic/usearch/<asset-id>/`. Seek reads the USearch version from
the loaded library and records it in each semantic generation.

Folder searches read regular files and skip `.git` folders. They do not skip
dependency, build, cache, or vendor folders by name. Git ignore rules apply only
inside Git repos. Files larger than 100 MiB are skipped, and folder scans stop
at 1,000,000 candidate files or 10 GiB of indexed bytes.

The semantic branch skips empty files, files that contain a NUL byte, and files
with the standard `// Code generated ... DO NOT EDIT.` marker in the first
4 KiB. These files remain available through Zoekt when the lexical index accepts
them; they do not supply semantic candidates.

Git applies the 10,000,000-file and 10 GiB work limits separately to its
committed and working-tree index families. The committed family counts all
candidate blobs. Its byte total includes candidate blobs at or below the
100 MiB document limit. It calculates both totals before
`.sourcegraph/ignore` filtering. The working-tree family counts selected
regular-file content. If the full repository exceeds a limit, a scoped search
can build a combined fallback for its selected paths. With a committed HEAD,
the default fallback builds scoped Zoekt and semantic data together. Dirty
files remain searchable through Zoekt and make joined retrieval ineligible.
An unscoped search reports the limit error.

### Cache maintenance

The cache cleans itself: after each run, seek garbage-collects corpora that
have not been used for 14 days. A corpus counts as used every time it is
searched or indexed. The automatic pass runs at most once per day and is
disabled when the cache lives on a network filesystem. GC has no total-size
target, so active or recent corpora can use more than a fixed total size.

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

`seek gc` manages only `<seek-cache>/corpora/`. It does not remove the extracted
model runtime, compiled Core ML models, or extracted USearch libraries. Those
provider caches are rebuildable. Remove their `reranker/` and
`semantic/usearch/` directories only when no `seek` process is running.

### Benchmarks

For repeat measurements of one build, keep the binary, Seek revision, corpus
revision, query set, provider, CPU allowance, and memory limit the same. For a
before-and-after comparison, record both binary hashes and Seek revisions. Keep
the corpus revision, query set, provider, CPU allowance, and memory limit fixed.
Do not publish a result from an uncommitted development binary as a result for
its base commit.

Every default query, including an exact or one-word query, builds or updates
semantic data for a supported corpus. Use `--lexical-only` when you need to
measure only Zoekt. To run the field matrix on your host, use:

```bash
./cicd/bench-field.sh                    # all workloads (includes Linux clone)
./cicd/bench-field.sh --no-linux         # skip the largest clone
./cicd/bench-field.sh --keep             # retain workdir for re-runs
SEEK_BIN=./seek ./cicd/bench-field.sh    # benchmark an explicit binary
```

The field script reports the current checkout. It does not reproduce results
from an older implementation.

The retained large-repository gate uses Kubernetes commit
[`912ec35`](https://github.com/kubernetes/kubernetes/commit/912ec3583d7733a240dad6a3755f5f2f6b76be3e).
The recorded reference host for its limits is an Apple M5 Pro with 18 logical
CPUs and 64 GiB RAM. Label results from other hosts separately. Run the gate
only on a clean, pinned checkout. The gate restores the native tokenizer
library from its tracked archive, then builds its own binary from the current
checkout. It rejects `SEEK_BIN` so the source model benchmark and cold binary
cannot use different revisions. It records and rechecks the source revision,
binary build revision and hash, native asset hashes, and corpus revision. It
also stores timing, process-tree CPU, RSS, swap, logs, and the last cache in a
new temporary folder. Keep the retained summary with any published result:

```bash
make test-bench-semantic \
  SEEK_BENCH_REPO=/path/to/kubernetes
```

Each cold run must cover 31,250 files and 204,694 semantic rows. Format 5 uses
393,012,480 fine-vector bytes instead of format 4's 786,024,960 bytes: 1,920
bytes per row from 20 centroids of 48 signed 16-bit values. The format layout
makes this an exact 50% reduction and saves 393,012,480 logical bytes for this
row count. Filesystem allocation can differ. Zoekt and other corpus files add
more space.

The gate requires at least 10 cold application runs and 10 fixed 100,000-row
model runs. It requires a cold nearest-rank p95 of at most 60 seconds, a model
p50 below 30 seconds, mean and median process-tree CPU use of at least 90% of
the effective CPU budget, every covered five-second window at least 85%, and
no new swap. Its generated summary reports the measured CPU, GPU, memory,
cache, scheduler, and limit values. These are proof limits, not runtime settings
or performance guarantees. Use
`SEEK_BENCH_REPO=/path/to/kubernetes uv run --script ./cicd/bench-semantic.py --samples 1 --report-only`
for a wiring check. Report-only mode can use `SEEK_BIN=/path/to/seek` for a
diagnostic run, but it does not apply the limits. A CPU failure stays a failure;
GPU or memory samples do not hide it.

### Parallel Safety

When multiple `seek` commands search the same repo at the same time:

| Scenario | Behavior |
|----------|----------|
| Index is fresh | All commands search at the same time |
| Update active | One command builds while others search the current index. Readers wait during publication; after 10 seconds, Seek warns and can read the shards that remain |
| Update fails; shards remain | Except for file or byte limit errors, Seek warns and reads the remaining shards. The next build repairs an interrupted swap |
| No index yet | First command builds it; others wait up to 60s |

### Search Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success (one or more matches) |
| 1 | No match (query ran successfully, zero results) |
| 2 | Error (usage error, indexing failed, invalid query) |

Uses the same exit-code pattern as `grep` and `ripgrep`, so `seek` works well
in scripts.

## Security

- [Security Policy](SECURITY.md) -- vulnerability reporting and response
  timeline
- [SBOM](https://github.com/dualeai/seek/releases) -- CycloneDX Software Bill
  of Materials attached to each release
- [Third-party notices](THIRD_PARTY_NOTICES.md) -- model and native runtime
  sources, versions, and licenses
- [GitHub Attestations](https://github.com/dualeai/seek/attestations) -- verify
  build provenance with `gh attestation verify`

## Contributing

Contributions are welcome. Please open an issue to discuss changes before
submitting a pull request.

```bash
git clone https://github.com/dualeai/seek.git
cd seek
make install  # Download modules and install test tools
make build    # Build Seek
make package  # Build the archive for this native target
make test     # Run static analysis and unit tests
make lint     # Run golangci-lint with fixes
```

Go 1.27 or newer is required. A build also needs a native C compiler: the Xcode
command-line tools on macOS or GCC on glibc-based Linux. Use `make build` so the
build prepares the native tokenizer library. Seek does not support a reduced
non-CGO binary.

### Release packaging

`make package` builds Seek and creates the archive for the current native
target. Each archive contains the `seek` executable, `LICENSE`, and
the two third-party notice files. The release workflow runs this target on
macOS and Linux, on amd64 and arm64. It does not cross-build. You can run the
target again to replace its output.

The release workflow downloads the four archives, generates the standard
CycloneDX SBOM, computes `checksums.txt`, and uploads these files. Artifact and
GitHub release uploads replace files with the same names, so a failed workflow
can run again.

`RELEASE_TAG=vX.Y.Z make release` repeats only the final upload after the four
archives and `sbom.cyclonedx.json` exist. It needs the GitHub CLI, `gh`. The
named GitHub release must already exist, and `gh` must have permission to upload
to it.

### Search resource update

`make search-assets-upgrade` is a manual maintainer command. It downloads the
model, tokenizer, native tokenizer archives, USearch libraries, and official
ONNX Runtime packages at fixed revisions. Microsoft does not publish an ONNX
Runtime 1.30.0 macOS amd64 package, so the command builds that library from the
fixed source commit when its versioned cache entry is absent.

The command needs macOS, `curl`, `zstd`, `unzip`, `uv`, CMake, Ninja, and the
Xcode command-line tools. `uv` selects the declared Python version and installs
the exact conversion dependencies. The command stores its work under
`${XDG_CACHE_HOME:-$HOME/.cache}/seek/search-assets-upgrade`. Downloads retry and
resume. A later run reuses completed paths and replaces tracked files only when
their bytes differ. The command calculates and prints byte counts and SHA-256
values from the files that it receives or builds. It has no preset remote byte
counts or checksums.

Review the resource diff and commit it. Normal builds, tests, packages, and
releases only use the committed files. They never call the update target and
never download model or runtime resources.

## License

[Apache-2.0](LICENSE). See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for
the resources included in the executable.
