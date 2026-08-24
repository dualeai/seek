# seek-router

`seek-router` adds two parts to Claude Code and Codex:

- the `seek-search` skill, which teaches agents how to query seek;
- a `PreToolUse` hook, which rewrites a small set of safe shell searches.

See [Agent Integration](../../README.md#agent-integration) for installation.

## Requirements

The hook requires `seek` and `jq` on `PATH`. If either command is missing, the
hook returns no decision and the original shell command runs.

## Routing contract

`bin/router.sh` accepts a hook payload on standard input. It has two results:

| Result | Meaning |
| --- | --- |
| `allow` with `updatedInput` | Replace the shell command with seek. |
| No output | Run the original shell command unchanged. |

The router handles only static searches whose meaning it can preserve:

- recursive `grep` with at least one explicit path;
- `rg` with an optional explicit path;
- short flags that do not change the result set;
- an ASCII search atom and ASCII literal paths.

For example:

```text
grep -rni Foo ./cmd
  -> seek -n 20 -m 3 'case:no content:Foo' './cmd'

rg needleFunc
  -> seek -n 20 -m 3 'case:yes content:needleFunc' '.'
```

The allowed `grep` short flags are `r`, `n`, `H`, `I`, `F`, `E`, and `i`.
The allowed `rg` short flags are `n`, `H`, `I`, `F`, and `i`. `-i` becomes
`case:no`; all other routed searches use `case:yes`.

When `rg` has no path, the rewrite adds `.`. This keeps the search in the
shell's current directory instead of widening it to the Git worktree root.

The hook keeps every field in the original `tool_input` object and changes only
`command`. It adds a short notice that explains the cap and the bypass.

## Why the contract is small

Grep, ripgrep, the shell, and seek have different query and path rules. A broad
translation can return the wrong files without an error. The router therefore
leaves these forms unchanged:

- `git grep`, `egrep`, `fgrep`, `ag`, and `ack`;
- non-recursive `grep` and `grep` that reads standard input;
- long flags, unknown flags, file filters, counts, file lists, and context
  flags;
- regular-expression operators, phrases, non-ASCII patterns, and seek filter
  syntax;
- globbing, variables, command substitution, comments, pipes, redirects, and
  compound shell commands;
- dynamic or non-ASCII paths.

Use seek directly for richer ranked searches. Use the original search tool
when exact shell or regular-expression behavior matters.

## Ranked result limits

Every routed command uses `-n 20 -m 3`: at most 20 files and three matches per
file. This limit keeps broad searches small. It also makes routed results
nonexhaustive.

For a rename, refactor, count, or complete reference list, bypass the hook:

```sh
SEEK_ROUTER=off grep -rn 'PATTERN' .
```

Set `SEEK_ROUTER=off` in the session environment to disable all routing.

## Fail-open behavior

The router always exits with status 0. Invalid JSON, unsupported commands,
missing dependencies, and internal parsing failures all produce no decision.
The harness then runs the original command.

The hook rewrites a Bash call instead of denying it. The search runs in the
agent shell, and the tool result is not a permission denial.

## One hook entry

The package uses one unconditional `Bash` matcher. Claude Code and Codex both
load it. The script exits quickly for commands outside its contract.

Codex asks the user to trust plugin hooks before it runs them. Both harnesses
set `CLAUDE_PLUGIN_ROOT`, so the shared hook command can locate the script.

## Layout

```text
.claude-plugin/plugin.json   Claude Code manifest
.codex-plugin/plugin.json    Codex manifest
hooks/hooks.json             shared PreToolUse hook
bin/router.sh                fail-open command router
skills/seek-search/SKILL.md  seek query guide
test/run.sh                  router and packaging tests
```

### Why there is no Agent Plugins manifest

[Agent Plugins 1.0](https://agent-plugins.org/specification) standardizes skills
and MCP servers. It does not standardize hooks. Its closed root manifest has no
`hooks` field. The specification permits hooks only as a
[client extension](https://agent-plugins.org/plugin-authors/client-extensions),
but Codex 0.149.1 does not define or load such an extension.

Codex 0.149.1 selects an Agent Plugins root `plugin.json` before
`.codex-plugin/plugin.json`, marks the package as an Agent Plugin, and skips all
lifecycle hooks for that format. It also skips hooks from the Codex overlay.
See the [manifest selection code](https://github.com/openai/codex/blob/rust-v0.149.1/codex-rs/utils/plugins/src/plugin_namespace.rs#L42-L78)
and the [Codex 0.149.1 loader](https://github.com/openai/codex/blob/rust-v0.149.1/codex-rs/core-plugins/src/loader.rs#L942-L952).
The package therefore uses `.codex-plugin/plugin.json` and
`.claude-plugin/plugin.json`, with no root Agent Plugins manifest.

Codex discovers `hooks/hooks.json` through its
[default plugin path](https://learn.chatgpt.com/docs/hooks).

## Static test procedure

Run this command from the seek repository root:

```sh
make test-plugin
```

This test checks router decisions, fail-open paths, one emitted seek command,
and the absence of a root `plugin.json`. A copied plugin package must pass the
same script:

```sh
sh /path/to/copied/seek-router/test/run.sh
```

For a host check, install the package and start a new Codex session in a
repository that has no `.codex/hooks.json`. Open `/hooks`, confirm that
`seek-router` appears under `PreToolUse`, trust it, and then run a safe static
`rg` search. The transcript must show the `[seek router] Routed to:` notice.

## Tool scope

The plugin does not replace built-in `Grep` or `Glob` tools. A hook can change
arguments for one tool, but it cannot turn that call into a Bash call. Doing so
would require a denial path with different error and retry behavior.
