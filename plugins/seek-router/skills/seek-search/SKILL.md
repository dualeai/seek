---
name: seek-search
description: Search code with seek, a local ranked search for exact queries, descriptions, and symbol definitions. Use it to find definitions, callers, configuration, and unfamiliar code. Use an exhaustive tool for renames, counts, and complete result lists.
---

# Search code with seek

`seek` returns relevance-ranked files with context and symbol tags. Use it for
navigation. Use an exhaustive tool when you need every match.

```sh
seek [flags] '<query>' [path...]
```

## Query filters

Put all filters in one quoted argument. Put paths after it.

| Filter | Finds |
| --- | --- |
| `sym:Name` | definitions - functions, classes, methods (ctags) |
| `content:REGEX` | a regex match in file content |
| `file:path` | paths matching a regular expression |
| `-file:path` | paths that do not match a regular expression |
| `lang:go` | one language |
| `type:file` | filenames only, no content |

Before model ranking, a query with bare words requires every word. A final
model-ranked result can lack one or more query words.

## Flags

| Flag | Effect |
| --- | --- |
| `-n N` | display at most N files (0 = all files returned within search bounds) |
| `-m N` | display at most N matches per file (0 = all returned matches) |
| `-A N` | N lines after each match (0-512) |
| `-C N` | N lines before and after each match (0-512) |
| `--lexical-only` | skip meaning-based indexing, search, and model ranking |

Flags go before the query. Do not combine `-A` and `-C`.

## Examples

```sh
seek 'sym:executeParsedSearchScoped'        # where is this defined
seek 'sym:Index file:index -file:test'      # definitions, excluding tests
seek 'content:func.*Test lang:go -file:bench'
seek 'type:file config'                     # configuration filenames
seek 'TODO' ./cmd ./docs                    # limit to two subtrees
seek -n 5 'retry backoff'                   # top 5 files only
seek -n 5 -m 1 -A 20 'sym:executeParsedSearchScoped' ./cmd/seek
```

## Search from descriptions

When you know the behavior but not its identifier or file, write a short
English description with two or more words:

```sh
seek 'find request parser'
seek -n 5 -m 2 -C 1 'validate search query syntax'
seek 'find request parser lang:go -file:_test\.go$'
```

Plain queries with two or more words can use the bundled local model. One clean
Git worktree, one file, or one folder can also add matches based on code meaning.
Other searches can still use the model to rank text matches.

A description can include `lang:`, `file:`, and `-file:` filters. These filters
stay active if Seek uses another search method. Exact identifiers, quoted
phrases, one-word queries, other filters, Boolean alternatives, and general
negation use text ranking.

Seek considers at most 128 files for a description. Each text-search pass is
limited to 10,000 matches and 60 seconds. These bounds are separate from `-n`.
Meaning-based search is approximate. A result can contain none of the query
words.

When no file contains every query word, Seek returns results that omit some
query words only if the best model score is strong enough. Otherwise, it returns
no output and exits with code 1. This does not prove absence. If the model fails,
Seek returns results that contain every query word.

Unless `--lexical-only` is set, Seek keeps its text and meaning-based indexes up
to date. The first search of a large repository can take tens of seconds or
longer and use several GiB of memory. Use `--lexical-only` to skip the
meaning-based index and all model work.

## Paths

With no path, seek searches the current Git worktree. You can mix directories
and exact files from inside or outside it. Across multiple roots, headers use
absolute paths and a `[git]` or `[folder]` tag.

Path operands limit results. `file:` and `-file:` filters apply after indexing,
so they do not reduce index size limits.

## Pitfalls

- **One quoted argument for filters.** `seek 'sym:Foo file:bar'`, not
  `seek sym:Foo file:bar`.
- **Single quotes**, so the shell does not expand `|`, `(`, `)`.
- **Flags before the query**: `seek -n 5 'Foo' ./cmd`.
- **Paths after the query** are path operands, not filters.

## When seek is the wrong tool

By default, `-n 0` and `-m 0` display all results returned within Seek's search
bounds. The router uses `-n 20 -m 3`. For every occurrence, such as for a rename,
refactor, or call-site count, use grep directly:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

The `SEEK_ROUTER=off` prefix matters only when the seek router hook is
installed; it tells the hook to leave that command alone.

The router supports strict static forms of `grep`, `rg`, `git grep`, `fd`, and
`find`. Unsupported flags or dynamic shell syntax run unchanged. See the
plugin README for the exact adapter contract.
