#!/bin/sh
# Contract tests for the conservative seek router.
# shellcheck disable=SC2016
# Fixture commands contain shell syntax that the test must not expand.
set -u

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
ROUTER="$ROOT/bin/router.sh"
ORIGINAL_PATH=$PATH
REAL_SEEK=$(command -v seek 2>/dev/null || :)

pass=0
fail=0

STUB=$(mktemp -d)
trap 'rm -rf "$STUB"' EXIT INT TERM
printf '#!/bin/sh\nexit 0\n' >"$STUB/seek"
chmod +x "$STUB/seek"
mkdir -p "$STUB/empty"
PATH="$STUB:$PATH"
export PATH

bash_payload() {
	jq -cn --arg command "$1" \
		'{session_id:"s1",tool_name:"Bash",tool_input:{command:$command,description:"d"}}'
}

record_failure() {
	printf 'FAIL %s: %s\n' "$1" "$2"
	fail=$((fail + 1))
}

check_route() {
	name=$1
	source_command=$2
	expected=$3
	payload=$(bash_payload "$source_command")
	out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
	code=$?
	if [ "$code" -ne 0 ]; then
		record_failure "$name" "exit $code, want 0"
		return
	fi
	got=$(printf '%s' "$out" | jq -er '.hookSpecificOutput.updatedInput.command' 2>/dev/null || :)
	if [ "$got" != "$expected" ]; then
		record_failure "$name" "command [$got], want [$expected]"
		return
	fi
	pass=$((pass + 1))
}

check_payload_route() {
	name=$1
	payload=$2
	expected=$3
	out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
	code=$?
	if [ "$code" -ne 0 ]; then
		record_failure "$name" "exit $code, want 0"
		return
	fi
	got=$(printf '%s' "$out" | jq -er '.hookSpecificOutput.updatedInput.command' 2>/dev/null || :)
	if [ "$got" != "$expected" ]; then
		record_failure "$name" "command [$got], want [$expected]"
		return
	fi
	pass=$((pass + 1))
}

check_passthrough() {
	name=$1
	source_command=$2
	payload=$(bash_payload "$source_command")
	out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
	code=$?
	if [ "$code" -ne 0 ]; then
		record_failure "$name" "exit $code, want 0"
	elif [ -n "$out" ]; then
		record_failure "$name" "want no decision, got [$out]"
	else
		pass=$((pass + 1))
	fi
}

check_payload_passthrough() {
	name=$1
	payload=$2
	out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
	code=$?
	if [ "$code" -ne 0 ]; then
		record_failure "$name" "exit $code, want 0"
	elif [ -n "$out" ]; then
		record_failure "$name" "want no decision, got [$out]"
	else
		pass=$((pass + 1))
	fi
}

# Accepted commands: static atoms, literal paths, and tool-specific short flags.
check_route "grep recursive" \
	'grep -rn "zebraQuux" .' \
	"seek -n 20 -m 3 'case:yes content:zebraQuux' '.'"

check_route "grep two paths" \
	'grep -rn needleFunc ./cmd ./plugins' \
	"seek -n 20 -m 3 'case:yes content:needleFunc' './cmd' './plugins'"

check_route "grep safe flag bundle" \
	'grep -rniHIF Foo ./cmd' \
	"seek -n 20 -m 3 'case:no content:Foo' './cmd'"

check_route "grep neutral ERE atom" \
	'grep -rE Runner ./cmd' \
	"seek -n 20 -m 3 'case:yes content:Runner' './cmd'"

check_route "rg current directory" \
	'rg needleFunc' \
	"seek -n 20 -m 3 'case:yes content:needleFunc' '.'"

check_route "rg safe flag bundle" \
	'rg -niHIF Foo ./cmd' \
	"seek -n 20 -m 3 'case:no content:Foo' './cmd'"

check_payload_route "valid JSON Unicode escape" \
	'{"tool_name":"Bash","tool_input":{"command":"grep -rn \u0066oo ."}}' \
	"seek -n 20 -m 3 'case:yes content:foo' '.'"

# The hook must preserve every Bash input field because updatedInput replaces
# the complete object.
payload=$(jq -cn '{
	tool_name:"Bash",
	tool_input:{
		command:"grep -rn Foo .",
		description:"café",
		timeout:5000,
		run_in_background:false,
		custom:{keep:true}
	}
}')
out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
if printf '%s' "$out" | jq -e '
	.hookSpecificOutput.updatedInput
	| .description == "café"
	  and .timeout == 5000
	  and .run_in_background == false
	  and .custom.keep == true
' >/dev/null 2>&1; then
	pass=$((pass + 1))
else
	record_failure "preserve complete tool input" "field changed or missing"
fi

if printf '%s' "$out" | jq -e '
	.hookSpecificOutput.additionalContext
	| contains("seek directly") and contains("SEEK_ROUTER=off")
