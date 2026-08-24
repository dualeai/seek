#!/bin/sh
# Assert the skill frontmatter a harness needs is present and parseable.
# The skill ships to harnesses with no hook support, so it has to stand on its
# own even when the router never runs.
set -u

file=${1:-}
[ -n "$file" ] && [ -f "$file" ] || { echo "check_skill: no such file: $file" >&2; exit 1; }

awk '
NR == 1 && $0 != "---" { print "missing opening ---" > "/dev/stderr"; exit 1 }
NR > 1 && $0 == "---" { closed = 1; exit }
NR > 1 && /^name:[ \t]*seek-search[ \t]*$/ { name = 1 }
NR > 1 && /^description:[ \t]*./ {
	sub(/^description:[ \t]*/, "")
	if (length($0) > 40) desc = 1
}
END {
	if (!closed) { print "frontmatter not closed" > "/dev/stderr"; exit 1 }
	if (!name)   { print "bad or missing name" > "/dev/stderr"; exit 1 }
	if (!desc)   { print "missing or thin description" > "/dev/stderr"; exit 1 }
}
' "$file"
