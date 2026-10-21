# seek

Ranked local search for AI coding agents. `seek` searches the current folder
or the paths you pass, then returns relevant matches with definitions and
context. Plain descriptions with two or more words can use a bundled local model
to find and rank code by meaning. No server or API key is needed.

Seek caches indexes for fast repeat searches and safe concurrent use. Run it as
a tool call or shell command.

Seek is built for the shortest search time, not for low resource use. It uses
all CPU cores and as much memory and cache space as a search needs.

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
setups. Seek never fetches from a remote: in a partial clone, files whose
content is not on disk are searchable by name only, and the first search after
their content arrives indexes it. A partial clone needs Git
2.45 or later, or a 2024 maintenance release that blocks lazy fetches (2.39.4,
2.40.2, 2.41.1, 2.42.2, 2.43.4, or 2.44.1).

The executable includes the search model and its runtime. Release archives
support macOS 15 or newer and glibc-based Linux on amd64 and arm64. Alpine and
other musl systems are not supported.

## Quick Start

```bash
cd your-project

seek 'handleRequest'                 # current folder, the same as .
seek 'sym:handleRequest'             # definition, not calls
seek 'handleRequest' ./src ./cmd     # selected paths
seek 'TODO' ../notes                 # folder outside Git
seek 'needle' ./src/server.go        # exact file
seek 'request authentication flow'   # plain description of this repo
seek --lexical-only 'handleRequest'  # text search only; skip all model work
```

The first search builds local indexes. On a large repository, it can take tens
of seconds or longer, use several GiB of memory, and create several GiB of cache
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
agents and CI get clean results. A file header is the path as you typed it plus
the path below it, the rule of ripgrep and `git grep`: with no path or `.`, the
path below the current folder. Across several folders or repos, each header
also has a `[git]` or `[folder]` tag.

Set `NO_COLOR` to a non-empty value to disable color. Set `CLICOLOR_FORCE` to
force color through a pipe. `NO_COLOR` takes precedence when both are set.

## Ranked, not exhaustive

Seek ranks results, but it does not guarantee every match. Meaning-based
retrieval is approximate. Text search counts every match. With `-n 0`, Seek
displays every returned file. With `-m 0`, it displays every returned match in
each file. For every
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

- No paths: search the current folder, the same as `.`. Inside a Git repo, the
  search covers that folder and the folders below it.
- Folders inside Git repos: use Git ignore and include local changes.
- Exact files: search only that file, not sibling files, even when the file is
  inside a Git repo.
- Folders outside Git: search that folder with filesystem rules.
- Visible nested Git worktrees inside selected folders are searched once with
  their own Git rules.
- Files or folders ignored by Git stay ignored when you search that repo or a
  folder inside it. Passing an exact ignored file or folder still searches it,
  and so does a search from inside an ignored folder.

Flags must come before the query:

```bash
seek -n 5 -m 3 'handleRequest' ./src
```

Paths must exist. Symlinks passed on the command line are resolved to their
targets. Broken symlinks and invalid paths exit with code 2. Symlinks found
while walking folders are skipped.

Paths limit the search results. A selected path inside a Git repository uses
the index of the whole repository. Query filters such as `file:api` and
`-file:test` match the header path, which starts with the path you typed:
`seek 'file:^src/' proj` finds nothing, and `seek 'file:^proj/src/' proj` finds
`proj/src/x.go`. They apply after indexing, so they do not change what Seek
indexes. A `file:` filter adds rank only where it also matches the file's path
inside its repository or folder. A plain word also matches that path, so from
`staging/src/k8s.io/api` the word `staging` matches every file.

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