' >/dev/null 2>&1; then
	pass=$((pass + 1))
else
	record_failure "notice teaches direct use and bypass" "text missing"
fi

# Execute one emitted command with the real seek binary. String comparison
# alone cannot prove that the generated query parses and returns a real match.
if [ -n "$REAL_SEEK" ]; then
	payload=$(bash_payload 'grep -rn normalizeLineMatches ./cmd/seek')
	out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
	got=$(printf '%s' "$out" | jq -er '.hookSpecificOutput.updatedInput.command' 2>/dev/null || :)
	REAL_SEEK_DIR=$(dirname "$REAL_SEEK")
	result=$(PATH="$REAL_SEEK_DIR:$ORIGINAL_PATH" sh -c "$got" 2>/dev/null)
	code=$?
	case $result in
	*normalizeLineMatches*) matched=1 ;;
	*) matched=0 ;;
	esac
	if [ "$code" -eq 0 ] && [ "$matched" -eq 1 ]; then
		pass=$((pass + 1))
	else
		record_failure "emitted command runs in seek" "exit $code, output [$result]"
	fi

	# A pathless rg starts in the shell's current directory. Without the added
	# dot, seek would widen this probe to the worktree and find seek-router.
	if [ -d "$ROOT/../../cmd/seek" ]; then
		payload=$(bash_payload 'rg seek-router')
		out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
		got=$(printf '%s' "$out" | jq -er '.hookSpecificOutput.updatedInput.command' 2>/dev/null || :)
		result=$(
			CDPATH='' cd -- "$ROOT/../../cmd/seek" &&
				PATH="$REAL_SEEK_DIR:$ORIGINAL_PATH" sh -c "$got" 2>/dev/null
		)
		code=$?
		if [ "$code" -eq 1 ] && [ -z "$result" ]; then
			pass=$((pass + 1))
		else
			record_failure "pathless rg stays in current directory" "exit $code, output [$result]"
		fi
	fi
fi

# Shell syntax and dynamic expansion stay with the shell.
check_passthrough "glob path" 'grep -rn foo *'
check_passthrough "variable pattern" 'grep -rn "$PATTERN" .'
check_passthrough "variable path" 'grep -rn foo "$SCOPE"'
check_passthrough "tilde path" 'grep -rn foo ~/src'
check_passthrough "brace expansion" 'grep -rn foo {src,test}'
check_passthrough "shell comment" 'grep -rn foo . # locate it'
check_passthrough "pipeline" 'grep -rn foo . | head -5'
check_passthrough "redirect" 'grep -rn foo . 2>/dev/null'
check_passthrough "command substitution" 'grep -rn "$(cat pattern)" .'
check_passthrough "backslash escape" 'grep -rn foo\ bar .'
check_passthrough "semicolon" 'grep -rn foo .; echo done'
check_passthrough "ampersand" 'grep -rn foo . && echo done'
check_passthrough "newline" 'grep -rn foo .
echo done'

# Each CLI gets its own small flag grammar.
check_passthrough "git grep" 'git grep -n foo'
check_passthrough "egrep" 'egrep -rn foo .'
check_passthrough "fgrep" 'fgrep -rn foo .'
check_passthrough "ag" 'ag foo .'
check_passthrough "ack" 'ack foo .'
check_passthrough "grep reads stdin" 'grep foo'
check_passthrough "grep nonrecursive directory" 'grep -n foo .'
check_passthrough "grep follows symlinks" 'grep -Rn foo .'
check_passthrough "grep include filter" 'grep -rn foo --include=*.go .'
check_passthrough "grep exclude filter" 'grep -rn foo --exclude=*.go .'
check_passthrough "grep word mode" 'grep -rw foo .'
check_passthrough "grep filenames only" 'grep -rl foo .'
check_passthrough "grep count" 'grep -rc foo .'
check_passthrough "grep pattern flag" 'grep -rn -e foo .'
check_passthrough "grep long flag" 'grep --recursive foo .'
check_passthrough "rg encoding argument" 'rg -E utf-8 foo .'
check_passthrough "rg replacement argument" 'rg -r replacement foo .'
check_passthrough "rg help flag" 'rg -h foo .'
check_passthrough "rg PCRE mode" 'rg -P foo .'
check_passthrough "rg long flag" 'rg --ignore-case foo .'

# Only regex-neutral ASCII atoms can enter seek query grammar.
check_passthrough "empty pattern" 'grep -rn "" .'
check_passthrough "phrase pattern" 'grep -rn "foo or bar" .'
check_passthrough "query filter text" 'grep -rn "foo:bar" .'
check_passthrough "dot regex" 'grep -rn "foo.bar" .'
check_passthrough "alternation" 'rg "foo|bar" .'
check_passthrough "parentheses" 'rg "foo(bar)" .'
check_passthrough "bracket class" 'grep -rn "[a-z]" .'
check_passthrough "BRE escape" 'grep -rn "a\(b\)" .'
check_passthrough "non-ASCII pattern" 'grep -rn café .'
check_passthrough "space in path" 'grep -rn foo "path with space"'
check_passthrough "path starting dash" 'grep -rn foo -src'

