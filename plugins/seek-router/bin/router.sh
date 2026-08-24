#!/bin/sh
# Route only static grep and rg searches whose meaning seek can preserve.
# Every unsupported or malformed input falls through to the original command.
set -u

if [ "${SEEK_ROUTER:-}" = "off" ]; then
	exit 0
fi

# jq provides strict JSON parsing and preserves the complete Bash input object.
# Missing dependencies are a normal fail-open path.
if ! command -v seek >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
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

# Use the C locale so byte-wise scans cannot fail on a UTF-8 continuation byte.
new_command=$(
	printf '%s\n' "$command" | LC_ALL=C awk '
function tokenize(s, argv,   i, c, cur, quote, started) {
	nargv = 0
	cur = ""
	quote = ""
	started = 0
	for (i = 1; i <= length(s); i++) {
		c = substr(s, i, 1)
		if (quote != "") {
			if (c == quote) {
				quote = ""
			} else {
				cur = cur c
				started = 1
			}
			continue
		}
		if (c == "\"" || c == sq) {
			quote = c
			started = 1
			continue
		}
		if (c == " " || c == "\t") {
			if (started) {
				argv[++nargv] = cur
				cur = ""
				started = 0
			}
			continue
		}
		cur = cur c
		started = 1
	}
	if (quote != "") return 0
	if (started) argv[++nargv] = cur
	return nargv
}

function has_shell_syntax(s,   i, unsafe) {
	unsafe = "\\$`#~*?[]{}()|&;<>"
	for (i = 1; i <= length(s); i++) {
		if (index(unsafe, substr(s, i, 1)) > 0) return 1
	}
	return index(s, "\n") > 0 || index(s, "\r") > 0
}

function grep_flags(t,   i, c) {
	if (t !~ /^-[A-Za-z]+$/) return 0
	for (i = 2; i <= length(t); i++) {
		c = substr(t, i, 1)
		if (c == "r") recursive = 1
		else if (c == "i") insensitive = 1
		else if (index("nHIFE", c) == 0) return 0
	}
	return 1
}

function rg_flags(t,   i, c) {
	if (t !~ /^-[A-Za-z]+$/) return 0
	for (i = 2; i <= length(t); i++) {
		c = substr(t, i, 1)
		if (c == "i") insensitive = 1
		else if (index("nHIF", c) == 0) return 0
	}
	return 1
}

function safe_pattern(v) {
	return v ~ /^[A-Za-z0-9_\/-]+$/
}

function safe_path(v) {
	return v != "" && substr(v, 1, 1) != "-" && v ~ /^[A-Za-z0-9_.\/-]+$/
}

function shell_quote(v) {
	return sq v sq
}

BEGIN {
	sq = sprintf("%c", 39)
	cmd = ""
}
{
	if (NR > 1) cmd = cmd "\n"
	cmd = cmd $0
}
END {
	if (cmd ~ /^[ \t]*SEEK_ROUTER=off[ \t]/ || has_shell_syntax(cmd)) exit 0
	if (!tokenize(cmd, argv) || nargv < 2) exit 0

	tool = argv[1]
	if (tool != "grep" && tool != "rg") exit 0

	recursive = 0
	insensitive = 0
	has_pattern = 0
	npaths = 0
	for (i = 2; i <= nargv; i++) {
		t = argv[i]
		if (!has_pattern && substr(t, 1, 1) == "-") {
			if (tool == "grep") {
				if (!grep_flags(t)) exit 0
			} else if (!rg_flags(t)) {
				exit 0
			}
			continue
		}
		if (!has_pattern) {
			pattern = t
			has_pattern = 1
			continue
		}
		if (!safe_path(t)) exit 0
		paths[++npaths] = t
	}

	if (!has_pattern || !safe_pattern(pattern)) exit 0
	if (tool == "grep" && (!recursive || npaths == 0)) exit 0
	if (tool == "rg" && npaths == 0) paths[++npaths] = "."

	case_filter = insensitive ? "case:no " : "case:yes "
	result = "seek -n 20 -m 3 " shell_quote(case_filter "content:" pattern)
	for (i = 1; i <= npaths; i++) result = result " " shell_quote(paths[i])
	print result
}
'
) || exit 0

if [ -z "$new_command" ]; then
	exit 0
fi

notice="[seek router] Routed to: $new_command. Ranked and capped, not exhaustive. Use seek directly next time, or set SEEK_ROUTER=off for exact grep results."
output=$(
	printf '%s' "$payload" |
		jq -cer --arg command "$new_command" --arg notice "$notice" '
			.tool_input.command = $command
			| {
				hookSpecificOutput: {
					hookEventName: "PreToolUse",
					permissionDecision: "allow",
					permissionDecisionReason: "routed to seek",
					updatedInput: .tool_input,
					additionalContext: $notice
				}
			}
		' 2>/dev/null
) || exit 0

printf '%s\n' "$output"
exit 0
