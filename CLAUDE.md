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

## Performance objective

Seek's objective is the shortest time for every search, using as much CPU,
memory, and disk as it needs. Low resource use is not a goal.

- Do not add a limit, a gate, or a fixed concurrency cap that saves CPU, memory,
  or disk at the cost of time.
- Size budgets from the host (CPU count, physical memory), not from small fixed
  values.
- Limits stay only where they protect correctness: the Zoekt 4 GiB shard
  format, read budgets that cannot deadlock, and the one cache budget
  (`SEEK_CACHE_BUDGET`).
- Measure before and after a performance change, and keep the faster version.

## Code search — use `seek`

Prefer `seek` over grep and ripgrep for code search. It returns ranked results
with context, grouped by file, with definition tags.

Usage: `seek [flags] '<query>' [path...]`. Flags come before the query, and
paths after it. Put all filters in the one single-quoted query. Paths can be
folders or files, inside or outside Git; with no path, seek searches the
current folder.

- `sym:Name` — definitions
- `file:path`, `-file:path` — keep or drop paths
- `lang:go` — one language
- `content:regex` — content only
- `type:file` — file names only

```sh
seek 'sym:searchPlannedCorpusParsed'
seek 'sym:Index file:index -file:test' ./cmd/seek
seek 'TODO' ./cmd/seek /tmp/notes
seek 'validate search query syntax'
```

A description of two or more words also uses the local model to find code by
meaning; filters and paths apply to both. These results are approximate, and
no result (exit code 1) is not proof of absence. `--lexical-only` skips the
model. The first description search of a large repository builds its index and
can take tens of seconds.

For absence checks, renames, counts, and complete call-site lists, use
`SEEK_ROUTER=off grep -rn 'PATTERN' .`

When a sub-agent does not read this file, pass it:
"Use `seek 'query' [path...]` for ranked code navigation, with all filters in
one quoted query. A description of two or more words also finds code by
meaning. Use `--lexical-only` to skip the model. Use `SEEK_ROUTER=off grep` for
absence checks, counts, renames, and complete call-site lists."

## GitHub Actions

All actions in `.github/workflows/` must be pinned by full commit SHA with an
inline version comment. Example:

```yaml
- uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
```

Never pin to a tag or branch (e.g. `@v4`). Always verify the SHA matches the
version on GitHub before committing.

## Router plugin

`plugins/seek-router/` has a PreToolUse hook that rewrites simple static
`grep`, `rg`, `git grep`, `fd`, and `find` calls into seek searches (at most 20
files and 3 matches per file), and the `seek-search` skill. Load it in this
repo with `claude --plugin-dir ./plugins/seek-router`; Codex reads
`.codex/hooks.json`. See `plugins/seek-router/README.md` for the contract.

The parser and the command adapters stay inside the plugin; Seek exposes only
its public CLI. Router changes must keep `make test-plugin` green: the hook
must exit 0 on every path, because exit 2 reads as a deny and blocks the search
instead of falling back to grep.
