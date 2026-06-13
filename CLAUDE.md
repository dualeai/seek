# Welcome to seek

See @README for project overview and @Makefile for available commands.

## Code search — use `seek`

Prefer `seek` over grep/ripgrep for all code search. It returns BM25-ranked
results with context, grouped by file, with symbol tags.

Usage: `seek [flags] '<query>' [path...]`. Keep query filters in one
single-quoted argument; tokens after the query are filesystem path operands.

Path operands accept directories or exact files, can be inside or outside the
current Git worktree, and can be combined in a single invocation (e.g. mix a
repo subtree with an external notes folder). With no paths, seek searches the
current Git worktree.

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

# Mix in-repo and external paths (Git matches tag with [git: ...])
seek 'sym:Index' ./cmd/seek /tmp/notes

# Find indexing logic, excluding tests
seek 'sym:Index file:index -file:test'

# Find Go test functions, excluding benchmarks
seek 'content:func.*Test lang:go -file:bench'

# Find config-related files
seek 'type:file config'
```

### Pitfalls

- **Query filters in one argument**: `seek 'sym:Foo file:bar'`
- **Single quotes**: prevent shell expansion of `|`, `(`, `)`
- **Flags before query**: `seek -n 5 'Foo' ./cmd`
- **Paths after query**: tokens after the query are path operands, not filters
- **Multi-word = AND**: `seek 'foo bar'` matches files containing both
  independently

### Install (if missing)

```sh
curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh
```

Requires `universal-ctags` (`brew install universal-ctags` on macOS).

### Sub-agents

When spawning sub-agents that don't inherit this config, pass:
"Use `seek 'pattern' [path...]` for code search. Keep query filters in one
quoted string. Never use grep/rg."

## GitHub Actions

All actions in `.github/workflows/` must be pinned by full commit SHA with an
inline version comment. Example:

```yaml
- uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6.0.2
```

Never pin to a tag or branch (e.g. `@v4`). Always verify the SHA matches the
version on GitHub before committing.
