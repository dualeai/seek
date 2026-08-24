# seek-router

A PreToolUse hook that turns an agent's `grep` / `rg` / `git grep` calls into
[seek](https://github.com/dualeai/seek) searches, plus the `seek-search` skill
that teaches the query syntax.

Install and usage: see the Agent Integration section of the [top-level
README](../../README.md).

## How it decides

`bin/router.sh` reads one hook payload on stdin and prints at most one decision
on stdout. There are exactly two outcomes:

| Outcome | When |
| --- | --- |
| `allow` + `updatedInput` | the whole command is a plain file search it can translate |
| nothing at all | everything else — the original command runs untouched |

It rewrites rather than denies. A denial makes the model produce a replacement
command, and a hook that keeps rejecting the same command can put the model in
a retry loop; rewriting sidesteps that, and the tool result is not marked as an
error.

The notice rides along in `additionalContext`, so the rewritten command stays a
single `seek` invocation with one interpolated value. That value is
shell-quoted: a pattern containing a quote must not be able to close ours and
leave the rest running as shell.

## One hook entry, not six

The package ships one unconditional entry under the `Bash` matcher.

Claude Code can narrow an entry with an `if` condition, and an earlier version
used six, one per search command, to keep the router from spawning on shell
calls that are not searches. Codex has no `if` field, and its matcher group
does not reject unknown keys, so all six loaded unconditionally and ran the
router six times per Bash call.

The matcher is the only gate both harnesses read. Re-adding `if` buys Claude
Code a few skipped spawns and costs Codex one spawn per entry; `router.sh`
already exits silently on anything it cannot translate.

## Always exit 0

Claude Code reads exit code 2 as a deny and shows the hook's stderr to the
model as the reason. A crashing router would therefore look like a deliberate
block and break search instead of falling back to grep. Every path exits 0,
including malformed input, and `make test-plugin` asserts it on every fixture.

## Layout

```
.claude-plugin/plugin.json   Claude Code + Codex manifest
plugin.json                  Agent Plugins 1.0.0 manifest (skill only)
hooks/hooks.json             the PreToolUse matcher
bin/router.sh                the router
skills/seek-search/SKILL.md  query syntax, ships everywhere
test/run.sh                  fixture suite, run by make test-plugin
```

## Not built: routing the Grep and Glob tools

Claude Code also exposes `Grep` and `Glob` tools, which this plugin leaves
alone. `updatedInput` can change a tool's arguments but not which tool runs, so
routing those means denying the call and returning seek's output in the deny
reason. That works — the model reads results out of a denial and answers from
them — but it costs considerably more than the Bash path:

- a denial can loop. Nothing caps consecutive PreToolUse blocks the way the
  `Stop` hook's limit does, so the router would need its own circuit breaker
  keyed per session and subagent.
- seek would run inside the hook rather than in the agent's shell, which puts
  index-build time on the hook's timeout instead of in plain sight.
- `Grep`'s `output_mode`, `head_limit` and context flags would each need a
  faithful mapping, and several have none — `count` in particular.

Whether it is worth building depends on how often the model reaches for the
`Grep` tool rather than shell `grep` in normal permission mode, and on whether
routing improves an answer rather than merely producing one. Judging that needs
end-to-end tasks scored on output quality, not a token count.
