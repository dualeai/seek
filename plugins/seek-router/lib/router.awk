# Parse one static search command and emit an equivalent ranked seek command.
# This file belongs to the plugin. Seek does not import or execute it.

BEGIN {
	sq = sprintf("%c", 39)
	regex_meta = "\\.+*?()|[]{}^$"
	file_limit = 20
	match_limit = 3
	max_context = 512
}

{
	if (NR > 1) multiline = 1
	command = command (NR > 1 ? "\n" : "") $0
}

END {
	if (multiline || command == "" || !tokenize(command)) exit

	search_argc = pipe_at ? pipe_at - 1 : argc
	if (search_argc < 1) exit
	if (pipe_at && !parse_head(pipe_at + 1, argc)) exit
	if (!route_search(search_argc)) exit
	if (head_limit && !route_files_only) exit

	limit = file_limit
	if (head_limit && head_limit < limit) limit = head_limit
	context_kind = route_context_flag == "" ? "none" : substr(route_context_flag, 2, 1)
	print context_kind "|" render_seek(limit)
}

function push_token() {
	args[++argc] = token
	token = ""
	token_started = 0
}

# Accept literal POSIX-shell words, quotes, and backslash escapes. Reject every
# form that can expand, redirect, start another command, or run in background.
function tokenize(source,    i, char, next_char, mode, length_source) {
	argc = 0
	pipe_at = 0
	token = ""
	token_started = 0
	mode = "plain"
	length_source = length(source)

	for (i = 1; i <= length_source; i++) {
		char = substr(source, i, 1)

		if (mode == "single") {
			if (char == sq) mode = "plain"
			else token = token char
			continue
		}

		if (mode == "double") {
			if (char == "\"") {
				mode = "plain"
				continue
			}
			if (char == "$" || char == "`") return 0
			if (char == "\\") {
				if (i == length_source) return 0
				next_char = substr(source, i + 1, 1)
				if (index("$`\"\\", next_char)) {
					token = token next_char
					i++
				} else {
					token = token char
				}
				continue
			}
			token = token char
			continue
		}

		if (char == " " || char == "\t") {
			if (token_started) push_token()
			continue
		}
		if (char == sq) {
			mode = "single"
			token_started = 1
			continue
		}
		if (char == "\"") {
			mode = "double"
			token_started = 1
			continue
		}
		if (char == "\\") {
			if (i == length_source) return 0
			token = token substr(source, ++i, 1)
			token_started = 1
			continue
		}
		if (char == "|") {
			if (token_started) push_token()
			if (pipe_at) return 0
			args[++argc] = char
			pipe_at = argc
			continue
		}
		if (index("$`#;&<>()", char) || index("*?[]{}", char)) return 0
		if (char == "~" && !token_started) return 0

		token = token char
		token_started = 1
	}

	if (mode != "plain") return 0
	if (token_started) push_token()
	if (!argc || pipe_at == 1 || pipe_at == argc) return 0
	return 1
}

function parse_head(first, last,    value) {
	if (args[first] != "head") return 0
	if (first == last) {
		head_limit = 10
		return 1
	}
	if (last - first == 1) {
		value = args[first + 1]
		if (value ~ /^-[0-9]+$/) return set_head_limit(substr(value, 2))
		if (value ~ /^-n[0-9]+$/) return set_head_limit(substr(value, 3))
		if (value ~ /^--lines=[0-9]+$/) return set_head_limit(substr(value, 9))
		return 0
	}
	if (last - first != 2 || (args[first + 1] != "-n" && args[first + 1] != "--lines")) return 0
	return set_head_limit(args[first + 2])
}

function set_head_limit(value) {
	if (!decimal_digits(value) || value + 0 <= 0) return 0
	head_limit = value + 0
	return 1
}

