# seek

Ranked local code search for AI coding agents. `seek` searches the current
folder or the paths you give, and returns the most relevant matches with their
definitions and context. A query also finds code by meaning, with a bundled
local model. No server or API key is needed.

Seek caches its indexes for fast repeat searches and safe concurrent use. It is
built for the shortest search time: it uses all CPU cores and as much memory and
cache space as a search needs.

[![CI](https://github.com/dualeai/seek/actions/workflows/ci.yml/badge.svg)](https://github.com/dualeai/seek/actions/workflows/ci.yml)
[![CodSpeed](https://img.shields.io/endpoint?url=https://codspeed.io/badge.json)](https://codspeed.io/dualeai/seek?utm_source=badge)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

## Install

```bash
curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh
```

The script installs the latest release in `~/.local/bin`. Binaries are also on
[GitHub Releases](https://github.com/dualeai/seek/releases), for macOS 15 or
newer and glibc-based Linux, on amd64 and arm64.

Seek needs:

- [Universal Ctags](https://github.com/universal-ctags/ctags) on `PATH` (or set
  `CTAGS_COMMAND`): `brew install universal-ctags` or
  `sudo apt-get install universal-ctags`.
- Git 2.36 or later for Git repositories. Seek never fetches from a remote: in a
  partial clone, a file whose content is not on disk is found by name only. A
  partial clone needs Git 2.45 or later, or a 2024 maintenance release that
  blocks lazy fetches.

The executable includes the model and its runtime.

## Quick start

```bash
seek 'handleRequest'                 # the current folder
seek 'sym:handleRequest'             # the definition, not the calls
seek 'handleRequest' ./src ./cmd     # selected paths
seek 'needle' ./src/server.go        # one file
seek 'request authentication flow'   # a description: finds code by meaning
seek --lexical-only 'handleRequest'  # text only, no model work
```

```text
## src/server.go (Go)
13 // handleRequest processes incoming HTTP requests.
14 // It validates auth and delegates to the appropriate handler.
15 [func] func handleRequest(w http.ResponseWriter, r *http.Request) {
16     ctx := r.Context()
17     validateAuth(ctx, r)
```

Results are grouped by file, most relevant first, with 3 lines of context on
each side. In `sym:` results, a tag such as `[func]` or `[class]` marks each
definition. `[uncommitted]` marks a file with local changes. Piped output has
no color. Set `NO_COLOR` to disable color, or `CLICOLOR_FORCE` to force it.

The first search of a project builds its indexes. On a large repository this
can take tens of seconds and several GiB of memory and cache.

## Usage

```bash
seek [flags] '<query>' [path...]
```

Flags come before the query, and paths after it. Put all filters inside the one
quoted query.

- With no path, Seek searches the current folder, the same as `.`.
- A Git repository, or a folder in one, follows Git's ignore rules and includes
  local changes. An ignored file or folder is searched only when you give it as
  a path.
- A folder outside Git is searched as it is on disk.
- A file path searches only that file.
- A path that is a symlink is resolved to its target. Symlinks found during a
  walk are skipped.

| Query | Meaning |
|-------|---------|
| `CoreRouter` | Text in file content or file names |
| `sym:CoreRouter` | Function, class, method, or type definitions |
| `content:"async def"` | Content only |
| `file:api`, `-file:test` | Keep or drop paths that match |
| `lang:python`, `-lang:go` | Keep or drop a language |
| `case:yes FooBar` | Case-sensitive (`yes`, `no`, `auto`) |
| `type:file config` | File names only |
| `foo or bar`, `(foo or bar) lang:go` | Either term; grouping |

`file:` matches the path as printed, which starts with the path you typed
(Seek prints `./src` as `src`).
Seek accepts the full [Zoekt query syntax](https://github.com/sourcegraph/zoekt/blob/0e68f41329d9/doc/query_syntax.md).

| Flag | Meaning |
|------|---------|
| `-n 5` | At most 5 files (`--limit`; `0` for all) |
| `-m 3` | At most 3 matches per file (`--max-matches`; `0` for all) |
| `-A 5`, `-C 5` | Lines of context after, or on both sides |
| `--lexical-only` | No meaning-based search and no model work |
| `-v` | Debug logs and full errors |

Run `seek completion <shell> --help` for shell completion.

## Descriptive search

Use a plain description when you know what the code does but not its name:

```bash
seek 'validate search query syntax'
seek -n 5 'find request parser lang:go -file:_test\.go$'
```

A query uses the model when it gives the model text, such as words or the
patterns of its filters. In every searched folder and repository, Seek adds
files whose code has a similar meaning, and the model ranks them with the text
matches. Filters and paths apply to both. A query that gives the model no text,
such as negations or punctuation alone, uses text order.

Meaning-based search is approximate. A file with every query word stays; a file
that only the model adds must pass a score threshold. No result means exit code
1, which does not prove that the code is absent. If the model fails, Seek
returns the text matches (`-v` gives the reason).

Write a description of three to six words: a shorter one adds few files by
meaning, and a longer one can add none.

The first search builds the meaning-based index; later searches embed only
changed files. Set `SEEK_PROVIDER=cpu` to run the model on the CPU,
for example to report a problem; the default picks the best provider of the
host.

## Ranked, not exhaustive

Seek ranks results; it does not list every match. For every occurrence, a
count, a rename, or an absence check, use an exhaustive tool:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

## Agent integration

The `seek-router` plugin teaches an agent the query syntax and routes its
static `grep`, `rg`, `git grep`, `fd`, and `find` commands to Seek.

```sh
claude plugin marketplace add dualeai/seek     # Claude Code
claude plugin install seek-router@seek --scope user

codex plugin marketplace add dualeai/seek      # OpenAI Codex
codex plugin add seek-router@seek
```

Start a new session after an install or an update. The router needs `jq` and a
POSIX `awk`, and leaves any command it does not support unchanged. Routed
searches show at most 20 files and 3 matches per file; prefix a command with
`SEEK_ROUTER=off` to run it unchanged. See the
[router guide](plugins/seek-router/README.md).

Without the plugin, add this to the agent's instructions:

```text
Use `seek` for ranked code navigation. Put filters in one quoted query and
paths after it: seek 'sym:Name file:src -file:test' ./cmd
Use a plain description when the name is unknown.
Use an exhaustive tool for renames, counts, and absence checks.
```

## Compared with ripgrep

| | ripgrep | seek |
|---|---|---|
| Search | Scans the files each time | Builds an index once, then reuses it |
| Order | Unspecified | Relevance; model ranking |
| Descriptions | Literal text or regular expression | Finds code with a similar meaning |
| Symbols | None | `sym:` finds definitions |
| Context | None by default | 3 lines on each side |
| Local changes | File contents | Committed and changed files, tagged `[uncommitted]` |

Use ripgrep for every matching line, counts, multiline or PCRE2 expressions,
and replacement output.

## Cache

Seek stores its indexes in the user cache, never in the searched folders:
`~/Library/Caches/seek` on macOS, `${XDG_CACHE_HOME:-$HOME/.cache}/seek` on
Linux. Set `SEEK_CACHE_DIR` to use another folder.

The index cache has one budget: 10% of the cache volume, or less, so that 5% of
the volume stays free. Set `SEEK_CACHE_BUDGET` (for example
`20G`) to change it. A search that does not fit stops with exit code 2. After
a search, Seek removes the least recently used indexes until the cache fits.
Unused compiled model files and the runtimes of other Seek versions go after 14
days. Cleanup runs at most once per `SEEK_GC_INTERVAL` (default `24h`), and
never on a network filesystem.

```sh
seek gc --dry-run --sort=size   # show the plan, largest indexes first
seek gc --force                 # clean up now
seek gc --all                   # delete everything that no search uses
```

Seek has no file-count or size limit. A file that it cannot index by content
(a NUL byte, under three bytes, larger than one 4 GiB index shard, not on disk,
or unreadable) is found by name only. After its results, a search gives the
count of such files, other than those under three bytes, that could also match.

## Parallel use

| Situation | What happens |
|-----------|--------------|
| The index is current | All searches run at the same time |
| Another search updates the index | Searches use the last complete index, can miss its newest changes, and find text matches only |
| An update fails | Seek warns, uses the last complete index, and finds text matches only |
| No index yet | The first search builds it; the others wait |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | At least one result |
| 1 | No result (not a proof of absence) |
| 2 | Error: usage, query, setting, or index failure |

## Security

A search runs no filter, diff, or credential command from a repository's Git
configuration, writes no repository configuration, starts no daemon, and uses
no network. See the [security policy](SECURITY.md) and the
[third-party notices](THIRD_PARTY_NOTICES.md). Each release has a CycloneDX
SBOM and [build attestations](https://github.com/dualeai/seek/attestations).

## Contributing

Open an issue to discuss a change before a pull request.

```bash
git clone https://github.com/dualeai/seek.git && cd seek
make install  # modules and test tools
make build    # needs Go 1.27+ and a C compiler (Xcode tools or GCC)
make test     # static analysis and unit tests
```

## License

[Apache-2.0](LICENSE). See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for
the resources in the executable.
