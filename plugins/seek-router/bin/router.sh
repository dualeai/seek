#!/bin/sh
# Route supported static search commands without adding hook code to seek.
# Every unsupported or malformed input falls through to the original command.
set -u

if [ "${SEEK_ROUTER:-}" = "off" ]; then
	exit 0
fi

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd) || exit 0
ROUTER_AWK="$ROOT/lib/router.awk"

if [ ! -r "$ROUTER_AWK" ] \
	|| ! command -v seek >/dev/null 2>&1 \
	|| ! command -v jq >/dev/null 2>&1 \
	|| ! command -v awk >/dev/null 2>&1; then
	exit 0
fi

payload=$(cat) || exit 0
command=$(
	printf '%s' "$payload" |
		jq -er '
			select(.tool_name == "Bash")
			| select((.tool_input | type) == "object")
			| .tool_input.command
			| select(type == "string" and length > 0)
		' 2>/dev/null
) || exit 0

route=$(printf '%s' "$command" | LC_ALL=C awk -f "$ROUTER_AWK" 2>/dev/null) || exit 0
[ -n "$route" ] || exit 0
context_kind=${route%%|*}
new_command=${route#*|}
[ "$new_command" != "$route" ] && [ -n "$new_command" ] || exit 0

# Context flags are a public seek feature. An older seek must keep the original
# rg command instead of receiving a flag it does not know.
case $context_kind in
A | C)
	help=$(seek --help 2>/dev/null) || exit 0
	;;
esac

case $context_kind in
A)
	case $help in *"--after-context"*) ;; *) exit 0 ;; esac
	;;
C)
	case $help in *"--context"*) ;; *) exit 0 ;; esac
	;;
none) ;;
*) exit 0 ;;
esac

output=$(
	printf '%s' "$payload" |
		jq -cer --arg command "$new_command" '
			.tool_input.command = $command
			| {
				hookSpecificOutput: {
					hookEventName: "PreToolUse",
					permissionDecision: "allow",
					updatedInput: .tool_input
				}
			}
		' 2>/dev/null
) || exit 0

printf '%s\n' "$output"
exit 0
