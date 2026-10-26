---
name: seek-search
description: Search code with seek, a local ranked search for exact queries, descriptions, and symbol definitions. Use it to find definitions, callers, configuration, and unfamiliar code. Use an exhaustive tool for renames, counts, and complete result lists.
---

# Search code with seek

`seek` returns the most relevant files with context, and tags the definitions
that `sym:` finds. Use it to find code, not to list every match.

```sh
seek [flags] '<query>' [path...]
```

Flags come before the query, and paths after it. Put all filters in the one
single-quoted query. With no path, seek searches the current folder. Paths can
be folders or files, inside or outside Git.

| Filter | Finds |
| --- | --- |
| `sym:Name` | definitions: functions, classes, methods |
| `content:REGEX` | content only |
| `file:path`, `-file:path` | keep or drop paths that match |
| `lang:go` | one language |
| `type:file` | file names only |

| Flag | Effect |
| --- | --- |
| `-n N` | at most N files (`0` for all) |
| `-m N` | at most N matches per file (`0` for all) |
| `-A N`, `-C N` | N lines of context after, or on both sides |
| `--lexical-only` | no model work |

```sh
seek 'sym:handleRequest'                     # where is this defined
seek 'sym:Index file:index -file:test'       # definitions, no tests
seek 'type:file config'                      # configuration file names
seek -n 5 'retry backoff' ./cmd ./docs       # top 5 files in two folders
```

## Descriptions

When you know what the code does but not its name, write a description:

```sh
seek 'validate search query syntax'
seek 'find request parser lang:go -file:_test\.go$'
```

A local model adds files with a similar meaning and ranks them with the text
matches. Filters and paths apply to both. The results are approximate: a
result can contain none of the query words, and no result (exit code 1) does
not prove that the code is absent.

Write a description of three to six words: a shorter one adds few files by
meaning, and a longer one can add none.

The first search of a large repository builds its indexes and can take tens of
seconds. `--lexical-only` skips the model.

## When seek is the wrong tool

For every occurrence, a count, a rename, or an absence check, use grep:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

The seek router rewrites simple `grep`, `rg`, `git grep`, `fd`, and `find`
commands into seek searches of at most 20 files and 3 matches per file. The
`SEEK_ROUTER=off` prefix runs the command unchanged.