# Non-search commands and malformed payloads always fail open.
check_passthrough "ordinary Bash" 'echo café'
check_payload_passthrough "Grep tool" \
	'{"tool_name":"Grep","tool_input":{"pattern":"foo"}}'
check_payload_passthrough "metadata command cannot shadow tool input" \
	'{"tool_name":"Bash","metadata":{"command":"grep -rn wrong ."},"tool_input":{"command":"echo safe"}}'
check_payload_passthrough "empty input" ''
check_payload_passthrough "not JSON" 'hello'
check_payload_passthrough "truncated JSON" \
	'{"tool_name":"Bash","tool_input":{"command":"grep -rn foo ."}'
check_payload_passthrough "malformed JSON separator" \
	'{"tool_name" "Bash","tool_input":{"command":"grep -rn foo ."}}'
check_payload_passthrough "missing tool input" '{"tool_name":"Bash"}'

payload=$(bash_payload 'SEEK_ROUTER=off grep -rn foo .')
out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
code=$?
if [ "$code" -eq 0 ] && [ -z "$out" ]; then
	pass=$((pass + 1))
else
	record_failure "per-call bypass" "router returned [$out]"
fi

payload=$(bash_payload 'grep -rn foo .')
out=$(printf '%s' "$payload" | SEEK_ROUTER=off sh "$ROUTER" 2>/dev/null)
code=$?
if [ "$code" -eq 0 ] && [ -z "$out" ]; then
	pass=$((pass + 1))
else
	record_failure "session bypass" "router returned [$out]"
fi

out=$(printf '%s' "$payload" | PATH="$STUB/empty" /bin/sh "$ROUTER" 2>/dev/null)
code=$?
if [ "$code" -eq 0 ] && [ -z "$out" ]; then
	pass=$((pass + 1))
else
	record_failure "missing seek" "router returned [$out]"
fi

out=$(printf '%s' "$payload" | PATH="$STUB" /bin/sh "$ROUTER" 2>/dev/null)
code=$?
if [ "$code" -eq 0 ] && [ -z "$out" ]; then
	pass=$((pass + 1))
else
	record_failure "missing jq" "router returned [$out]"
fi

# Plugin-local checks must also pass after a harness copies only this directory
# into its cache.
for manifest in \
	"$ROOT/.claude-plugin/plugin.json" \
	"$ROOT/.codex-plugin/plugin.json" \
	"$ROOT/plugin.json" \
	"$ROOT/hooks/hooks.json"
do
	if sh "$ROOT/test/check_json.sh" "$manifest" 2>/dev/null; then
		pass=$((pass + 1))
	else
		record_failure "manifest JSON" "$manifest"
	fi
done

# Agent Plugins 1.0 has a closed manifest schema. Skills use the fixed skills/
# location and must not appear as a top-level manifest field.
if jq -e '
	.["$schema"] == "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	and (.name | type) == "string"
	and ((keys - [
		"$schema", "name", "version", "description", "author", "homepage",
		"repository", "license", "keywords", "extensions"
	]) | length == 0)
' "$ROOT/plugin.json" >/dev/null 2>&1; then
	pass=$((pass + 1))
else
	record_failure "Agent Plugins manifest schema" "unsupported top-level field"
fi

plugin_version=$(jq -er '.version' "$ROOT/plugin.json" 2>/dev/null || :)
if [ -n "$plugin_version" ] && jq -e --arg version "$plugin_version" '
	.name == "seek-router"
	and .version == $version
	and .skills == "./skills/"
' "$ROOT/.codex-plugin/plugin.json" >/dev/null 2>&1 \
	&& jq -e --arg version "$plugin_version" '.version == $version' \
		"$ROOT/.claude-plugin/plugin.json" >/dev/null 2>&1; then
	pass=$((pass + 1))
else
	record_failure "Codex plugin manifest" "unsupported or missing field"
fi

if sh "$ROOT/test/check_skill.sh" "$ROOT/skills/seek-search/SKILL.md" 2>/dev/null; then
	pass=$((pass + 1))
else
	record_failure "skill manifest" "invalid frontmatter"
fi

REPO_ROOT=$(CDPATH='' cd -- "$ROOT/../.." && pwd)
for manifest in \
	"$REPO_ROOT/.claude-plugin/marketplace.json" \
	"$REPO_ROOT/.codex/hooks.json"
do
	if [ ! -f "$manifest" ]; then
		continue
	fi
	if sh "$ROOT/test/check_json.sh" "$manifest" 2>/dev/null; then
		pass=$((pass + 1))
	else
		record_failure "repository manifest JSON" "$manifest"
	fi
done

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
