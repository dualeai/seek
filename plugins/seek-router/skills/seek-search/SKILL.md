---
name: seek-search
description: Search code with seek, a BM25-ranked local search with optional code re-ranking and ctags symbols. Use for any code search - finding a definition, tracing callers, locating config, or exploring an unfamiliar area - instead of grep, ripgrep, or find.
---

# Search code with seek

`seek` ranks results by relevance and returns them grouped by file with context
and symbol tags. Use it wherever you would otherwise reach for grep, ripgrep,
git grep, or find.

```sh
seek [flags] '<query>' [path...]
```

## Query filters

All filters go inside ONE quoted argument. Paths come after it.

| Filter | Finds |
| --- | --- |
| `sym:Name` | definitions - functions, classes, methods (ctags) |
| `content:REGEX` | a regex match in file content |
| `file:path` | paths matching a substring |
| `-file:path` | paths NOT matching a substring |
| `lang:go` | one language |
| `type:file` | filenames only, no content |

Bare words are matched independently and combined with AND: `seek 'parse token'`
returns files containing both.

## Flags

| Flag | Effect |
| --- | --- |
| `-n N` | at most N files (0 = unlimited) |
| `-m N` | at most N matches per file (0 = unlimited) |
| `-A N` | N lines after each match (0-512) |
| `-C N` | N lines before and after each match (0-512) |
| `--rerank` | rank a plain descriptive query with the bundled code model |

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

Use `--rerank` when you know what the code does but do not know its identifier
or file. Write a short English description with two or more words:

```sh
seek --rerank 'find request parser' ./cmd
seek --rerank 'remove expired cache entries' ./cmd
seek --rerank -n 5 -m 2 -C 1 'validate search query syntax' ./cmd
```

Seek checks files that match parts of the query and moves files that best match
its meaning toward the top. This can find useful code that does not contain
every query word.

**Ranking warning:** Re-ranking can move a less useful file upward. Compare the
same query with and without `--rerank` when the order is important.

Re-ranking applies only to plain queries. Do not add it to an exact identifier,
an exact phrase expression such as `seek '"two words"'`, a `sym:` query,
filter, regular expression, Boolean expression, negation, or one-word query.
Those forms keep the normal BM25 path. The router does not add `--rerank`;
call `seek` directly when you want it.

Do not use a re-ranked result to prove that all query words exist or that code
is present. Use normal search for absence checks, exact matches, renames,
counts, and complete call-site lists.

The model runs locally. Seek can give it nearby source context that the user
did not request for display. The `-C` and `-A` flags still control returned
context, while `-n` and `-m` still control returned files and matches. If
re-ranking is unavailable or fails, Seek returns the normal BM25 results that
match all query terms.

## Paths

With no path, seek searches the current Git worktree. Path operands accept
directories or exact files, inside or outside the worktree, and can be mixed in
one call. Across roots, headers show the absolute path and a `[git]`/`[folder]`
tag so every match is directly openable.

Path operands constrain what Seek indexes. `file:` and `-file:` query filters
apply to results after indexing and do not reduce index limits.

## Pitfalls

- **One quoted argument for filters.** `seek 'sym:Foo file:bar'`, not
  `seek sym:Foo file:bar`.
- **Single quotes**, so the shell does not expand `|`, `(`, `)`.
- **Flags before the query**: `seek -n 5 'Foo' ./cmd`.
- **Paths after the query** are path operands, not filters.

## When seek is the wrong tool

seek ranks results. Its `-n` and `-m` defaults are unlimited, but the router
uses `-n 20 -m 3` to keep automatic searches small. A routed search is not
exhaustive. When you need every occurrence, such as for a rename, refactor, or
call-site count, use grep directly:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

The `SEEK_ROUTER=off` prefix matters only when the seek router hook is
installed; it tells the hook to leave that command alone.

The router supports strict static forms of `grep`, `rg`, `git grep`, `fd`, and
`find`. Unsupported flags or dynamic shell syntax run unchanged. See the
plugin README for the exact adapter contract.
