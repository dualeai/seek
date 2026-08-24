---
name: seek-search
description: Search code with seek, a BM25-ranked local search with ctags symbols. Use for any code search - finding a definition, tracing callers, locating config, or exploring an unfamiliar area - instead of grep, ripgrep, or find.
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

Flags go before the query.

## Examples

```sh
seek 'sym:executeParsedSearchScoped'        # where is this defined
seek 'sym:Index file:index -file:test'      # definitions, excluding tests
seek 'content:func.*Test lang:go -file:bench'
seek 'type:file config'                     # config-ish filenames
seek 'TODO' ./cmd ./docs                    # limit to two subtrees
seek -n 5 'retry backoff'                   # top 5 files only
```

## Paths

With no path, seek searches the current Git worktree. Path operands accept
directories or exact files, inside or outside the worktree, and can be mixed in
one call. Across roots, headers show the absolute path and a `[git]`/`[folder]`
tag so every match is directly openable.

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
