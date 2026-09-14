# Welcome to seek

See @README for project overview and @Makefile for available commands.

## Writing

Always use ASD-STE100 Simplified Technical English in responses to the user.

Use this project adaptation of Orwell's six rules for prose, including responses,
documentation, comments, commit messages, issues, and pull requests:

1. Avoid stock metaphors, similes, idioms, and other figures of speech.
2. Choose the shorter of two words when both are equally precise.
3. Remove every word that adds no meaning.
4. Use active voice whenever it states the same meaning clearly.
5. Replace foreign phrases, scientific terms, and jargon with everyday English
   when it is equally precise.
6. Break any rule above before making the prose unclear, inaccurate, unsafe,
   unnatural, or less precise. Preserve exact quotations and required technical
   language.

## Code search — use `seek`

Prefer `seek` over grep/ripgrep for all code search. It returns
relevance-ranked results with context, grouped by file, with symbol tags.

Usage: `seek [flags] '<query>' [path...]`. Keep query filters in one
single-quoted argument; tokens after the query are filesystem path operands.

Path operands accept directories or exact files, can be inside or outside the
current Git worktree, and can be combined in a single invocation (e.g. mix a
repo subtree with an external notes folder). Symlinked path operands are
silently resolved to their targets; symlinks discovered during walks are still
skipped. With no paths, seek searches the current Git worktree.

### Key filters (combine with spaces inside the quotes)

- `sym:Name` — find definitions (functions, classes, methods) via ctags
- `file:path` / `-file:path` — include/exclude paths
- `lang:go` — filter by language
- `content:regex` — regex on file content
- `type:file` — return matching filenames only

### Project examples

```sh
# Find the parsed search execution boundary
seek 'sym:executeParsedSearchScoped'

# Search only selected paths
seek 'Search' ./cmd/seek

# Multiple paths in one invocation
seek 'func' ./cmd/seek/searcher.go ./cmd/seek/indexer.go

# Search an exact file
seek 'package' ./cmd/seek/searcher.go

# Search a folder outside the Git worktree
seek 'TODO' /tmp/notes

# Mix in-repo and external paths (across roots, headers show the absolute
# path and a [git]/[folder] tag so each match is directly openable)
seek 'sym:Index' ./cmd/seek /tmp/notes

# Find indexing logic, excluding tests
seek 'sym:Index file:index -file:test'

# Find Go test functions, excluding benchmarks
seek 'content:func.*Test lang:go -file:bench'

# Find config-related files
seek 'type:file config'
```

### Descriptive search

Use a plain query when you know the behavior but not its identifier or file:

```sh
seek 'validate search query syntax'
```

Plain queries with two or more words use local model re-ranking by default.
One unscoped clean Git worktree, one stable plain file, or one stable plain
folder also uses semantic retrieval. Scoped or dirty Git and multi-corpus
searches can still use model re-ranking when enough candidates exist, but not
semantic retrieval. A description can contain `lang:`, `file:`, and `-file:`
filters; Seek keeps them in lexical and semantic retrieval. Eligibility follows
Zoekt's parsed and simplified query tree, so equivalent spellings and aliases
can use the same route. Exact identifiers, phrases, one-word queries, and trees
that retain other filters, Boolean alternatives, or general negation use strict
BM25 order.
Unless `--lexical-only` is set, a supported search builds or updates both Zoekt
and semantic data, including exact and router-generated queries. An eligible
description uses Zoekt only when truncation is certain before model start. A
first large-repo search can take tens of seconds or longer and use all
available compute, several GiB of memory, and significant cache space. Use
`--lexical-only` to skip all semantic index and model work. Use
`SEEK_ROUTER=off grep` for absence checks, renames, counts, and complete
call-site lists.

An eligible description scores at most 128 candidate files before display
limits. If it has no strict all-word result, the best normalized model score
must clear the route's support boundary. Rejection returns no output and status
1. This is not proof of absence. A non-truncated strict all-word result admits
the complete model-expanded set without the score check. Semantic retrieval
remains approximate and bounded.

### Pitfalls

- **Query filters in one argument**: `seek 'sym:Foo file:bar'`
- **Single quotes**: prevent shell expansion of `|`, `(`, `)`
- **Flags before query**: `seek -n 5 'Foo' ./cmd`
- **Paths after query**: tokens after the query are path operands, not filters
- **Strict lexical terms use AND**: the strict branch of `seek 'foo bar'`
  matches files containing both terms. A final reranked result can lack one or
  both terms.

### Install (if missing)

```sh
curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh
```

Requires `universal-ctags` (`brew install universal-ctags` on macOS).

### Sub-agents

When spawning sub-agents that don't inherit this config, pass:
"Use `seek 'pattern' [path...]` for ranked code navigation. Keep query filters
in one quoted string. Plain multi-word descriptions use model re-ranking by
default. A supported default search maintains both Zoekt and semantic data; use
`--lexical-only` to skip all semantic and model work. An unscoped clean Git
worktree or one stable plain file or folder also uses semantic retrieval.
Descriptions can include `lang:`, `file:`, and `-file:` filters. Eligible
descriptions score at most 128 candidate files. Without a strict all-word
result, the best normalized score must clear a support boundary; a
non-truncated strict result admits expansion without that score check. Use
`SEEK_ROUTER=off grep` for absence checks, all occurrences, counts, renames, or
complete call-site lists. A descriptive no-result is not proof of absence."

## GitHub Actions

All actions in `.github/workflows/` must be pinned by full commit SHA with an
inline version comment. Example:

```yaml
- uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
```

Never pin to a tag or branch (e.g. `@v4`). Always verify the SHA matches the
version on GitHub before committing.

## Router plugin

`plugins/seek-router/` ships a PreToolUse hook that rewrites the agent's safe
static `grep`, `rg`, `git grep`, `fd`, and `find` calls into seek searches, plus
the `seek-search` skill.
Load it in this repo with `claude --plugin-dir ./plugins/seek-router`; Codex
picks it up from `.codex/hooks.json` with no install.

The router caps ranked results at 20 files and 3 matches per file. It answers
"where is this", not "every occurrence". When you need every hit for a rename,
refactor, or call-site count:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

`SEEK_ROUTER=off` in the environment disables routing for a whole session.

The hook requires `jq` and a POSIX `awk`. Its parser and command adapters stay
inside the plugin; Seek exposes only its normal public CLI. The hook uses strict
forms for each supported command and accepts a final `head` for file results.
It leaves unsupported flags, dynamic shell syntax, and other pipelines
unchanged. See `plugins/seek-router/README.md` for the full contract.

Router changes must keep `make test-plugin` green; it feeds recorded hook
payloads to the script and asserts exit code 0 on every path, because exit 2
reads as a deny and would break search instead of falling back to grep.
