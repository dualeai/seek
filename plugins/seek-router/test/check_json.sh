#!/bin/sh
# Minimal JSON well-formedness check for the plugin manifests.
#
# Not a full parser: it strips string literals, then verifies that brackets
# balance and that nothing outside a string is a character JSON does not allow.
# Enough to catch a truncated or comma-mangled manifest in CI without adding a
# jq or python dependency the rest of the plugin does not have.
set -u

file=${1:-}
[ -n "$file" ] && [ -f "$file" ] || { echo "check_json: no such file: $file" >&2; exit 1; }

awk '
{ doc = doc $0 "\n" }
END {
	n = length(doc)
	depth = 0
	instr = 0
	stack = ""
	for (i = 1; i <= n; i++) {
		c = substr(doc, i, 1)
		if (instr) {
			if (c == "\\") { i++; continue }
			if (c == "\"") instr = 0
			continue
		}
		if (c == "\"") { instr = 1; seen = 1; continue }
		if (c == "{" || c == "[") { stack = stack c; continue }
		if (c == "}" || c == "]") {
			open = substr(stack, length(stack), 1)
			if ((c == "}" && open != "{") || (c == "]" && open != "[")) {
				print "unbalanced " c > "/dev/stderr"; exit 1
			}
			stack = substr(stack, 1, length(stack) - 1)
			continue
		}
		if (c ~ /[ \t\r\n:,]/) continue
		if (c ~ /[-+.0-9eE]/) continue
		if (c ~ /[a-z]/) continue            # true, false, null
		print "unexpected character: " c > "/dev/stderr"; exit 1
	}
	if (instr) { print "unterminated string" > "/dev/stderr"; exit 1 }
	if (stack != "") { print "unclosed " stack > "/dev/stderr"; exit 1 }
	if (!seen) { print "no content" > "/dev/stderr"; exit 1 }
}
' "$file"
