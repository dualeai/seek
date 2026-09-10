---
name: seek-search
description: Search code with seek, a local BM25 search with optional code re-ranking and ctags symbols. Use it for ranked navigation of definitions, callers, configuration, and unfamiliar code. Use an exhaustive tool for renames, counts, and complete result lists.
---

# Search code with seek

`seek` returns relevance-ranked files with context and symbol tags. Use it for
navigation. Use an exhaustive tool when you need every match.

```sh
seek [flags] '<query>' [path...]
```

## Query filters

All filters go inside ONE quoted argument. Paths come after it.

| Filter | Finds |
| --- | --- |
| `sym:Name` | definitions - functions, classes, methods (ctags) |
| `content:REGEX` | a regex match in file content |
| `file:path` | paths matching a regular expression |
| `-file:path` | paths NOT matching a regular expression |
| `lang:go` | one language |
| `type:file` | filenames only, no content |

Bare words are matched independently and combined with AND: `seek 'parse token'`
returns files containing both.

## Flags

| Flag | Effect |
| --- | --- |
| `-n N` | display at most N files (0 = no display limit) |
| `-m N` | display at most N matches per file (0 = no display limit) |
| `-A N` | N lines after each match (0-512) |
| `-C N` | N lines before and after each match (0-512) |
| `--rerank` | rank an eligible plain query with the local model |

Flags go before the query. Do not combine `-A` and `-C`.

## Examples

```sh
seek 'sym:executeParsedSearchScoped'        # where is this defined
seek 'sym:Index file:index -file:test'      # definitions, excluding tests
seek 'content:func.*Test lang:go -file:bench'
seek 'type:file config'                     # config-ish filenames
seek 'TODO' ./cmd ./docs                    # limit to two subtrees
seek -n 5 'retry backoff'                   # top 5 files only
seek -n 5 -m 1 -A 20 'sym:executeParsedSearchScoped' ./cmd/seek
```

## Re-rank descriptive searches

Use `--rerank` when you know the behavior but not its identifier or file. Write
a short English description with two or more words:

```sh
seek --rerank 'find request parser' ./cmd
seek --rerank -n 5 -m 2 -C 1 'validate search query syntax' ./cmd
```

Seek scores up to 20 files that match at least one query word. The model scores
each path with its best nearby code. Seek combines the model and BM25 orders
with weighted reciprocal rank fusion; BM25 has twice the model weight. Results
can lack some query words.

**Ranking warning:** Re-ranking can reduce result quality. Compare the query
with and without `--rerank` when order matters.

`--rerank` accepts only plain queries with two or more words. Exact identifiers
and phrases, `sym:` queries, filters, regular expressions, Boolean operators,
negation, and one-word queries stay on the BM25 path. The router never adds
`--rerank`; call `seek` directly.

Use normal seek for exact ranked navigation. For absence checks, renames,
counts, and complete call-site lists, use the exhaustive command below.

The model runs locally and can use context that is not in the output. Display
flags still apply. If re-ranking fails or is unavailable, Seek returns the
normal all-word BM25 results.

## Paths

With no path, seek searches the current Git worktree. You can mix directories
and exact files from inside or outside it. Across roots, headers use absolute
paths and a `[git]` or `[folder]` tag.

Path operands constrain the search results. They can also enable a scoped index
when a whole Git repo exceeds an index limit. `file:` and `-file:` query filters
apply to results after indexing and do not reduce index limits.

## Pitfalls

- **One quoted argument for filters.** `seek 'sym:Foo file:bar'`, not
  `seek sym:Foo file:bar`.
- **Single quotes**, so the shell does not expand `|`, `(`, `)`.
- **Flags before the query**: `seek -n 5 'Foo' ./cmd`.
- **Paths after the query** are path operands, not filters.

## When seek is the wrong tool

By default, `-n` and `-m` add no display limits, but internal safety bounds
still apply. The router uses `-n 20 -m 3`. For every occurrence, such as for a
rename, refactor, or call-site count, use grep directly:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

The `SEEK_ROUTER=off` prefix matters only when the seek router hook is
installed; it tells the hook to leave that command alone.

The router supports strict static forms of `grep`, `rg`, `git grep`, `fd`, and
`find`. Unsupported flags or dynamic shell syntax run unchanged. See the
plugin README for the exact adapter contract.
