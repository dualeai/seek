#!/bin/sh
# Fixture tests for the seek router. Each case feeds a recorded hook payload to
# the router and asserts the decision it prints, plus exit code 0 on every path
# — including the malformed ones, because exit 2 reads as a deny.
set -u

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
ROUTER="$ROOT/bin/router.sh"

pass=0
fail=0

# Stub seek on PATH so `command -v seek` succeeds without a real install.
STUB=$(mktemp -d)
trap 'rm -rf "$STUB"' EXIT INT TERM
printf '#!/bin/sh\nexit 0\n' > "$STUB/seek"
chmod +x "$STUB/seek"
mkdir -p "$STUB/empty"
PATH="$STUB:$PATH"
export PATH

# check NAME PAYLOAD EXPECT
#   EXPECT "" means passthrough (no output).
#   Otherwise EXPECT is a substring the output must contain.
check() {
	name=$1
	payload=$2
	expect=$3

	out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
	code=$?

	if [ "$code" -ne 0 ]; then
		printf 'FAIL %s: exit %s, want 0\n' "$name" "$code"
		fail=$((fail + 1))
		return
	fi
	if [ -z "$expect" ]; then
		if [ -n "$out" ]; then
			printf 'FAIL %s: want passthrough, got: %s\n' "$name" "$out"
			fail=$((fail + 1))
			return
		fi
	else
		case $out in
		*"$expect"*) ;;
		*)
			printf 'FAIL %s:\n  want substring: %s\n  got:            %s\n' "$name" "$expect" "$out"
			fail=$((fail + 1))
			return
			;;
		esac
	fi
	pass=$((pass + 1))
}

bash_payload() {
	printf '{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"%s","description":"d"}}' "$1"
}

# --- rewrite path -----------------------------------------------------------

check "grep -rn" \
	"$(bash_payload 'grep -rn \"zebraQuux\" .')" \
	"seek -n 20 'content:zebraQuux'"

check "grep with --include chain" \
	"$(bash_payload 'grep -r \"zebraQuux\" --include=\"*.go\" --include=\"*.ts\" .')" \
	"seek -n 20 'content:zebraQuux'"

check "rg bare" \
	"$(bash_payload 'rg needleFunc')" \
	"seek -n 20 'content:needleFunc'"

check "git grep" \
	"$(bash_payload "git grep -n needleFunc -- '*'")" \
	"seek -n 20 'content:needleFunc'"

check "path operand kept" \
	"$(bash_payload 'grep -rn foo ./cmd')" \
	"seek -n 20 'content:foo' './cmd'"

check "grep -e pattern" \
	"$(bash_payload 'grep -rn -e needleFunc .')" \
	"seek -n 20 'content:needleFunc'"

check "definition lookup maps to sym" \
	"$(bash_payload 'grep -rn \"func zebraQuux\" .')" \
	"seek -n 20 -m 1 'sym:zebraQuux'"

check "decision is allow" \
	"$(bash_payload 'grep -rn foo .')" \
	'"permissionDecision":"allow"'

check "notice carries the bypass" \
	"$(bash_payload 'grep -rn foo .')" \
	'SEEK_ROUTER=off grep -rn'

check "notice says not exhaustive" \
	"$(bash_payload 'grep -rn foo .')" \
	'NOT exhaustive'

check "description copied into updatedInput" \
	"$(bash_payload 'grep -rn foo .')" \
	'"description":"d"'

check "timeout copied into updatedInput" \
	'{"tool_name":"Bash","tool_input":{"command":"grep -rn foo .","timeout":5000}}' \
	'"timeout":5000'

check "run_in_background copied into updatedInput" \
	'{"tool_name":"Bash","tool_input":{"command":"grep -rn foo .","run_in_background":false}}' \
	'"run_in_background":false'

# --- rewrite path: suffixes that do not change which files match ------------

check "trailing 2>/dev/null and head -N" \
	"$(bash_payload 'grep -r \"zebraQuux\" --include=\"*.go\" . 2>/dev/null | head -20')" \
	"seek -n 20 'content:zebraQuux'"

check "head -N below default tightens the cap" \
	"$(bash_payload 'grep -rn foo . | head -5')" \
	"seek -n 5 'content:foo'"

check "head -n 5 long form" \
	"$(bash_payload 'grep -rn foo . | head -n 5')" \
	"seek -n 5 'content:foo'"

check "head above default keeps the default" \
	"$(bash_payload 'grep -rn foo . | head -50')" \
	"seek -n 20 'content:foo'"

check "trailing 2>/dev/null alone" \
	"$(bash_payload 'grep -rn foo . 2>/dev/null')" \
	"seek -n 20 'content:foo'"

# --- passthrough: not a file search ----------------------------------------

check "pipe filter" "$(bash_payload 'ls | grep foo')" ""
check "pipeline tail" "$(bash_payload 'go test ./... | grep FAIL')" ""
check "compound command" "$(bash_payload 'grep -c func a.go && echo done')" ""
check "grep into pipe non-head" "$(bash_payload 'grep -rn foo . | sort')" ""
check "redirect" "$(bash_payload 'grep -rn foo . > out.txt')" ""
check "pipe to wc is not head" "$(bash_payload 'grep -rn foo . | wc -l')" ""
check "pipe to head then more" "$(bash_payload 'grep -rn foo . | head -5 | wc -l')" ""
check "head without count" "$(bash_payload 'grep -rn foo . | head')" ""
check "command substitution" "$(bash_payload 'grep -rn $(cat p.txt) .')" ""
check "not a searcher" "$(bash_payload 'ls -la')" ""
check "unmapped flag" "$(bash_payload 'grep -rn --binary-files=text foo .')" ""
check "count mode is unmapped" "$(bash_payload 'grep -c foo a.go')" ""
check "git non-grep" "$(bash_payload 'git status --short')" ""
check "already routed" "$(bash_payload "seek 'content:foo'")" ""
check "no pattern" "$(bash_payload 'grep -rn')" ""

