#!/bin/sh
# seek router — PreToolUse hook for Claude Code and Codex.
#
# Reads one hook payload as JSON on stdin. When the payload is a plain shell
# search (grep / egrep / rg / ag / ack / git grep), it answers with an "allow"
# decision plus an updatedInput that replaces the command with the seek
# equivalent. Everything else produces no output at all, which both harnesses
# read as "hook had no opinion" and run the original command.
#
# Contract, in order of importance:
#
#  1. ALWAYS exit 0. Claude Code treats exit code 2 as a deny and shows the
#     hook's stderr to the model as the denial reason, so a crash here would
#     read as a deliberate block and break search instead of degrading to grep.
#  2. Any doubt -> passthrough. Unknown tool, unparsable JSON, an unmapped
#     flag, a pattern that cannot be quoted safely: print nothing, exit 0.
#  3. Never invent results. This hook does not run seek; it rewrites the
#     command so seek runs in the agent's own shell, where its cost and its
#     output are visible exactly like any other command.
#
# No jq: the payload shape we need is three fields deep and awk is everywhere
# jq is not. Anything the parser does not understand falls out as passthrough.
set -u

# Session-wide off switch, checked before any work.
if [ "${SEEK_ROUTER:-}" = "off" ]; then
	exit 0
fi

# A router that cannot reach seek must not rewrite anything.
if ! command -v seek >/dev/null 2>&1; then
	exit 0
fi

exec awk '
# ---------------------------------------------------------------- JSON input

# Decode the JSON string that starts at position p (the opening quote) in s.
# Returns the decoded text; sets ok=0 in the caller-visible global when the
# string is unterminated or uses an escape we do not decode.
function json_string(s, p,   out, c, e) {
	if (substr(s, p, 1) != "\"") { ok = 0; return "" }
	p++
	out = ""
	while (p <= length(s)) {
		c = substr(s, p, 1)
		if (c == "\"") { endpos = p; return out }
		if (c == "\\") {
			e = substr(s, p + 1, 1)
			if (e == "n") out = out "\n"
			else if (e == "t") out = out "\t"
			else if (e == "r") out = out "\r"
			else if (e == "b" || e == "f") out = out " "
			else if (e == "\"" || e == "\\" || e == "/") out = out e
			else { ok = 0; return "" }   # \uXXXX and friends: not our business
			p += 2
			continue
		}
		out = out c
		p++
	}
	ok = 0
	return ""
}

# Value of a top-level-ish string key. Good enough because the keys we read
# ("tool_name", "command", "description") are unique in a hook payload; a
# false positive still lands in the passthrough checks below.
function json_str_value(s, key,   p) {
	p = index(s, "\"" key "\"")
	if (p == 0) return ""
	p += length(key) + 2
	while (p <= length(s) && substr(s, p, 1) ~ /[ \t\r\n:]/) p++
	return json_string(s, p)
}

# Raw (undecoded) scalar for a non-string key, e.g. a number or true/false.
function json_raw_value(s, key,   p, out, c) {
	p = index(s, "\"" key "\"")
	if (p == 0) return ""
	p += length(key) + 2
	while (p <= length(s) && substr(s, p, 1) ~ /[ \t\r\n:]/) p++
	out = ""
	while (p <= length(s)) {
		c = substr(s, p, 1)
		if (c ~ /[,}\]]/) break
		out = out c
		p++
	}
	gsub(/[ \t\r\n]+$/, "", out)
	if (out !~ /^(true|false|-?[0-9]+(\.[0-9]+)?)$/) return ""
	return out
}

# --------------------------------------------------------------- JSON output

function json_escape(v,   out, i, c, n) {
	out = ""
	n = length(v)
	for (i = 1; i <= n; i++) {
		c = substr(v, i, 1)
		if (c == "\"") out = out "\\\""
		else if (c == "\\") out = out "\\\\"
		else if (c == "\n") out = out "\\n"
		else if (c == "\t") out = out "\\t"
		else if (c == "\r") out = out "\\r"
		else out = out c
	}
	return out
}

# ------------------------------------------------------------ shell quoting