function route_search(last) {
	reset_route()
	if (args[1] == "grep") return route_grep(2, last)
	if (args[1] == "rg") return route_rg(2, last)
	if (args[1] == "git" && last >= 2 && args[2] == "grep") return route_git_grep(3, last)
	if (args[1] == "fd") return route_fd(2, last)
	if (args[1] == "find") return route_find(2, last)
	return 0
}

function reset_route(    i) {
	for (i = 1; i <= route_npaths; i++) delete route_paths[i]
	route_npaths = 0
	route_pattern = ""
	route_insensitive = 0
	route_files_only = 0
	route_query_kind = "content"
	route_context_flag = ""
	route_context_lines = 0
	route_match_limit = match_limit
}

function route_grep(first, last,    i, j, arg, flag, pattern_at, recursive, insensitive, fixed, extended, files_only) {
	pattern_at = 0
	for (i = first; i <= last; i++) {
		arg = args[i]
		if (arg == "--") {
			pattern_at = i + 1
			break
		}
		if (substr(arg, 1, 1) != "-" || arg == "-") {
			pattern_at = i
			break
		}
		if (length(arg) < 2 || substr(arg, 1, 2) == "--") return 0
		for (j = 2; j <= length(arg); j++) {
			flag = substr(arg, j, 1)
			if (flag == "r") recursive = 1
			else if (flag == "i") insensitive = 1
			else if (flag == "F") fixed = 1
			else if (flag == "E") extended = 1
			else if (flag == "l") files_only = 1
			else if (!index("nHI", flag)) return 0
		}
	}
	if (!recursive || !pattern_at || pattern_at >= last || (fixed && extended)) return 0
	route_pattern = args[pattern_at]
	if (route_pattern == "" || (!fixed && !legacy_search_atom(route_pattern))) return 0
	if (fixed) route_pattern = regexp_quote(route_pattern)
	if (!copy_paths(pattern_at + 1, last) || has_dash_path()) return 0
	route_insensitive = insensitive
	route_files_only = files_only
	return 1
}

function route_rg(first, last,    i, j, arg, flag, value, pattern_at, path_at, pattern_set, insensitive, fixed, files_only) {
	pattern_at = 0
	for (i = first; i <= last; i++) {
		arg = args[i]
		if (arg == "--") {
			if (pattern_set) path_at = i + 1
			else pattern_at = i + 1
			break
		}
		if (substr(arg, 1, 1) != "-" || arg == "-") {
			if (pattern_set) path_at = i
			else pattern_at = i
			break
		}
		if (arg == "-e" || arg == "--regexp") {
			if (pattern_set || i == last) return 0
			route_pattern = args[++i]
			pattern_set = 1
			continue
		}
		if (substr(arg, 1, 9) == "--regexp=") {
			if (pattern_set || length(arg) == 9) return 0
			route_pattern = substr(arg, 10)
			pattern_set = 1
			continue
		}
		if (substr(arg, 1, 2) == "--") {
			if (arg == "--ignore-case") insensitive = 1
			else if (arg == "--fixed-strings") fixed = 1
			else if (arg == "--files-with-matches") files_only = 1
			else if (arg == "--line-number" || arg == "--with-filename" || arg == "--no-filename" || arg == "--no-messages") continue
			else if (arg == "--after-context" || arg == "--context") {
				if (i == last || !set_rg_context(arg == "--after-context" ? "-A" : "-C", args[++i])) return 0
			} else if (substr(arg, 1, 16) == "--after-context=") {
				if (!set_rg_context("-A", substr(arg, 17))) return 0
			} else if (substr(arg, 1, 10) == "--context=") {
				if (!set_rg_context("-C", substr(arg, 11))) return 0
			} else return 0
			continue
		}
		if (length(arg) < 2) return 0
		if (arg == "-A" || arg == "-C") {
			if (i == last || !set_rg_context(arg, args[++i])) return 0
			continue
		}
		for (j = 2; j <= length(arg); j++) {
			flag = substr(arg, j, 1)
			if (flag == "i") insensitive = 1
			else if (flag == "F") fixed = 1
			else if (flag == "l") files_only = 1
			else if (flag == "e") {
				if (pattern_set) return 0
				value = substr(arg, j + 1)
				if (value == "") {
					if (i == last) return 0
					value = args[++i]
				}
				route_pattern = value
				pattern_set = 1
				break
			}
			else if (flag == "A" || flag == "C") {
				value = substr(arg, j + 1)
				if (value == "") {
					if (i == last) return 0
					value = args[++i]
				}
				if (!set_rg_context("-" flag, value)) return 0
				break
			}
			else if (!index("nHI", flag)) return 0
		}
	}
	if (pattern_at) {
		if (pattern_set || pattern_at > last) return 0
		route_pattern = args[pattern_at]
		pattern_set = 1
		path_at = pattern_at + 1
	}
	if (!pattern_set || route_pattern == "") return 0
	if (fixed) route_pattern = regexp_quote(route_pattern)

	if (!path_at || path_at > last) {
		route_paths[++route_npaths] = "."
	} else if (!copy_paths(path_at, last) || has_dash_path()) {
		return 0
	}
	if (route_context_flag != "" && ((!path_at || path_at > last) || files_only)) return 0

	route_insensitive = insensitive
	route_files_only = files_only
	if (route_context_flag != "") route_match_limit = 1
	return 1
}

