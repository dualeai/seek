package main

// queryRole is what one query atom does in a descriptive search.
type queryRole string

const (
	// roleDescription: the model reads the atom's text, and the lexical half
	// keeps the Zoekt meaning.
	roleDescription queryRole = "description"
	// roleFilter: Zoekt applies the atom in lexical retrieval, and the
	// per-node Zoekt mask applies it to semantic retrieval: the rows of the
	// files that the filters keep on the node's own shards.
	roleFilter queryRole = "filter"
	// roleModifier: the atom is neither model text nor a filter: a parse
	// directive, grouping, an output mode, or a search-wide condition (a
	// condition such as --lexical-only can choose strict ranking).
	roleModifier queryRole = "modifier"
)

// querySyntax is one row of the query support table. The table covers every
// spelling that the pinned Zoekt parser accepts, and the Seek-only forms.
type querySyntax struct {
	// id is the row ID (T01-T41).
	id       string
	spelling string
	role     queryRole
	meaning  string
	// example is a query with the atom; empty for a search-wide condition.
	example string
}

// querySupport is the support table of descriptive search, kept as test data:
// the planner gives every row its role from Zoekt's parsed tree
// (TestQuerySupportRowsRoute).
var querySupport = []querySyntax{
	{"T01", "bare word", roleDescription, "Matches the file name or the content.", "parse"},
	{"T02", "bare regular expression token", roleDescription, "The model reads the token text; the lexical half keeps the regular expression.", "parsed?"},
	{"T03", "regex:", roleDescription, "The same atom as a bare token.", "regex:request"},
	{"T04", "quoted phrase", roleDescription, "The model reads the phrase; the lexical half keeps Zoekt's meaning, a regular expression of the quoted text.", `"request line"`},
	{"T05", "quoted word", roleDescription, "The same atom as the bare word.", `"request"`},
	{"T06", "content: (c:)", roleFilter, "Content only, applied by Zoekt; the semantic mask applies it too.", "content:method"},
	{"T07", "-content: (-c:)", roleFilter, "Content without the term, applied by Zoekt; the semantic mask applies it too.", "-content:malformed"},
	{"T08", "file: (f:)", roleFilter, "Path filter; the semantic mask applies it too.", "file:server"},
	{"T09", "-file: (-f:)", roleFilter, "Negated path filter; the semantic mask applies it too.", "-file:_test.go"},
	{"T10", "lang:", roleFilter, "Language filter; the semantic mask applies it too.", "lang:go"},
	{"T11", "lang: with an unknown language", roleFilter, "Zoekt pass-through: matches nothing.", "lang:nosuchlanguage"},
	{"T12", "-lang:", roleFilter, "Files in other languages, applied by Zoekt; the semantic mask applies it too.", "-lang:go"},
	{"T13", "sym:", roleFilter, "Files with a matching ctags symbol, applied by Zoekt; the semantic mask applies it too.", "sym:ParseRequest"},
	{"T14", "-sym:", roleFilter, "Files without the symbol, applied by Zoekt; the semantic mask applies it too.", "-sym:ParseRequest"},
	{"T15", "case:yes, case:no, case:auto", roleModifier, "Parse directive: sets the case sensitivity of the atoms and leaves the tree. A negated -case: makes Zoekt's per-shard searcher panic (an upstream bug).", "case:yes"},
	{"T16", "smart case (a capital letter)", roleDescription, "A description word that matches its case.", "Parse"},
	{"T17", "type:file, type:filename (t:file, t:filename)", roleModifier, "Output mode: the search prints file names only. With a negated -type:, Zoekt fails the search of each node; Seek warns and skips the node.", "type:file"},
	{"T18", "type:filematch (t:filematch)", roleFilter, "Zoekt pass-through; Zoekt's per-shard searcher panics on it (an upstream bug).", "type:filematch"},
	{"T19", "type:repo (t:repo)", roleFilter, "Zoekt pass-through; Zoekt's per-shard searcher panics on it (an upstream bug).", "type:repo"},
	{"T20", "repo: (r:)", roleFilter, "Zoekt pass-through on Seek's internal repository names.", "repo:nomatch"},
	{"T21", "-repo: (-r:)", roleFilter, "Zoekt pass-through.", "-repo:nomatch"},
	{"T22", "branch: (b:)", roleFilter, "Zoekt pass-through: committed Git files carry the branch HEAD.", "branch:HEAD"},
	{"T23", "archived:, fork:, public:", roleFilter, "Zoekt pass-through on repository flags.", "archived:no"},
	{"T24", "meta.<field>:", roleFilter, "Zoekt pass-through: Seek sets no repository metadata.", "meta.owner:x"},
	{"T25", "or between filters", roleFilter, "Either filter. An or binds the whole query unless it is in parentheses; an or between descriptions keeps strict ranking.", "(file:server or file:tools)"},
	{"T26", "and, not, OR", roleDescription, "Plain words, not operators: only a lowercase or is an operator.", "and"},
	{"T27", "-word", roleFilter, "Files without the term, applied by Zoekt; the semantic mask applies it too.", "-malformed"},
	{"T28", "parentheses", roleModifier, "Grouping; Zoekt flattens it.", "(alpha beta)"},
	{"T29", "-( ... )", roleFilter, "Files that match none of the group, applied by Zoekt; the semantic mask applies it too.", "-(file:test or lang:python)"},
	{"T30", "backslash escape", roleDescription, "A literal character in a word.", `foo\.bar`},
	{"T31", "C++, X[], X(), a::b() programming names", roleDescription, "When Zoekt rejects the query, Seek quotes each programming name and parses the query again; filters keep their meaning, and the model reads the name.", "C++"},
	{"T32", "unknown prefix (l:go, ext:go)", roleDescription, "Not a Zoekt prefix: a plain word.", "l:go"},
	{"T33", "API-only nodes (FileNameSet)", roleFilter, "Seek's path-operand scope; the parser makes no such node.", ""},
	{"T34", "one description atom", roleDescription, "Strict ranking: the model routes need two or more description atoms.", ""},
	{"T35", "two or more identifiers", roleDescription, "Description atoms like any words.", ""},
	{"T36", "no description atom", roleFilter, "Strict ranking: filters alone give the model nothing to read.", ""},
	{"T37", "more than 125 terms", roleModifier, "A long query keeps every token in 128-token query windows.", ""},
	{"T38", "--lexical-only", roleModifier, "No model work for any query.", ""},
	{"T39", "path-operand scope", roleFilter, "The scope is one more filter subtree: Zoekt keeps the files in scope, and the mask keeps their rows for semantic retrieval.", ""},
	{"T40", "two or more nodes", roleModifier, "Each node runs the same pipeline with its own mask; the results do not depend on the node count.", ""},
	{"T41", "a token with no letter or digit", roleModifier, "Zoekt keeps it in both lexical queries; the model reads nothing from it and the route does not change.", "."},
}
