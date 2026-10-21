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
| `-n N` | display at most N files (0 = all returned files) |
| `-m N` | display at most N matches per file (0 = all returned matches) |
| `-A N` | N lines after each match |
| `-C N` | N lines before and after each match |
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

Plain queries with two or more words use the bundled local model: every
searched folder and repository adds matches based on code meaning, and the
model ranks them with the text matches. Filters (`lang:`, `file:`, `-file:`,
`sym:`, `content:`, a path operand) apply to both. One-word queries, a lone
quoted phrase, and `or` between descriptions use text ranking. The README
"Support table" lists each query form.

Meaning-based search is approximate. A result can contain none of the query
words. A result that the model adds must pass its score threshold; files that
contain every query word stay. With no result, Seek returns no output and exits
with code 1. This does not prove absence. If the model fails, Seek returns the
text matches.

Unless `--lexical-only` is set, a description search keeps the text and
meaning-based indexes up to date; other searches keep only the text index up to
date. The first search of a large repository can take tens of seconds or
longer and use several GiB of memory. A first routed `grep` on a large cold
folder waits for the full text build, with no deadline. Use `--lexical-only`
to skip the meaning-based index and all model work.

## Paths

With no path, seek searches the current folder, the same as `.`. You can mix
directories and exact files from inside or outside it. A header is the path as
you typed it plus the path below it; across several roots it also has a `[git]`
or `[folder]` tag. `file:` and `-file:` match that same path.

Path operands limit results. `file:` and `-file:` filters apply after indexing,
so they do not change what Seek indexes.

## Pitfalls

- **One quoted argument for filters.** `seek 'sym:Foo file:bar'`, not
  `seek sym:Foo file:bar`.
- **Single quotes**, so the shell does not expand `|`, `(`, `)`.
- **Flags before the query**: `seek -n 5 'Foo' ./cmd`.
- **Paths after the query** are path operands, not filters.

## When seek is the wrong tool

By default, `-n 0` and `-m 0` display all returned results. The router uses
`-n 20 -m 3`. For every occurrence, such as for a rename, refactor, or
call-site count, use grep directly:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

The `SEEK_ROUTER=off` prefix matters only when the seek router hook is
installed; it tells the hook to leave that command alone.

The router supports strict static forms of `grep`, `rg`, `git grep`, `fd`, and
`find`. Unsupported flags or dynamic shell syntax run unchanged. See the
plugin README for the exact adapter contract.