function set_rg_context(flag, value,    number) {
	if (route_context_flag != "" || !decimal_digits(value)) return 0
	number = value + 0
	if (number < 0 || number > max_context) return 0
	route_context_flag = flag
	route_context_lines = number
	return 1
}

function route_git_grep(first, last,    i, j, arg, flag, pattern_at, insensitive, fixed, files_only) {
	pattern_at = 0
	for (i = first; i <= last; i++) {
		arg = args[i]
		if (substr(arg, 1, 1) != "-" || arg == "-") {
			pattern_at = i
			break
		}
		if (length(arg) < 2 || substr(arg, 1, 2) == "--") return 0
		for (j = 2; j <= length(arg); j++) {
			flag = substr(arg, j, 1)
			if (flag == "i") insensitive = 1
			else if (flag == "F") fixed = 1
			else if (flag == "l") files_only = 1
			else if (flag != "n") return 0
		}
	}
	if (!pattern_at || pattern_at > last) return 0
	route_pattern = args[pattern_at]
	if (!fixed && !portable_regex_literal(route_pattern)) return 0
	if (fixed) route_pattern = regexp_quote(route_pattern)

	if (pattern_at == last) {
		route_paths[++route_npaths] = "."
	} else {
		if (args[pattern_at + 1] != "--") return 0
		if (pattern_at + 1 == last) route_paths[++route_npaths] = "."
		else if (!copy_paths(pattern_at + 2, last) || !literal_git_paths()) return 0
	}
	route_insensitive = insensitive
	route_files_only = files_only
	return 1
}

function route_fd(first, last,    i) {
	i = first
	if (i <= last && args[i] == "-t") {
		if (i == last || args[i + 1] != "f") return 0
		i += 2
	}
	if (i > last || args[i] == "" || substr(args[i], 1, 1) == "-") return 0
	route_pattern = args[i++]
	if (i > last) route_paths[++route_npaths] = "."
	else if (!copy_paths(i, last) || has_dash_path()) return 0
	route_query_kind = "file"
	route_files_only = 1
	route_insensitive = !has_upper(route_pattern)
	return 1
}

