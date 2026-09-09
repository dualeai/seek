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
- **Optional code re-ranking** -- release binaries can re-rank a small candidate
  set with an embedded 17M-parameter code model

## Install

```bash
curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh
```

Or with Go:

```bash
go install github.com/dualeai/seek/cmd/seek@latest
```

The pre-built archives include the code re-ranker. A normal source build on a
supported target also includes it. A build with `CGO_ENABLED=0`, or a build for
another target, keeps the BM25 fallback without the model or runtime.

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

The re-ranker does not need a model download, an ONNX Runtime install, or an ML
service. The executable contains those resources. Release archives support
macOS 15 or newer on amd64 and arm64, and glibc-based Linux on amd64 and arm64.
The Linux archives are built on Ubuntu 24.04. Alpine and other musl systems are
not supported. The re-ranker adds no system dependency. Seek still needs
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
codex plugin marketplace upgrade seek
```

Start a new Codex session after each install or update. Open `/hooks` and trust
`seek-router` once. Claude Code has no equivalent step.

The package does not use an Agent Plugins 1.0 root manifest because that
standard does not define portable hooks. See the
[compatibility note](plugins/seek-router/README.md#why-there-is-no-agent-plugins-manifest).

The router requires `seek`, `jq`, and a POSIX `awk`. Building or updating a seek
index also requires Universal Ctags on `PATH` or through `CTAGS_COMMAND`. The
router is implemented inside the plugin and calls only the public seek CLI.
Seek does not contain hook parsing or command adapters. If a hook dependency is
missing, the hook leaves the original command unchanged.

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

Path operands constrain what Seek indexes. Query filters such as `file:api` and
`-file:test` filter search results after indexing. They do not reduce index
limits.

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

More [query syntax](https://github.com/sourcegraph/zoekt/blob/a0f5789d25cb/doc/query_syntax.md)
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
| `seek --rerank "find request parser"` | Re-rank a plain multi-word search with the English-to-code model |

Flags compose with query filters and paths. For example,
`seek -n 3 "sym:handleRequest file:api" ./src` returns the top 3 matching files
under `./src` containing a `handleRequest` definition under paths matching
`api`.

### Optional code re-ranking

Release binaries can improve a plain code search with the bundled
LateOn-Code-edge model:

```bash
seek --rerank 'find request parser' ./cmd
```

Re-ranking is off by default. It applies only to a plain query with two or more
words. Use a short English description because the model was trained for
English-to-code retrieval. A query with a filter, regular expression, Boolean
operator, negation, or one word keeps the normal BM25 path.

For an eligible query, Seek scores at most 20 files drawn from matches for any
query word. It scores their best matched snippets with the code model and
combines that order with the relaxed BM25 order by reciprocal rank fusion. If
fewer than two candidate files exist, or if collection, model setup, or
inference fails, Seek returns the strict BM25 results. A build without the
bundled backend prints a warning. Add `--verbose` to see other fallback
messages.

The Seek executable never accesses the network. When the runtime is absent, the
next eligible query expands the embedded ONNX Runtime into the private Seek
cache. Later queries reuse that checked file. The backend uses ONNX Runtime on
the CPU. It does not enable a GPU, NPU, FPGA, Core ML, or OpenVINO provider in
this version.

The frozen validation used 1,133 queries from 57 held-out Semble repositories.
Of 854 eligible queries, the OR candidate pass reached 0.873 Recall@20. The
combined rank reached 0.628 NDCG@10, compared with 0.572 for OR alone, for a
gain of 0.055. Its MRR@10 was 0.579, compared with 0.517 for OR alone. It
improved 313 eligible queries, left 463 equal, and made 78 worse (9.1%). Of
those 78, 25 lost at least 0.25 NDCG@10. On an Apple M5 Pro, a cold query took
919 ms and 475 warm queries had a 313 ms p95. The release archive was 32.708 MiB
and the highest measured RSS was 230.594 MiB. These values describe this fixed
local test, not a performance comparison or a result for all computers.
CodSpeed is the source for performance comparisons.

## What seek adds over ripgrep

[ripgrep](https://github.com/BurntSushi/ripgrep) is excellent for one-off text
search. seek adds the parts agents usually need when they search repeatedly:

| | ripgrep | seek |
|---|---|---|
| **Search model** | Scans files per query | Builds and reuses an index |
| **Relevance ranking** | Results in file-path order | Best matches first |
| **Definitions** | Text matches only | Tags such as `[func]` and `[class]` |
| **Context lines** | None by default | 3 lines around each match |
| **Local changes** | No local-change label | Includes and labels local changes |
| **Language detection** | Extension-based `--type` | Labels files via [go-enry](https://github.com/go-enry/go-enry) |
| **Parallel agents** | Each command scans | Agents share one index safely |

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

Seek does not reduce index quality to save memory. It sends every supported
file up to 100 MiB through the same content and symbol-analysis path. A file
larger than one normal shard still gets full content and ctags analysis. Seek
only limits large shard and ctags jobs to three at a time across active
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

Indexes are stored centrally in the user cache, never inside searched folders:

- macOS: `~/Library/Caches/seek/corpora/<id>/`
- Linux: `${XDG_CACHE_HOME:-$HOME/.cache}/seek/corpora/<id>/`
- Index files live in `index/`; `.state`, `.head`, `.git-committed-v1`, and
  `.lock` live next to it.

The re-ranker stores only its extracted runtime under
`<seek-cache>/reranker/1.29.0/<sha256>/`. The model and tokenizer stay in the
executable.

Folder searches read regular files and skip `.git` folders. They do not skip
dependency, build, cache, or vendor folders by name. Git ignore rules apply only
inside Git repos. Files larger than 100 MiB are skipped, and folder scans stop
at 1,000,000 candidate files or 10 GiB of indexed bytes.

Git applies the 10,000,000-file and 10 GiB work limits separately to its
committed and working-tree index families. The committed family counts all
candidate blobs. Its byte total includes candidate blobs at or below the
100 MiB document limit. It calculates both totals before
`.sourcegraph/ignore` filtering. The working-tree family counts selected
regular-file content. If the full repository exceeds a limit, a scoped search
can build a combined fallback for its selected paths. An unscoped search
reports the limit error.

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

### Benchmarks

Pre-cutover field benchmarks, generated on Apple M1 Max / macOS with
`./cicd/bench-field.sh --keep` on 2026-06-21. The Git rows use the former
go-git committed reader; the folder rows are not part of that reader change:

| Kind | Workload | Files | Cold index | Warm search | Dirty 1% | Dirty 10% |
|------|----------|-------|------------|-------------|----------|-----------|
| git | spf13/cobra | 66 | 1.1s | 210ms | 260ms | 270ms |
| git | prometheus/prometheus | 1,635 | 2.8s | 250ms | 360ms | 680ms |
| git | kubernetes/kubernetes | 30,507 | 24.8s | 1.4s | 2.1s | 8.8s |
| git | torvalds/linux | 94,541 | 231.6s | 2.4s | 9.8s | 85.7s |
| folder | synthetic-10k | 10,000 | 18.1s | 190ms | 410ms | 1.8s |
| folder | synthetic-100k | 100,000 | 81.6s | 650ms | 2.7s | 17.3s |

These local values are diagnostic. CodSpeed is the source for performance
comparisons. Each field workload has one sample, with about 10-20% run-to-run
variance.

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

Go 1.27 or newer is required. A normal build on a supported target also needs a
native C compiler: the Xcode command-line tools on macOS or GCC on glibc-based
Linux. Use `CGO_ENABLED=0 make build` only when you need the BM25 fallback build.

### Release packaging

`make package` builds Seek and creates the archive for the current native
target. Each archive contains one `seek` executable. The release workflow runs
this target on macOS and Linux, on amd64 and arm64. It does not cross-build.
You can run the target again to replace its output.

The release workflow downloads the four archives, generates the standard
CycloneDX SBOM, computes `checksums.txt`, and uploads these files. Artifact and
GitHub release uploads replace files with the same names, so a failed workflow
can run again.

`RELEASE_TAG=vX.Y.Z make release` repeats only the final upload after the four
archives and `sbom.cyclonedx.json` exist. It needs the GitHub CLI, `gh`. The
named GitHub release must already exist, and `gh` must have permission to upload
to it.

### Re-ranker resource update

`make rerank-assets-upgrade` is a manual maintainer command. It downloads the
model, tokenizer, and three official ONNX Runtime packages at fixed revisions.
Microsoft does not publish an ONNX Runtime 1.29.0 macOS amd64 package, so the
command builds that one library from the fixed source commit when its versioned
cache entry is absent.

The command needs macOS, `curl`, `zstd`, CMake, Ninja, Python 3.10 or newer, and
the Xcode command-line tools. It stores downloads, source, and build work under
`${XDG_CACHE_HOME:-$HOME/.cache}/seek/rerank-assets-upgrade`. Downloads retry and
resume. A later run reuses completed paths and replaces tracked files only when
their bytes differ. The command calculates and prints byte counts and SHA-256
values from the files that it receives or builds. It has no preset remote byte
counts or checksums.

Review the resource diff and commit it. Normal builds, tests, packages, and
releases only use the committed files. They never call the update target and
never download model or runtime resources.

## License

[Apache-2.0](LICENSE)
