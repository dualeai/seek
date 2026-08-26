# seek-router

`seek-router` adds two parts to Claude Code and Codex:

- the `seek-search` skill, which teaches agents how to query seek;
- a `PreToolUse` hook, which rewrites supported static shell searches.

See [Agent Integration](../../README.md#agent-integration) for installation.

## Requirements

The router requires `seek`, `jq`, and a POSIX `awk`. Building or updating a seek
index also requires Universal Ctags on `PATH` or through `CTAGS_COMMAND`.
Context rewrites require the public seek `-A` and `-C` flags. If those flags are
not available, only context commands stay unchanged.

The router implementation is wholly inside this plugin. `bin/router.sh` owns
the hook JSON and `lib/router.awk` owns shell parsing and command adapters. Seek
does not import, call, or expose the router. The plugin invokes only normal
public seek commands.

## Routing contract

`bin/router.sh` accepts a hook payload on standard input. It has two results:

| Result | Meaning |
| --- | --- |
| `allow` with `updatedInput` | Replace the shell command with seek. |
| No output | Run the original shell command unchanged. |

The router handles one static shell call, or one file-name search piped to
`head` with a positive numeric limit (`-N`, `-n N`, `-nN`, `--lines N`, or
`--lines=N`). It accepts literal shell words and quoted strings. It leaves
assignments, variables, substitutions, globs, redirects, background jobs,
compound commands, comments, and other pipelines unchanged.

For example:

```text
grep -rni Foo ./cmd
  -> seek -n 20 -m 3 'case:no content:"Foo"' './cmd'

rg -l 'class Router|route_request' ./src | head
  -> seek -n 10 -m 3 'case:yes type:file content:"class Router|route_request"' './src'
```

Each adapter has a separate contract:

- `grep`: recursive search with an explicit path. It accepts bundled `r`, `n`,
  `l`, `H`, `I`, `F`, `E`, or `i` flags. Non-fixed patterns use a small ASCII
  atom grammar.
- `rg`: one pattern, optional literal paths, short search flags, and their
  direct long forms. `-e` selects one pattern in spaced or attached form.
  After-context and symmetric context need an explicit path and accept 0 to 512
  lines.
- `git grep`: one plain pattern and optional bundled `n`, `l`, `F`, or `i` flags.
  Literal paths can follow `--`. Tree objects and path magic stay unchanged.
- `fd`: `fd PATTERN [ROOT...]`, with optional leading `-t f`. It becomes a
  ranked indexed-path search limited to files.
- `find`: `find ROOT... -type f -name PATTERN` in either predicate order, with
  optional final `-print`.
  The name pattern can contain `*`, but no other glob operator. `*` does not
  cross a directory separator.

`-i` becomes `case:no`; other routed content searches use `case:yes`. The `fd`
adapter uses a small ASCII smart-case rule: `A` to `Z` makes the search
case-sensitive. Seek applies this regex to the full indexed path, returns files
only, and uses the seek corpus rules. This is ranked navigation, not fd output,
Unicode-case, or regex-parser emulation.

Routed regular expressions use Seek's Zoekt dialect. The router does not read a
ripgrep configuration file. Use the original command when those rules matter.

When `rg` or `fd` has no path, the rewrite adds `.`. This keeps the search in
the shell's current directory instead of widening it to the Git worktree root.

The hook keeps every field in the original `tool_input` object and changes only
`command`.

## Deliberate pass-through cases

Grep, ripgrep, Git, fd, find, the shell, and seek have different rules. The
router leaves these forms unchanged:

- `egrep`, `fgrep`, `ag`, and `ack`;
- non-recursive `grep` and `grep` that reads standard input;
- long or unknown flags, file filters, counts, replacement, and multiline
  modes;
- `git grep` tree objects, path magic, and regular-expression operators;
- `fd` actions and options other than a leading `-t f`;
- compound or repeated `find` expressions;
- dynamic shell syntax and pipelines other than a final supported `head`.

Use seek directly for richer ranked searches. Use the original search tool
when exact shell or regular-expression behavior matters.

## Ranked result limits

Routed commands use at most 20 files and three matches per file. An `rg`
context search uses one match per file. A final `head` can lower the file cap
for file-name results. These ranked results are not exhaustive.

For a rename, refactor, count, or complete reference list, bypass the hook:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

Set `SEEK_ROUTER=off` in the session environment to disable all routing.

## Fail-open behavior

The wrapper always exits with status 0. Invalid JSON, unsupported commands,
missing dependencies, and parser failures all produce no decision. The host
then runs the original command.

The hook rewrites a Bash call instead of denying it. The search runs in the
agent shell, and the tool result is not a permission denial.

## One hook entry

The package uses one unconditional `Bash` matcher. Claude Code and Codex both
load it. The wrapper has a 5-second timeout and exits quickly for commands
outside its contract. The emitted Seek command runs after the hook and is not
limited by this timeout.

Codex asks the user to trust plugin hooks before it runs them. Both hosts set
`CLAUDE_PLUGIN_ROOT`, so the shared hook command can locate the wrapper.

## Layout

```text
.claude-plugin/plugin.json   Claude Code manifest
.codex-plugin/plugin.json    Codex manifest
hooks/hooks.json             shared PreToolUse hook
bin/router.sh                fail-open wrapper
lib/router.awk               static shell parser and command adapters
skills/seek-search/SKILL.md  seek query guide
test/run.sh                  wrapper and package tests
test/router_test.py          command and ownership contract tests
```

### Why there is no Agent Plugins manifest

[Agent Plugins 1.0](https://agent-plugins.org/specification) standardizes skills
and MCP servers, but it does not define this hook. The package therefore keeps
the Codex and Claude Code manifests in their client-specific directories and
does not add a root `plugin.json`.

Codex discovers `hooks/hooks.json` through its
[default plugin path](https://learn.chatgpt.com/docs/hooks).

## Static test procedure

Run this command from the seek repository root:

```sh
make test-plugin
```

The target builds the repository version of seek for public CLI integration.
The plugin tests own the 18 reported commands, all adapters, shell safety,
fail-open paths, emitted commands, the ownership boundary, manifests, hook
timeouts, and the absence of a root `plugin.json`.

The wrapper test also reads the repository hook and marketplace files, so run
it through `make test-plugin` in the repository. It is not a copied-package
test.

For a host check, install the package and start a new Codex session in a
repository that has no `.codex/hooks.json`. Open `/hooks`, confirm that
`seek-router` appears under `PreToolUse`, trust it, and run a supported static
search. The shell transcript shows the emitted `seek` command.

## Tool scope

The plugin does not replace built-in `Grep` or `Glob` tools. A hook can change
arguments for one tool, but it cannot turn that call into a Bash call. Doing so
would require a denial path with different error and retry behavior.
