# seek

Ranked local search for AI coding agents. `seek` searches the current Git repo
or the paths you pass, then returns relevant matches with definitions and
context. When you do not know the exact name, use `--rerank` with a plain
English description. Supported builds bundle the local code-search model; no
server or API key is needed.

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
```

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

When you know the behavior but not the name, use `--rerank`:

```bash
seek --rerank 'request authentication flow' ./src
```

## Highlights

- **Search what you point at** -- current repo by default; pass files,
  folders, selected paths, or another repo when you need a narrower search.
- **Best match first** -- ranked by relevance, not file-path order
- **Plain-English re-ranking** -- when you do not know the name, `--rerank`
  combines text rank with a bundled local model on supported builds
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

Pre-built archives and normal source builds on supported targets include the
re-ranker. `CGO_ENABLED=0` and other targets use BM25 without the model or
runtime.

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

The router does not add `--rerank`. Call `seek --rerank` directly when you want
to search from a plain English description of two or more words.

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
Use --rerank only for a plain description with two or more words and no filters:
seek --rerank 'request auth flow'
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
| `seek --rerank "find request parser"` | Re-rank an eligible plain query with the local model |

`-n` and `-m` use `0` by default, which adds no display limit. `-A` and `-C`
accept values from 0 to 512 and cannot be used together.

Flags combine with filters and paths. For example,
`seek -n 3 "sym:handleRequest file:api" ./src` returns up to 3 files under
`./src` with `api` in the path that define `handleRequest`.

### Optional code re-ranking

Use `--rerank` when you know the behavior but not its name or location. Seek
uses its bundled
[LateOn-Code-edge](https://huggingface.co/lightonai/LateOn-Code-edge/tree/4bcdf5ed93f791259eb130b577a240f753d68dd8)
model to move likely code toward the top.

```bash
seek --rerank 'find request parser' ./cmd
seek --rerank -n 5 -m 2 -C 1 'validate search query syntax' ./cmd
```

Re-ranking is off by default. It accepts only plain queries with two or more
words. Exact identifiers and phrases, `sym:` queries, filters, regular
expressions, Boolean operators, negation, and one-word queries stay on the BM25
path, even with `--rerank`.

Seek scores up to 20 candidate files that match at least one query word. The
model scores each path with its best nearby code. Seek combines the model and
BM25 orders with weighted reciprocal rank fusion; BM25 has twice the model
weight. This can find a useful file that lacks some query words. Normal
all-word BM25 matches remain, subject to the display limits.

**Ranking warning:** Re-ranking can lower result quality. Compare the query with
and without `--rerank` when order matters.

The `-n`, `-m`, `-C`, and `-A` flags still control displayed output. The model
can use nearby source context that these flags do not display.

The model runs locally and needs no download or service. Re-ranking uses more
time and memory than BM25. Its first use can be slower while Seek extracts the
embedded runtime; later searches reuse it. If re-ranking is unavailable or
fails, Seek returns the normal all-word BM25 results. If the build has no model,
`--rerank` prints a warning. Use `--verbose` for other fallback messages.

### Shell completion

Run `seek completion <shell> --help` to set up Bash, Zsh, fish, or PowerShell.

## What seek adds over ripgrep

[ripgrep](https://github.com/BurntSushi/ripgrep) is excellent for one-off text
search. seek adds the parts agents usually need when they search repeatedly:

| | ripgrep | seek |
|---|---|---|
| **Search method** | Scans files for each command | Builds, then reuses a local index |
| **Default directory scope** | Current directory | Current Git worktree; outside Git, pass a path |
| **Default order** | Unspecified; `--sort path` gives stable path order | BM25 relevance |
| **Plain descriptions** | Literal (`-F`) or regular expression | `--rerank` combines BM25 and a local model for plain multi-word queries |
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
3. **Update the index** -- Git repos keep committed files and local changes
   separate. Folders index regular files directly.
4. **Search** -- reads the index for every selected repo or folder, runs the
   normal query, merges duplicate results, and sorts by BM25 relevance.
5. **Optionally re-rank** -- for an eligible query, scores up to 20 relaxed
   candidates and combines the model and BM25 orders.
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
