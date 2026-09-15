# seek

Ranked local search for AI coding agents. `seek` searches the current Git repo
or the paths you pass, then returns relevant matches with definitions and
context. Plain descriptions with two or more words can use a bundled local model
to find and rank code by meaning. No server or API key is needed.

Seek caches indexes for fast repeat searches and safe concurrent use. Run it as
a tool call or shell command.

<!-- Status -->
[![CI](https://github.com/dualeai/seek/actions/workflows/ci.yml/badge.svg)](https://github.com/dualeai/seek/actions/workflows/ci.yml)
[![CodSpeed](https://img.shields.io/endpoint?url=https://codspeed.io/badge.json)](https://codspeed.io/dualeai/seek?utm_source=badge)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

## Install

```bash
curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh
```

The script installs the latest release in `~/.local/bin`. If that directory is
not on `PATH`, the script prints the command to add it. Run `seek --version` to
check the install. You can also download a binary from
[GitHub Releases](https://github.com/dualeai/seek/releases).

## Requirements

[Universal Ctags](https://github.com/universal-ctags/ctags) is required to build
or update an index and to support `sym:` definition search. Put it on `PATH` or
set `CTAGS_COMMAND` to its executable:

```bash
brew install universal-ctags          # macOS
sudo apt-get install universal-ctags  # Linux
```

Git 2.36 or later is required for Git-backed searches and `git worktree`
setups.

The executable includes the search model and its runtime. Release archives
support macOS 15 or newer and glibc-based Linux on amd64 and arm64. Alpine and
other musl systems are not supported.

## Quick Start

```bash
cd your-project

seek 'handleRequest'                 # current repo
seek 'sym:handleRequest'             # definition, not calls
seek 'handleRequest' ./src ./cmd     # selected paths
seek 'TODO' ../notes                 # folder outside Git
seek 'needle' ./src/server.go        # exact file
seek 'request authentication flow'   # plain description of this repo
seek --lexical-only 'handleRequest'  # text search only; skip all model work
```

The first search builds local indexes. On a large repository, it can take tens
of seconds or longer, use several GiB of memory, and create about 1 GiB of cache
data. Use `--lexical-only` for a faster text-only search with no model work.

A search returns output like this:

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

## Ranked, not exhaustive

Seek ranks results, but it does not guarantee every match. A descriptive search
considers at most 128 files. Each text-search pass is limited to 10,000 matches
and 60 seconds. With `-n 0`, Seek displays every file returned within these
bounds. With `-m 0`, it displays every returned match in each file. For every
occurrence, a count, a rename, or an absence check, use an exhaustive tool:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

The `SEEK_ROUTER=off` prefix matters only when the router plugin is installed.

## Usage

```bash
seek [flags] '<query>' [path...]
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
seek -n 5 -m 3 'handleRequest' ./src
```

Paths must exist. Symlinks passed on the command line are resolved to their
targets. Broken symlinks and invalid paths exit with code 2. Symlinks found
while walking folders are skipped.

Paths limit the search results. Selected paths can also use a smaller index when
a complete Git repository exceeds an index limit. Query filters such as
`file:api` and `-file:test` apply after indexing, so they do not reduce index
limits.

## Query Syntax

### Search

| Query | What it does |
|-------|-------------|
| `seek 'CoreRouter'` | Substring search across content and file names |
| `seek 'content:"async def.*handler"'` | Search file content, not file names |
| `seek 'regex:foo.*bar'` | Explicit regex search |

### Symbols

| Query | What it does |
|-------|-------------|
| `seek 'sym:CoreRouter'` | Find function, class, method, and type definitions |

### Filters

| Query | What it does |
|-------|-------------|
| `seek 'file:router/src'` | Filter results to paths matching `router/src` |
| `seek 'lang:python error'` | Filter by language |
| `seek 'case:yes FooBar'` | Case-sensitive search (`yes`, `no`, `auto`) |
| `seek 'type:file config'` | Return file names without content matches |

### Boolean Logic

| Query | What it does |
|-------|-------------|
| `seek '-file:test'` | Exclude paths matching `test` |
| `seek 'foo or bar'` | Match either term |
| `seek '(foo or bar) lang:go'` | Group expressions with parentheses |
| `seek 'handleError file:api -file:test'` | Combine content and path filters |

Seek also supports the [query syntax](https://github.com/sourcegraph/zoekt/blob/df97bab6f7bb/doc/query_syntax.md)
of its pinned Zoekt search engine. Results are ranked by relevance.

### Flags

| Flag | What it does |
|------|-------------|
| `seek -n 5 'query'` | Display at most 5 files (`--limit`) |
| `seek -m 3 'query'` | Display at most 3 matches per file (`--max-matches`) |
| `seek -A 5 'query'` | Show 5 lines after each match (`--after-context`) |
| `seek -C 5 'query'` | Show 5 lines on both sides (`--context`) |
| `seek -n 5 -m 3 'query'` | Top 5 files, max 3 matches each |
| `seek -v 'query'` | Show debug logs and detailed errors (`--verbose`) |
| `seek --lexical-only 'query'` | Skip meaning-based indexing, search, and model ranking |

`-n 0` and `-m 0` display all results returned within the search bounds above.
They do not make search exhaustive. `-A` and `-C` accept values from 0 to 512
and cannot be used together.

Flags combine with filters and paths. For example,
`seek -n 3 'sym:handleRequest file:api' ./src` returns up to 3 files under
`./src` with `api` in the path that define `handleRequest`.

### Descriptive search

Use a plain description when you know the behavior but not its name or
location. Seek uses a bundled local model; it needs no download or service.

```bash
seek 'find request parser'
seek -n 5 -m 2 -C 1 'validate search query syntax'
seek 'find request parser lang:go -file:_test\.go$'
```

A plain query with two or more words can use model ranking. It can include
`lang:`, `file:`, and `-file:` filters. Other filters, Boolean expressions,
quoted phrases, exact identifiers, and one-word queries use text-match order.

When you search one clean Git worktree, one file, or one folder, Seek can also
find code with a similar meaning. A selected Git subpath, a Git worktree with
local changes, or multiple sources can still use the model to rank text matches.
Seek keeps `lang:`, `file:`, and `-file:` filters active if it uses another
search method.

Seek considers at most 128 files for a description. This bound is separate from
the `-n` display limit. Meaning-based search is approximate, so `-n 0` cannot
make it exhaustive.

When no file contains every query word, Seek returns results that omit some
query words only if the best model score is strong enough. Otherwise, it prints
nothing and exits with code 1. This does not prove that the code is absent. A
description that is too long for the model uses text results only.

If meaning-based search is unavailable, Seek can still use the model to rank text
matches. If the model fails, Seek returns results that contain every query
word. Use `--verbose` to see why it used another search method.

Unless `--lexical-only` is set, Seek keeps both its text and meaning-based
indexes up to date. This includes exact queries that use text ranking. Use
`--lexical-only` to skip the meaning-based index and all model work.

Seek selects the model run-time provider for the host. Set `SEEK_PROVIDER` to
pin one when you report a problem, so the report names one provider: `cpu` runs
the model on the CPU, `coreml-mlprogram-static` asks for Apple Core ML, and
`auto` or an unset value keeps the automatic choice. A name this build cannot
use stops a search with exit code 2; it does not stop `--lexical-only`, `seek gc`
or other commands that never load the model. A pinned provider that fails to start
falls back to text search, and `--verbose` gives the reason. `--verbose` also
prints the provider Seek selected.

The automatic choice compares Core ML with the CPU provider once for each model,
run-time version and operating system build, then keeps the result beside the
compiled model. That comparison runs the model twice, so it adds a
noticeable pause to the first search under each of those combinations, and to the
first search after a system update — about half a second on a recent Apple laptop,
longer on slower hardware. Later searches read the stored result. This finds a provider that reports success but returns wrong
numbers, such as
[ONNX Runtime issue 32569](https://github.com/microsoft/onnxruntime/issues/32569)
on macOS 15 ARM64.

### Shell completion

Run `seek completion <shell> --help` to set up Bash, Zsh, fish, or PowerShell.

## Agent integration

The `seek-router` plugin teaches an agent the query syntax and rewrites
supported static `grep`, `rg`, `git grep`, `fd`, and `find` commands.

For Claude Code:

```sh
claude plugin marketplace add dualeai/seek
claude plugin install seek-router@seek --scope user
```

For OpenAI Codex:

```sh
codex plugin marketplace add dualeai/seek
codex plugin add seek-router@seek
```

Update an installed plugin with the matching command:

```sh
claude plugin update seek-router@seek
codex plugin marketplace upgrade seek
```

Start a new session after an install or update. In Codex, open `/hooks` and
trust `seek-router` when requested. The router requires `seek`, `jq`, and a
POSIX `awk`. If a dependency or command form is not supported, it leaves the
command unchanged.

Routed searches display at most 20 files and three matches per file. Use
`SEEK_ROUTER=off` with the original command when you need every match. See the
[router guide](plugins/seek-router/README.md) for supported command forms,
requirements, and host checks.

Without the plugin, add this short rule to the agent instruction file:

```text
Use `seek` for ranked code navigation. Put filters in one quoted query and
paths after it: seek 'sym:Name file:src -file:test' ./cmd
Use a plain description with two or more words when the name is unknown.
Use an exhaustive tool for renames, counts, and absence checks.
```

## What seek adds over ripgrep

[ripgrep](https://github.com/BurntSushi/ripgrep) is excellent for one-off text
search. seek adds the parts agents usually need when they search repeatedly:

| | ripgrep | seek |
|---|---|---|
| **Search method** | Scans files for each command | Builds, then reuses a local index |
| **Default directory scope** | Current directory | Current Git worktree; outside Git, pass a path |
| **Default order** | Unspecified; `--sort path` gives stable path order | Relevance for exact queries; local model ranking for eligible descriptions |
| **Plain descriptions** | Literal (`-F`) or regular expression | Uses the bundled model and can find code with a similar meaning |
| **Symbols** | No symbol index | `sym:` finds definitions and tags symbols |
| **Context** | None by default; use `-A`, `-B`, or `-C` | Up to 3 lines on each side by default |
| **Local changes** | Current file contents; no status label | Committed and working-tree content; changed results get `[uncommitted]` |
| **Languages** | Built-in and custom file-type globs | Automatic detection and `lang:` filters |
| **Concurrent use** | Stateless | Agents share locked indexes |

Use ripgrep for all matching lines, counts, multiline regular expressions,
optional PCRE2 features, or replacement output. Replacement changes output,
not files. Use seek for ranked navigation, definitions, compact context, or a
description of the code.

## Storage and limits

Seek stores indexes in the user cache, never in searched folders. Set
`SEEK_CACHE_DIR` to select another cache root. The default search-index paths
are:

- macOS: `~/Library/Caches/seek/corpora/<id>/`
- Linux: `${XDG_CACHE_HOME:-$HOME/.cache}/seek/corpora/<id>/`

Git searches include committed files and local changes. Folder searches read
regular files, skip `.git` directories, and do not skip dependency, build,
cache, or vendor directories by name. Git ignore rules apply only in Git
repositories.

Seek skips files larger than 100 MiB. Folder indexing stops at 1,000,000 files
or 10 GiB. Git applies limits of 10,000,000 files and 10 GiB separately to
committed files and local changes. A scoped search can still index its selected
paths when the complete repository exceeds a limit.

The meaning-based index skips empty files, files with a NUL byte, and generated
files with the standard `// Code generated ... DO NOT EDIT.` marker in the first
4 KiB. These files can still appear in text results.

An upgrade can rebuild meaning-based data and can briefly need space for both
the old and new copies. Do not alternate old and new Seek binaries with one
cache. Use `--lexical-only` when you do not want this rebuild.

## Cache maintenance

Seek automatically removes search-index caches that have not been used for 14
days. It checks at most once per 24 hours. Automatic cleanup is disabled on a
network filesystem. Set `SEEK_CACHE_DIR` to a local directory to enable it, or
use `XDG_CACHE_HOME` on Linux. Cleanup has no total-size limit.

Environment settings:

- `SEEK_GC_MAX_AGE` -- maximum unused age (default `14d`)
- `SEEK_GC_INTERVAL` -- delay between automatic checks (default `24h`)

Manual control:

```sh
seek gc --dry-run --sort=size   # show the largest search indexes
seek gc --force                 # run cleanup now
seek gc --all                   # delete every search index not in use
```

`--sort` orders the table by `name` (default), `age` (oldest first), or
`size` (largest first).

`seek gc` manages only `<seek-cache>/corpora/`. It does not remove rebuildable
model files in `<seek-cache>/reranker/` or `<seek-cache>/semantic/usearch/`.
Remove those directories only when no `seek` process is running.

## Parallel use

When multiple `seek` commands search the same repo at the same time:

| Scenario | Behavior |
|----------|----------|
| Index is fresh | All commands search at the same time |
| Update active | One command builds while others search the current index. Searches wait up to 10 seconds while the new index replaces the old one, then warn and use the available index |
| An update fails and an old index exists | Seek warns and uses the old index. File and byte limit errors still stop the search |
| No index yet | First command builds it; others wait up to 60s |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success (one or more matches) |
| 1 | No accepted result: either search found no match or model-added results were too weak. This does not prove absence |
| 2 | Error (usage error, invalid query or setting, indexing failed) |

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

## License

[Apache-2.0](LICENSE). See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for
the resources included in the executable.
