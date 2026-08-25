#!/bin/sh
# Plugin-owned router, wrapper, package, and public seek CLI checks.
set -u

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
ROUTER="$ROOT/bin/router.sh"
WORK=$(mktemp -d)
trap 'rm -r "$WORK"' EXIT HUP INT TERM

pass=0
fail=0

record_failure() {
	printf 'FAIL %s: %s\n' "$1" "$2"
	fail=$((fail + 1))
}

record_pass() {
	pass=$((pass + 1))
}

if ! command -v python3 >/dev/null 2>&1; then
	printf 'FAIL requirements: python3 is required for plugin tests\n'
	exit 1
fi
if ! command -v jq >/dev/null 2>&1 || ! command -v awk >/dev/null 2>&1; then
	printf 'FAIL requirements: jq and awk are required for plugin tests\n'
	exit 1
fi
if ! command -v seek >/dev/null 2>&1; then
	printf 'FAIL requirements: a current seek binary is required for plugin tests\n'
	exit 1
fi

bash_payload() {
	python3 -c 'import json,sys; print(json.dumps({"session_id":"s1","tool_name":"Bash","tool_input":{"command":sys.argv[1],"description":"café","timeout":5000,"run_in_background":False,"custom":{"keep":True}}}))' "$1"
}

check_silent() {
	silent_name=$1
	silent_payload=$2
	out=$(printf '%s' "$silent_payload" | sh "$ROUTER" 2>/dev/null)
	code=$?
	if [ "$code" -eq 0 ] && [ -z "$out" ]; then
		record_pass
	else
		record_failure "$silent_name" "exit $code, output [$out]"
	fi
}

if python3 "$ROOT/test/router_test.py"; then
	record_pass
else
	record_failure "router contract" "plugin-owned contract tests failed"
fi

# Use one observed file-list family to inspect the host response boundary.
payload=$(bash_payload "rg -l 'seek-router' . | head -n 2")
out=$(printf '%s' "$payload" | sh "$ROUTER" 2>/dev/null)
code=$?
if [ "$code" -eq 0 ] && printf '%s' "$out" | python3 -c '
import json, sys
d = json.load(sys.stdin)
h = d["hookSpecificOutput"]
assert set(h) == {"hookEventName", "permissionDecision", "updatedInput"}
assert h["hookEventName"] == "PreToolUse"
assert h["permissionDecision"] == "allow"
i = h["updatedInput"]
assert i["description"] == "café"
assert i["timeout"] == 5000
assert i["run_in_background"] is False
assert i["custom"] == {"keep": True}
assert i["command"].startswith("seek ")
' 2>/dev/null; then
	record_pass
else
	record_failure "wrapper rewrite" "exit $code, invalid response [$out]"
fi

check_emitted_command() {
	emitted_name=$1
	emitted_source=$2
	emitted_payload=$(bash_payload "$emitted_source")
	emitted_output=$(printf '%s' "$emitted_payload" | sh "$ROUTER" 2>/dev/null)
	emitted_code=$?
	emitted_command=$(
		printf '%s' "$emitted_output" |
			python3 -c 'import json,sys; print(json.load(sys.stdin)["hookSpecificOutput"]["updatedInput"]["command"])' \
				2>/dev/null || :
	)
	emitted_result=$(CDPATH='' cd -- "$ROOT" && sh -c "$emitted_command" 2>/dev/null)
	emitted_run_code=$?
	case $emitted_result in
	*"## "*) emitted_match=1 ;;
	*) emitted_match=0 ;;
	esac
	if [ "$emitted_code" -eq 0 ] && [ -n "$emitted_command" ] \
		&& [ "$emitted_run_code" -eq 0 ] && [ "$emitted_match" -eq 1 ]; then
		record_pass
	else
		record_failure "$emitted_name" \
			"hook $emitted_code, command [$emitted_command], run $emitted_run_code, output [$emitted_result]"
	fi
}

# Each adapter must emit a command that the normal public seek CLI can run.
check_emitted_command "grep seek execution" "grep -rn router ."
check_emitted_command "rg seek execution" "rg -l router . | head -n 2"
check_emitted_command "git grep seek execution" "git grep router -- ."
check_emitted_command "fd seek execution" "fd router ."
check_emitted_command "find seek execution" "find . -type f -name '*.awk'"
check_emitted_command "context seek execution" "rg -A 5 'Route supported' bin/router.sh"

check_silent "malformed payload" '{"tool_name":'
check_silent "ordinary command" "$(bash_payload 'echo unchanged')"

out=$(printf '%s' "$payload" | SEEK_ROUTER=off sh "$ROUTER" 2>/dev/null)
code=$?
if [ "$code" -eq 0 ] && [ -z "$out" ]; then
	record_pass
else
	record_failure "session bypass" "exit $code, output [$out]"
fi

mkdir -p "$WORK/empty"
out=$(printf '%s' "$payload" | PATH="$WORK/empty" /bin/sh "$ROUTER" 2>/dev/null)
code=$?
if [ "$code" -eq 0 ] && [ -z "$out" ]; then
	record_pass
else
	record_failure "missing seek" "exit $code, output [$out]"
fi
repo_root=$(CDPATH='' cd -- "$ROOT/../.." && pwd)
repo_hook=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["hooks"]["PreToolUse"][0]["hooks"][0]["command"])' "$repo_root/.codex/hooks.json")
out=$(CDPATH='' cd -- "$repo_root/cmd/seek" && printf '%s' "$payload" | sh -c "$repo_hook" 2>/dev/null)
code=$?
if [ "$code" -eq 0 ] && printf '%s' "$out" | python3 -c 'import json,sys; assert json.load(sys.stdin)["hookSpecificOutput"]["updatedInput"]["command"].startswith("seek ")' 2>/dev/null; then
	record_pass
else
	record_failure "repository hook from subdirectory" "exit $code, output [$out]"
fi

if python3 -c '
import json, os, sys
root = sys.argv[1]
repo = os.path.abspath(os.path.join(root, "..", ".."))
codex = json.load(open(os.path.join(root, ".codex-plugin", "plugin.json"), encoding="utf-8"))
claude = json.load(open(os.path.join(root, ".claude-plugin", "plugin.json"), encoding="utf-8"))
assert codex["name"] == "seek-router"
assert codex["version"] == claude["version"] == "0.3.0"
assert codex["skills"] == "./skills/"
for manifest in (codex, claude):
    text = manifest["description"]
    for name in ("grep", "rg", "git grep", "fd", "find"):
        assert name in text
for path in (os.path.join(root, "hooks", "hooks.json"), os.path.join(repo, ".codex", "hooks.json")):
    data = json.load(open(path, encoding="utf-8"))
    hook = data["hooks"]["PreToolUse"][0]["hooks"][0]
    assert hook["type"] == "command"
    assert hook["timeout"] == 5
    assert "statusMessage" not in hook
market = json.load(open(os.path.join(repo, ".claude-plugin", "marketplace.json"), encoding="utf-8"))
assert market["plugins"][0]["name"] == "seek-router"
assert not os.path.lexists(os.path.join(root, "plugin.json"))
' "$ROOT" 2>/dev/null; then
	record_pass
else
	record_failure "package metadata" "manifest or hook contract is invalid"
fi

if sh "$ROOT/test/check_skill.sh" "$ROOT/skills/seek-search/SKILL.md" 2>/dev/null; then
	record_pass
else
	record_failure "skill frontmatter" "invalid skill metadata"
fi

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