Seek also supports the [query syntax](https://github.com/sourcegraph/zoekt/blob/0e68f41329d9/doc/query_syntax.md)
of its pinned Zoekt search engine. Results are ranked by relevance.

### Support table

Each row is one query form: its role in a descriptive search and what Seek
does with it. A description atom is text that the model reads; a filter
applies in lexical and semantic retrieval; a modifier changes neither. A Zoekt
pass-through is an atom that Seek sends to Zoekt with no meaning of its own.
The test `TestQuerySupportReadmeMatchesTable` keeps this table equal to the one
in the code.

| ID | Form | Role | What Seek does | Example |
|----|------|------|----------------|---------|
| T01 | bare word | description | Matches the file name or the content. | `parse` |
| T02 | bare regular expression token | description | The model reads the token text; the lexical half keeps the regular expression. | `parsed?` |
| T03 | regex: | description | The same atom as a bare token. | `regex:request` |
| T04 | quoted phrase | description | The model reads the phrase; the lexical half keeps Zoekt's meaning, a regular expression of the quoted text. | `"request line"` |
| T05 | quoted word | description | The same atom as the bare word. | `"request"` |
| T06 | content: (c:) | filter | Content only, applied by Zoekt. | `content:method` |
| T07 | -content: (-c:) | filter | Content without the term, applied by Zoekt. | `-content:malformed` |
| T08 | file: (f:) | filter | Path filter; the semantic mask applies it too. | `file:server` |
| T09 | -file: (-f:) | filter | Negated path filter; the semantic mask applies it too. | `-file:_test.go` |
| T10 | lang: | filter | Language filter; the semantic mask applies it too. | `lang:go` |
| T11 | lang: with an unknown language | filter | Zoekt pass-through: matches nothing. | `lang:nosuchlanguage` |
| T12 | -lang: | filter | Files in other languages, applied by Zoekt. | `-lang:go` |
| T13 | sym: | filter | Files with a matching ctags symbol, applied by Zoekt. | `sym:ParseRequest` |
| T14 | -sym: | filter | Files without the symbol, applied by Zoekt. | `-sym:ParseRequest` |
| T15 | case:yes, case:no, case:auto | modifier | Parse directive: sets the case sensitivity of the atoms and leaves the tree. A negated -case: makes Zoekt's per-shard searcher panic (an upstream bug). | `case:yes` |
| T16 | smart case (a capital letter) | description | A description word that matches its case. | `Parse` |
| T17 | type:file, type:filename (t:file, t:filename) | modifier | Output mode: the search prints file names only. With a negated -type:, Zoekt fails the search of each node; Seek warns and skips the node. | `type:file` |
| T18 | type:filematch (t:filematch) | filter | Zoekt pass-through; Zoekt's per-shard searcher panics on it (an upstream bug). | `type:filematch` |
| T19 | type:repo (t:repo) | filter | Zoekt pass-through; Zoekt's per-shard searcher panics on it (an upstream bug). | `type:repo` |
| T20 | repo: (r:) | filter | Zoekt pass-through on Seek's internal repository names. | `repo:nomatch` |
| T21 | -repo: (-r:) | filter | Zoekt pass-through. | `-repo:nomatch` |
| T22 | branch: (b:) | filter | Zoekt pass-through: committed Git files carry the branch HEAD. | `branch:HEAD` |
| T23 | archived:, fork:, public: | filter | Zoekt pass-through on repository flags. | `archived:no` |
| T24 | meta.<field>: | filter | Zoekt pass-through: Seek sets no repository metadata. | `meta.owner:x` |
| T25 | or between filters | filter | Either filter. An or binds the whole query unless it is in parentheses; an or between descriptions keeps strict ranking. | `(file:server or file:tools)` |
| T26 | and, not, OR | description | Plain words, not operators: only a lowercase or is an operator. | `and` |
| T27 | -word | filter | Files without the term, applied by Zoekt. | `-malformed` |
| T28 | parentheses | modifier | Grouping; Zoekt flattens it. | `(alpha beta)` |
| T29 | -( ... ) | filter | Files that match none of the group, applied by Zoekt. | `-(file:test or lang:python)` |
| T30 | backslash escape | description | A literal character in a word. | `foo\.bar` |
| T31 | C++, X[], X(), a::b() programming names | description | When Zoekt rejects the query, Seek quotes each programming name and parses the query again; filters keep their meaning, and the model reads the name. | `C++` |
| T32 | unknown prefix (l:go, ext:go) | description | Not a Zoekt prefix: a plain word. | `l:go` |
| T33 | API-only nodes (FileNameSet) | filter | Seek's path-operand scope; the parser makes no such node. |  |
| T34 | one description atom | description | Strict ranking: the model routes need two or more description atoms. |  |
| T35 | two or more identifiers | description | Description atoms like any words. |  |
| T36 | no description atom | filter | Strict ranking: filters alone give the model nothing to read. |  |
| T37 | more than 125 terms | modifier | A long query keeps every token in 128-token query windows. |  |
| T38 | --lexical-only | modifier | No model work for any query. |  |
| T39 | path-operand scope | filter | The scope is one more filter subtree: Zoekt keeps the files in scope, and the mask keeps their rows for semantic retrieval. |  |
| T40 | two or more nodes | modifier | Each node runs the same pipeline with its own mask; the results do not depend on the node count. |  |
| T41 | a token with no letter or digit | modifier | Zoekt keeps it in both lexical queries; the model reads nothing from it and the route does not change. | `.` |

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

`-n 0` and `-m 0` display all returned results. They do not make a descriptive
search exhaustive. `-A` and `-C` accept any value of 0 or more and cannot be
used together.

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

A plain query with two or more description words uses the model. Every
searched folder and repository, clean or with local changes, scoped or not,
adds files whose code has a similar meaning, and the model ranks them with the
text matches. Filters (`lang:`, `file:`, `-file:`, `sym:`, `content:`, and the
path operands) apply to both. A one-word query, a lone quoted phrase, an `or`
between descriptions, and a query of filters alone use text-match order. The
[Support table](#support-table) lists each query form.

Every file that has a unit within the similarity radius of the description is
a candidate; there is no count bound, and `-n` limits only the display.
Meaning-based search is approximate, so `-n 0` cannot make it exhaustive. A
long description keeps every word, in 128-token windows.

Files that contain every query word stay. A file that only the model adds must
pass the model's score threshold. With no result, Seek prints nothing and exits
with code 1. This does not prove that the code is absent. If the model fails,
Seek returns the text matches; use `--verbose` to see why.

The first description search builds the meaning-based data of every searched
folder and repository before it answers, so a first search and a repeat search
give the same results. In a Git repository, files with local changes or
untracked files go to a small overlay that holds their current bytes; the
unchanged part of the repository is never embedded again. An exact word, a
symbol lookup, or a phrase builds no meaning-based data. Use `--lexical-only` to
skip the meaning-based index and all model work for every query.

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
| **Default directory scope** | Current directory | Current folder, the same as `.`, with each Git repository in it searched by Git's rules |
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

A Seek upgrade that changes the bundled model or its run time makes the stored
meaning-based indexes unusable, so the first search after such an upgrade rebuilds
them. It repeats only the meaning-based half of the first search, reading the
files again and running the model over them; the text index stays valid and is
not rebuilt. Text search works throughout.

Git searches include committed files and local changes. Folder searches read
regular files, skip `.git` directories, and do not skip dependency, build,
cache, or vendor directories by name. Git ignore rules apply only in Git
repositories.

Seek has no file-count limit and no byte limit for a search. The only limit on
the index cache is its budget (see "Cache maintenance"); the only limit on the
text of one file is the index format (below).

To finish sooner, the text index reads file content in parallel up to a quarter
of the host's physical memory at a time, and builds files that fill an index
shard alone on all CPU cores at once.

Text search covers the full content of every text file, however large it is and
however many different character sequences it holds. The only limit for one
file is the index format: one Zoekt index shard holds at most 4 GiB. A file
whose shard would be larger keeps only its name in the text index. Two more
kinds of files keep only their name, because Zoekt has no option to index their
content: files with a NUL byte, and files shorter than three bytes. Files whose
bytes Seek does not have keep only their name too: cloud-only files that are not
on disk, blobs that a partial clone lacks, and files that cannot be read. A
search can match these names, not their content. When a search could have
matched such files by content, it prints one line after the results that counts
them (Zoekt's own files of under three bytes are not counted). Zoekt stores a
short `NOT-INDEXED:` marker as their text, so a search for its words lists
them. Large files with much non-ASCII text, such
as CJK prose, need much memory to index: 100 MiB of random CJK characters needs
about 6.6 GiB.

When a search of a file path operand finds no content match and the index holds
no content for that file, Seek writes a warning with the reason to standard
error. Searches of folders and repositories do not write this warning.

The meaning-based index skips empty files and files with a NUL byte. Generated
files are in both indexes.

An upgrade can rebuild meaning-based data and can briefly need space for both
the old and new copies. Do not alternate old and new Seek binaries with one
cache. Use `--lexical-only` when you do not want this rebuild.

## Cache maintenance

The search-index cache has one byte budget: 10% of the cache volume, or less so
that 5% of the volume stays free. Before any index build, Seek estimates the
cache that the search needs. If it does not fit, the search stops with exit
code 2 and names the planned size and the budget. After a search, Seek removes
the least recently used indexes that the search did not use until the cache
fits. Age alone never removes an index. Compiled model files that no search
used for 14 days are removed too; the check runs at most once per 24 hours.
Automatic cleanup is disabled on a network filesystem. Set `SEEK_CACHE_DIR` to a
local directory to enable it, or use `XDG_CACHE_HOME` on Linux. The cache
directory must support `flock(2)` locks: a lock error other than a held lock
stops the search of that index with an error that names the lock file.

Environment settings:

- `SEEK_CACHE_DIR` -- cache root (default: the user cache directory)
- `SEEK_CACHE_BUDGET` -- index cache budget, for example `20G` (default: see
  above)
- `SEEK_GC_INTERVAL` -- delay between automatic checks (default `24h`)
- `SEEK_PROVIDER` -- pin the model run-time provider; see "Descriptive search"

Manual control:

```sh
seek gc --dry-run --sort=size   # show the largest search indexes
seek gc --force                 # run cleanup now
seek gc --all                   # delete every search index not in use
```

`--sort` orders the table by `name` (default), `age` (oldest first), or
`size` (largest first).

`seek gc` manages `<seek-cache>/corpora/`, and removes compiled model files under
`<seek-cache>/reranker/coreml/` that no search has used for 14 days. Seek compiles those again when it needs them, and checks the provider again,
because the check result is stored beside the compiled model. It does not remove the
extracted run time in `<seek-cache>/reranker/<version>/` or the search library in
`<seek-cache>/semantic/usearch/`. Remove those directories by hand only when no
`seek` process is running.

## Parallel use

When multiple `seek` commands search the same repo at the same time:

| Scenario | Behavior |
|----------|----------|
| Index is fresh | All commands search at the same time |
| Update active | One command builds while others search the current index. A search waits while the new index replaces the old one, with no deadline; Ctrl-C ends the wait |
| An update fails and an old index exists | Seek warns and uses the old index. A search that does not fit the cache budget stops before any build |
| No index yet | First command builds it; the others wait for that build |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success (one or more matches) |
| 1 | No accepted result: either search found no match or model-added results were too weak. This does not prove absence |
| 2 | Error (usage error, invalid query or setting, indexing failed) |

Uses the same exit-code pattern as `grep` and `ripgrep`, so `seek` works well
in scripts.

## Security

A search does not trust a repository's own Git configuration or attributes: it
runs no filter, diff, or credential command that a repository sets. It writes
no repository configuration and starts no daemon. It uses no network, also in a
partial clone. Git's own index refresh, which updates `.git/index` stat data,
stays.

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