function route_find(first, last,    i, predicate_at, predicate_last, pattern) {
	predicate_at = 0
	for (i = first; i <= last; i++) {
		if (substr(args[i], 1, 1) == "-") {
			predicate_at = i
			break
		}
		if (args[i] == "" || substr(args[i], 1, 1) == "-") return 0
	}
	if (!predicate_at || predicate_at <= first) return 0
	predicate_last = last
	if (args[predicate_last] == "-print") predicate_last--
	if (predicate_last - predicate_at != 3) return 0
	if (args[predicate_at] == "-type" && args[predicate_at + 1] == "f" && args[predicate_at + 2] == "-name") {
		pattern = args[predicate_at + 3]
	} else if (args[predicate_at] == "-name" && args[predicate_at + 2] == "-type" && args[predicate_at + 3] == "f") {
		pattern = args[predicate_at + 1]
	} else return 0
	if (!copy_paths(first, predicate_at - 1) || !literal_find_roots()) return 0
	pattern = find_name_regex(pattern)
	if (pattern == "") return 0
	route_pattern = pattern
	route_query_kind = "file"
	route_files_only = 1
	return 1
}

function copy_paths(first, last,    i) {
	if (first > last) return 0
	for (i = first; i <= last; i++) route_paths[++route_npaths] = args[i]
	return 1
}

function has_dash_path(    i) {
	for (i = 1; i <= route_npaths; i++) {
		if (route_paths[i] == "" || substr(route_paths[i], 1, 1) == "-") return 1
	}
	return 0
}

function literal_git_paths(    i, path) {
	for (i = 1; i <= route_npaths; i++) {
		path = route_paths[i]
		if (path == "" || substr(path, 1, 1) == "-" || substr(path, 1, 1) == ":") return 0
		if (path ~ /[*?\[\]]/) return 0
	}
	return 1
}

function literal_find_roots(    i, path) {
	for (i = 1; i <= route_npaths; i++) {
		path = route_paths[i]
		if (path == "" || substr(path, 1, 1) == "-" || path == "!" || path == "(" || path == ")" || path == ",") return 0
	}
	return 1
}

function find_name_regex(pattern,    i, char, output) {
	if (pattern == "" || pattern ~ /[\/?\[\]\\]/) return ""
	output = "(^|/)"
	for (i = 1; i <= length(pattern); i++) {
		char = substr(pattern, i, 1)
		if (char == "*") output = output "[^/]*"
		else if (index(regex_meta, char)) output = output "\\" char
		else output = output char
	}
	return output "$"
}

function regexp_quote(value,    i, char, output) {
	for (i = 1; i <= length(value); i++) {
		char = substr(value, i, 1)
		if (index(regex_meta, char)) output = output "\\" char
		else output = output char
	}
	return output
}

function portable_regex_literal(value,    i) {
	if (value == "") return 0
	for (i = 1; i <= length(value); i++) {
		if (index(regex_meta, substr(value, i, 1))) return 0
	}
	return 1
}

function legacy_search_atom(value,    i, char) {
	if (value == "") return 0
	for (i = 1; i <= length(value); i++) {
		char = substr(value, i, 1)
		if (char ~ /[A-Za-z0-9_\/-]/) continue
		return 0
	}
	return 1
}

function has_upper(value) {
	return value ~ /[A-Z]/
}

function decimal_digits(value) {
	return value != "" && value !~ /[^0-9]/
}

function quote_query_value(value,    i, char, output) {
	output = "\""
	for (i = 1; i <= length(value); i++) {
		char = substr(value, i, 1)
		if (char == "\\" || char == "\"") output = output "\\"
		output = output char
	}
	return output "\""
}

function shell_quote(value,    i, char, output) {
	output = sq
	for (i = 1; i <= length(value); i++) {
		char = substr(value, i, 1)
		if (char == sq) output = output sq "\\" sq sq
		else output = output char
	}
	return output sq
}

function render_seek(limit,    i, query, output) {
	query = route_insensitive ? "case:no " : "case:yes "
	if (route_query_kind == "file") query = query "type:file file:"
	else {
		if (route_files_only) query = query "type:file "
		query = query "content:"
	}
	query = query quote_query_value(route_pattern)

	output = "seek -n " limit " -m " route_match_limit
	if (route_context_flag != "") output = output " " route_context_flag " " route_context_lines
	output = output " " shell_quote(query)
	for (i = 1; i <= route_npaths; i++) output = output " " shell_quote(route_paths[i])
	return output
}
