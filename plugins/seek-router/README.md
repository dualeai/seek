# seek-router

`seek-router` adds two parts to Claude Code and Codex:

- the `seek-search` skill, which teaches agents how to query seek;
- a `PreToolUse` hook, which rewrites simple static shell searches into seek
  searches.

See [Agent integration](../../README.md#agent-integration) to install it. The
router needs `seek`, `jq`, and a POSIX `awk`.

## What the hook does

```text
grep -rni Foo ./cmd
  -> seek -n 20 -m 3 'case:no content:"Foo"' './cmd'

rg -l 'class Router|route_request' ./src | head
  -> seek -n 10 -m 3 'case:yes type:file content:"class Router|route_request"' './src'
```

Routed searches show at most 20 files and 3 matches per file (one per file for
an `rg` context search). They are ranked, not complete. A routed search can
use the model too, so the first one in a large folder can build the
meaning-based index and take tens of seconds. For every match, prefix the
command with `SEEK_ROUTER=off`, or set it in the session environment to stop
all routing.

The hook handles one static shell call, or one file-name search piped to `head`
with an optional positive line count. It accepts literal words and quoted
strings.

- `grep`: recursive search for one word (any string with `F`) with an explicit
  path, with bundled `r`, `n`, `l`, `H`, `I`, `F`, `E`, or `i` flags.
- `rg`: one pattern (or `-e` pattern), optional literal paths, short search
  flags and their long forms. Context flags need an explicit path.
- `git grep`: one plain pattern, bundled `n`, `l`, `F`, or `i` flags, and
  literal paths after `--`.
- `fd`: `fd NAME [ROOT...]`, with an optional leading `-t f`, where `NAME` is
  a literal name with no `/` and no regular-expression operator. An uppercase
  ASCII letter makes it case-sensitive.
- `find`: `find ROOT... -type f -name PATTERN`, in either order, with an
  optional final `-print`. The pattern can hold `*`, which does not cross a `/`.

`-i` becomes `case:no`; other content searches use `case:yes`. Regular
expressions use Seek's Zoekt dialect. With no path, Seek searches the current
folder, as the original command does.

The hook leaves everything else unchanged: `egrep`, `fgrep`, `ag`, `ack`,
non-recursive `grep`, standard input, other flags, file filters, counts,
replacement, multiline modes, `git grep` tree objects and path magic, other
`fd` options, compound `find` expressions, variables, substitutions, globs,
redirects, and other pipelines.

The hook always exits with status 0. Invalid input, an unsupported command, a
missing dependency, or a parser failure gives no decision, and the host runs
the original command. The hook does not change the built-in `Grep` and `Glob`
tools.

## Maintainers

`bin/router.sh` reads the hook payload and writes `allow` with `updatedInput`
(only `command` changes) or nothing. `lib/router.awk` holds the shell parser and
the command adapters. The plugin calls only the public `seek` command.

```text
.claude-plugin/plugin.json   Claude Code manifest
.codex-plugin/plugin.json    Codex manifest
hooks/hooks.json             shared PreToolUse hook (5-second timeout)
bin/router.sh                fail-open wrapper
lib/router.awk               shell parser and command adapters
skills/seek-search/SKILL.md  seek query guide
test/                        wrapper, adapter, and package tests
```

There is no root `plugin.json`: Agent Plugins 1.0 does not define this hook,
so each host keeps its own manifest.

Run `make test-plugin` from the repository root. It builds seek and tests every
adapter, the fail-open paths, the manifests, and the hook timeout. To check a
host, install the plugin, start a new Codex session, open `/hooks`, trust
`seek-router`, and run a supported search.