# --- passthrough: kill switches --------------------------------------------

check "per-call bypass" "$(bash_payload 'SEEK_ROUTER=off grep -rn foo .')" ""

out=$(printf '%s' "$(bash_payload 'grep -rn foo .')" | SEEK_ROUTER=off sh "$ROUTER" 2>/dev/null)
if [ $? -eq 0 ] && [ -z "$out" ]; then
	pass=$((pass + 1))
else
	printf 'FAIL session kill switch: want silent passthrough, got: %s\n' "$out"
	fail=$((fail + 1))
fi

out=$(printf '%s' "$(bash_payload 'grep -rn foo .')" | PATH="$STUB/empty" /bin/sh "$ROUTER" 2>/dev/null)
if [ $? -eq 0 ] && [ -z "$out" ]; then
	pass=$((pass + 1))
else
	printf 'FAIL seek missing: want silent passthrough, got: %s\n' "$out"
	fail=$((fail + 1))
fi

# --- passthrough: other tools ----------------------------------------------

check "Grep tool not routed in v1" \
	'{"tool_name":"Grep","tool_input":{"pattern":"needleFunc"}}' ""
check "Glob tool not routed in v1" \
	'{"tool_name":"Glob","tool_input":{"pattern":"*.go"}}' ""
check "Read tool" \
	'{"tool_name":"Read","tool_input":{"file_path":"/tmp/a.go"}}' ""

# --- passthrough: malformed input ------------------------------------------

check "empty stdin" "" ""
check "not json" "hello world" ""
check "truncated json" '{"tool_name":"Bash","tool_input":{"command":"grep -rn foo' ""
check "missing tool_input" '{"tool_name":"Bash"}' ""
check "unknown escape" \
	'{"tool_name":"Bash","tool_input":{"command":"grep -rn \u0066oo ."}}' ""

# --- quoting -----------------------------------------------------------------
# A pattern that closes our quoting would turn a safe command into an
# injection, so each of these must either be quoted shut or passed through.

quote_case() {
	name=$1
	payload=$2
	out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
	code=$?
	if [ "$code" -ne 0 ]; then
		printf 'FAIL %s: exit %s, want 0\n' "$name" "$code"
		fail=$((fail + 1))
		return
	fi
	if [ -z "$out" ]; then
		pass=$((pass + 1))                     # passthrough is always safe
		return
	fi
	# Pull the rewritten command back out and confirm the shell parses it as a
	# single seek invocation rather than something with a second command in it.
	cmdline=$(printf '%s' "$out" | sed -n 's/.*"command":"\(.*\)","description.*/\1/p; s/.*"command":"\([^"]*\)".*/\1/p' | head -1)
	cmdline=$(printf '%s' "$cmdline" | sed 's/\\\\"/"/g; s/\\\\\\\\/\\\\/g')
	case $cmdline in
	"seek "*) ;;
	*)
		printf 'FAIL %s: rewritten command does not start with seek: %s\n' "$name" "$cmdline"
		fail=$((fail + 1))
		return
		;;
	esac
	printf '%s' "$cmdline" | grep -q '; *rm\|&& *rm\|`' && {
		printf 'FAIL %s: injection survived quoting: %s\n' "$name" "$cmdline"
		fail=$((fail + 1))
		return
	}
	pass=$((pass + 1))
}

quote_case "single quote in pattern"  "$(bash_payload "grep -rn \\\"it's\\\" .")"
quote_case "double quote in pattern"  '{"tool_name":"Bash","tool_input":{"command":"grep -rn \"say \\\"hi\\\"\" ."}}'
quote_case "semicolon in pattern"     "$(bash_payload 'grep -rn \"foo; rm -rf /\" .')"
quote_case "ampersands in pattern"    "$(bash_payload 'grep -rn \"foo && rm -rf /\" .')"
quote_case "backtick in pattern"      "$(bash_payload 'grep -rn \"foo\`id\`\" .')"
quote_case "dollar paren in pattern"  "$(bash_payload 'grep -rn \"foo\$(id)\" .')"
quote_case "backslash in pattern"     "$(bash_payload 'grep -rn \"foo\\\\\\\\bar\" .')"
quote_case "newline in pattern"       '{"tool_name":"Bash","tool_input":{"command":"grep -rn \"foo\\nbar\" ."}}'
# --- skill manifest ----------------------------------------------------------
# The skill is the portable half: it ships to harnesses with no hook support,
# so its frontmatter must stay parseable even when the router does not run.

if sh "$ROOT/test/check_skill.sh" "$ROOT/skills/seek-search/SKILL.md" 2>/dev/null; then
	pass=$((pass + 1))
else
	printf 'FAIL SKILL.md frontmatter\n'
	fail=$((fail + 1))
fi

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
