---
name: seek-search
description: Search code with seek, a local relevance-ranked search with lexical retrieval, default code re-ranking, eligible semantic retrieval, and ctags symbols. Use it for ranked navigation of definitions, callers, configuration, and unfamiliar code. Use an exhaustive tool for renames, counts, and complete result lists.
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

The strict lexical branch matches bare words independently and combines them
with AND. A final reranked result can lack one or both words.

## Flags

| Flag | Effect |
| --- | --- |
| `-n N` | display at most N files (0 = no display limit) |
| `-m N` | display at most N matches per file (0 = no display limit) |
| `-A N` | N lines after each match (0-512) |
| `-C N` | N lines before and after each match (0-512) |
| `--lexical-only` | skip semantic indexing, search, and model re-ranking |

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

## Search from descriptions

When you know the behavior but not its identifier or file, write a short
English description with two or more words:

```sh
seek 'find request parser'
seek -n 5 -m 2 -C 1 'validate search query syntax'
```

Seek collects up to 20 files from strict lexical, relaxed lexical, and semantic
retrieval. The model scores each path with its best nearby code. Seek combines
the lexical and model orders with weighted reciprocal rank fusion; lexical rank
has twice the model weight. Results can have none of the query words.

Plain queries with two or more words use model re-ranking by default. One
unscoped clean Git worktree, one stable plain file, or one stable plain folder
also uses semantic retrieval. Scoped or dirty Git and multi-corpus searches can
use model re-ranking when enough candidates exist, but not semantic retrieval.
Exact identifiers and phrases, `sym:` queries, filters, regular expressions,
Boolean operators, negation, and one-word queries use strict BM25 order.

Unless `--lexical-only` is set, every supported search builds or updates both
Zoekt and semantic data. This includes exact queries and commands rewritten by
the router. A first search of a large repo can take tens of seconds or longer
and use all available compute, several GiB of memory, and significant cache
space. Use `--lexical-only` only when you want the Zoekt-only fast path with no
model work.

Use normal seek for exact ranked navigation. For absence checks, renames,
counts, and complete call-site lists, use the exhaustive command below.

The model runs locally and can use context that is not in the output. Display
flags still apply. If semantic retrieval is unavailable, model re-ranking can
still use lexical candidates. If the model fails, Seek returns the strict
all-word BM25 results. A damaged USearch graph uses an exact vector scan when
the stored vectors are valid.

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