# Single-quote v for /bin/sh. The only interpolation point in the rewritten
# command, and the one place where getting it wrong turns a safe command into
# an injection: a bare pattern containing a quote would otherwise close ours
# and leave the remainder running as shell.
function shq(v,   out, i, c, n) {
	out = "'"'"'"
	n = length(v)
	for (i = 1; i <= n; i++) {
		c = substr(v, i, 1)
		if (c == "'"'"'") out = out "'"'"'\\'"'"''"'"'"
		else out = out c
	}
	return out "'"'"'"
}

# ------------------------------------------------------------- tokenisation

# Split a command line into argv, honouring single and double quotes. Sets
# ok=0 on an unbalanced quote so the caller passes through rather than
# guessing at what the user meant.
function tokenize(cmd, argv,   i, n, c, cur, started, q) {
	n = length(cmd)
	nargv = 0
	cur = ""
	started = 0
	q = ""
	for (i = 1; i <= n; i++) {
		c = substr(cmd, i, 1)
		if (q != "") {
			if (c == q) { q = "" ; continue }
			cur = cur c
			started = 1
			continue
		}
		if (c == "\"" || c == "'"'"'") { q = c; started = 1; continue }
		if (c ~ /[ \t]/) {
			if (started) { argv[++nargv] = cur; cur = ""; started = 0 }
			continue
		}
		cur = cur c
		started = 1
	}
	if (q != "") { ok = 0; return 0 }
	if (started) argv[++nargv] = cur
	return nargv
}

# Flags we can honour by ignoring them: they change grep output shape, not
# which files match. An unmapped flag means we do not understand the command
# well enough to rewrite it.
function skippable_flag(t) {
	if (t ~ /^-[rRnNhHiIwlEF]+$/) return 1
	if (t ~ /^--(recursive|line-number|ignore-case|word-regexp|with-filename|no-filename|files-with-matches|extended-regexp|fixed-strings)$/) return 1
	if (t ~ /^--(include|exclude|exclude-dir|color|colour)=/) return 1
	return 0
}

BEGIN { ok = 1; payload = "" }
{ payload = payload $0 "\n" }

END {
	if (!ok) exit 0

	tool = json_str_value(payload, "tool_name")
	if (!ok || tool != "Bash") exit 0          # v1 routes shell searches only

	cmd = json_str_value(payload, "command")
	if (!ok || cmd == "") exit 0

	# Per-call bypass. The Bash matcher is the only gate the hook gets, so it
	# runs for a bypassed command too and has to recognise the prefix itself.
	if (cmd ~ /^[ \t]*SEEK_ROUTER=off[ \t]/) exit 0

	# Two suffixes are worth peeling off before the compound-command guard,
	# because neither changes WHICH files match and together they are the
	# shape the model reaches for most: a recursive grep that silences walk
	# errors and truncates output. Observed live:
	#   grep -r "X" --include="*.go" ... . 2>/dev/null | head -20
	# Refusing to route that leaves the common case on grep.
	headlimit = 0
	if (match(cmd, /[ \t]*\|[ \t]*head[ \t]+(-n[ \t]*|-)[0-9]+[ \t]*$/)) {
		tail = substr(cmd, RSTART, RLENGTH)
		cmd = substr(cmd, 1, RSTART - 1)
		if (match(tail, /[0-9]+/)) headlimit = substr(tail, RSTART, RLENGTH) + 0
	}
	sub(/[ \t]*2>[ \t]*\/dev\/null[ \t]*$/, "", cmd)
	sub(/[ \t]+$/, "", cmd)

	# Anything still compound is somebody else s business: a pipe makes the
	# searcher a filter reading stdin rather than a file search, and the rest
	# can hide a second command.
	if (index(cmd, "|") || index(cmd, "&") || index(cmd, ";") ||
	    index(cmd, ">") || index(cmd, "<") || index(cmd, "$(") ||
	    index(cmd, "`") || index(cmd, "\n")) exit 0

	# Already routed, by us or by a competing rewriter. Hooks run in parallel
	# and the last updatedInput wins, so re-rewriting is a race we skip.
	if (cmd ~ /(^|[ \t])seek([ \t]|$)/) exit 0

	if (!tokenize(cmd, argv) || nargv == 0) exit 0
	if (!ok) exit 0

	start = 2
	if (argv[1] == "git") {
		if (nargv < 2 || argv[2] != "grep") exit 0
		start = 3
	} else if (argv[1] !~ /^(grep|egrep|rg|ag|ack)$/) {
		exit 0
	}

	pattern = ""
	npaths = 0
	sawdd = 0
	for (i = start; i <= nargv; i++) {
		t = argv[i]
		if (t == "--") { sawdd = 1; continue }
		if (!sawdd && t ~ /^-/) {
			if (t == "-e") {                    # -e PATTERN
				if (i == nargv) exit 0
				if (pattern != "") exit 0
				pattern = argv[++i]
				continue
			}
			if (skippable_flag(t)) continue
			exit 0                              # unmapped flag: passthrough
		}
		if (pattern == "" && !sawdd) { pattern = t; continue }
		# git grep pathspecs after `--`, and plain path operands, are scope.
		if (t == "." || t == "./" || t == "*") continue
		paths[++npaths] = t
	}
	if (pattern == "") exit 0

	# A grep for a definition is better served by the symbol index than by a
	# content match, which returns a context window where grep -n returns the
	# one line the caller asked for.
	# A trailing `| head -N` was the caller asking for less; honour it when it
	# asks for less than the default, never for more.
	limit = 20
	if (headlimit > 0 && headlimit < limit) limit = headlimit
	maxmatch = 0
	if (match(pattern, /^(func|def|class|type|fn)[ \t]+[A-Za-z_][A-Za-z0-9_]*$/)) {
		sym = pattern
		sub(/^[a-z]+[ \t]+/, "", sym)
		query = "sym:" sym
		maxmatch = 1
	} else {
		query = "content:" pattern
	}

	newcmd = "seek -n " limit
	if (maxmatch > 0) newcmd = newcmd " -m " maxmatch
	newcmd = newcmd " " shq(query)
	for (i = 1; i <= npaths; i++) newcmd = newcmd " " shq(paths[i])

	notice = "[seek router] Search routed to seek: " newcmd \
	         ". Results are BM25-ranked and capped at " limit \
	         " files, so NOT exhaustive; --include filters were dropped, widening scope." \
	         " For every match (rename, refactor, counting call sites) rerun as:" \
	         " SEEK_ROUTER=off grep -rn '"'"'PATTERN'"'"' ."

	# updatedInput replaces the entire input object, so every field of the
	# original Bash input is copied back alongside the rewritten command.
	out = "{\"command\":\"" json_escape(newcmd) "\""
	desc = json_str_value(payload, "description")
	if (ok && desc != "") out = out ",\"description\":\"" json_escape(desc) "\""
	ok = 1
	tmo = json_raw_value(payload, "timeout")
	if (tmo != "") out = out ",\"timeout\":" tmo
	bg = json_raw_value(payload, "run_in_background")
	if (bg != "") out = out ",\"run_in_background\":" bg
	out = out "}"

	printf "{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"allow\",\"permissionDecisionReason\":\"routed to seek\",\"updatedInput\":%s,\"additionalContext\":\"%s\"}}\n", out, json_escape(notice)
	exit 0
}
'
