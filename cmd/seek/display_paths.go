package main

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// Headers and file: filters read one string per file. It is the operand as
// typed and cleaned, plus the path of the file below the operand's root: the
// ripgrep and git grep rule. With no operand, or ".", it is the path below the
// cwd with no "./". A file takes the form of the deepest operand that holds
// it.

// displayOperand is one path operand: its cleaned text ("" for ".") and its
// canonical root.
type displayOperand struct {
	typed string
	root  string
}

// displayOperands holds the operands. The zero value has no operand.
type displayOperands struct {
	// byRoot holds the first operand of each root (header).
	byRoot map[string]displayOperand
	// typedAsPath holds each operand root and each folder above one. The
	// value is true when each operand at or below the folder is typed as its
	// root's path below the folder. That path is "" for the folder itself
	// (headersArePaths).
	typedAsPath map[string]bool
}

// newDisplayOperands resolves the path operands. The caller gives "." when
// the user gave no path. It skips an operand with no absolute path.
func newDisplayOperands(operands []string) displayOperands {
	resolved := displayOperands{
		byRoot:      make(map[string]displayOperand, len(operands)),
		typedAsPath: make(map[string]bool, len(operands)),
	}
	for _, operand := range operands {
		abs, err := filepath.Abs(operand)
		if err != nil {
			continue
		}
		// Every root takes its on-disk case, as the node roots do, so a
		// wrong-case operand or cwd still gives relative headers.
		root := onDiskPath(canonicalCorpusPath(abs))
		typed := filepath.Clean(operand)
		if typed == "." {
			typed = ""
		}
		resolved.add(displayOperand{typed: typed, root: root})
	}
	return resolved
}

// add puts the operand in both maps. It updates the entry of the root and of
// each folder above it, so its cost grows only with the folder depth.
// headersArePaths then reads the answer for a node from one entry.
func (operands displayOperands) add(operand displayOperand) {
	if _, ok := operands.byRoot[operand.root]; !ok {
		operands.byRoot[operand.root] = operand
	}
	for dir, ok := operand.root, true; ok; dir, ok = parentFolder(dir) {
		if typed, seen := operands.typedAsPath[dir]; !seen || typed {
			rel := strings.TrimPrefix(operand.root[len(dir):], string(filepath.Separator))
			operands.typedAsPath[dir] = operand.typed == rel
		}
	}
}

// parentFolder returns the folder that holds the clean path dir. It returns
// false for "/" and for a path with no separator. It cuts bytes: filepath.Dir
// would clean each parent again.
func parentFolder(dir string) (string, bool) {
	cut := strings.LastIndexByte(dir, filepath.Separator)
	if cut < 0 || dir == string(filepath.Separator) {
		return "", false
	}
	return dir[:max(cut, 1)], true
}

// header is the display path of the file at abs, or abs when no operand holds
// it. Each root is a clean absolute path (canonicalCorpusPath, onDiskPath). So
// only abs and its parent folders can be the root of an operand that holds
// abs. header looks them up, deepest first: the cost does not grow with the
// operand count.
func (operands displayOperands) header(abs string) string {
	path := filepath.Clean(abs)
	for dir, ok := path, true; ok; dir, ok = parentFolder(dir) {
		if operand, held := operands.byRoot[dir]; held {
			if dir == path {
				return operand.typed
			}
			rel := strings.TrimPrefix(path[len(dir):], string(filepath.Separator))
			return filepath.Join(operand.typed, rel)
		}
	}
	return abs
}

// headersArePaths reports whether every file of the node at root shows its
// node-relative path, so a file: filter needs no rewrite. That is so when
// three conditions hold:
//   - At least one operand is at or below the root.
//   - Each operand at or below the root is typed as its path below the root
//     ("." at the root).
//   - No operand above the root is the deepest one that holds it.
//
// Each node calls it. It reads the maps that newDisplayOperands built once,
// so its cost does not grow with the operand count.
func (operands displayOperands) headersArePaths(root string) bool {
	root = filepath.Clean(root)
	if !operands.typedAsPath[root] {
		return false
	}
	// The deepest operand that holds the root, if any, has its root there.
	for dir, ok := root, true; ok; dir, ok = parentFolder(dir) {
		if _, held := operands.byRoot[dir]; held {
			return dir == root
		}
	}
	return true
}

// applyHeaders sets the display path of every result and of its copies. It
// also sets the text of each file-name line, so one result shows one path
// form.
func applyHeaders(results []corpusSearchResult, operands displayOperands) {
	for index := range results {
		result := &results[index]
		result.header = operands.header(filepath.Join(result.displayRoot, filepath.FromSlash(result.file.FileName)))
		headerLine := filepath.ToSlash(result.header)
		for lineIndex := range result.file.LineMatches {
			line := &result.file.LineMatches[lineIndex]
			if !line.FileName || string(line.Line) == headerLine {
				continue
			}
			// The header and the node path share the path below the
			// operand. A highlight there moves to its place in the header.
			// The rewrite removes a highlight in a part that the header drops.
			var fragments []zoekt.LineFragmentMatch
			if name := result.file.FileName; string(line.Line) == name {
				shared := sharedPathSuffix(headerLine, name)
				pathStart, headerStart := len(name)-len(shared), len(headerLine)-len(shared)
				for _, fragment := range line.LineFragments {
					if fragment.LineOffset >= pathStart {
						fragment.LineOffset += headerStart - pathStart
						fragments = append(fragments, fragment)
					}
				}
			}
			line.LineFragments = fragments
			line.Line = []byte(headerLine)
		}
		for copyIndex := range result.also {
			duplicate := &result.also[copyIndex]
			duplicate.header = operands.header(filepath.Join(duplicate.displayRoot, filepath.FromSlash(duplicate.path)))
		}
	}
}

// sharedPathSuffix returns the longest end of path components that a and b
// share.
func sharedPathSuffix(a, b string) string {
	i, j := len(a), len(b)
	for i > 0 && j > 0 && a[i-1] == b[j-1] {
		i--
		j--
	}
	for i < len(a) && ((i > 0 && a[i-1] != '/') || (j > 0 && b[j-1] != '/')) {
		i++
		j++
	}
	return a[i:]
}

// fileNameAtom reports a Zoekt atom that matches the file name.
func fileNameAtom(q query.Q) bool {
	switch atom := q.(type) {
	case *query.Substring:
		return atom.FileName && !atom.Content
	case *query.Regexp:
		return atom.FileName && !atom.Content
	}
	return false
}

// hasFileNameAtom reports an explicit file: atom (not a bare word's half).
func hasFileNameAtom(halves map[query.Q]struct{}, queries ...query.Q) bool {
	found := false
	for _, q := range queries {
		query.VisitAtoms(q, func(atom query.Q) {
			if _, half := halves[atom]; !half {
				found = found || fileNameAtom(atom)
			}
		})
	}
	return found
}

// nodeHeaderQuery makes the file: atoms of qp match the display path of each
// file of the node: the string that the header prints. Each atom becomes the
// set of the node's files whose display path it matches, with Zoekt's case
// rules. The strict, relaxed and filter queries all read the sets, so lexical
// and semantic retrieval keep one rule.
func nodeHeaderQuery(ctx context.Context, read *nodeRead, qp queryPlan, operands displayOperands) (queryPlan, error) {
	plan := read.plan
	queries := []query.Q{qp.strict}
	if qp.model != nil {
		queries = append(queries, qp.model.relaxedQ)
		queries = append(queries, qp.model.filters...)
	}
	if !hasFileNameAtom(qp.halves, queries...) {
		return qp, nil
	}
	if len(operands.byRoot) == 0 || operands.headersArePaths(plan.displayRoot) {
		// The headers are the index paths: Zoekt matches each explicit
		// file: atom on them and adds its file-name term. The header sets
		// below keep that term (the atom-or-not-atom clause).
		return qp, nil
	}
	opts := searchOpts
	opts.NumContextLines = 0
	all := &query.Type{Type: query.TypeFileName, Child: &query.Const{Value: true}}
	files, err := read.search(ctx, all, searchConfig{opts: &opts})
	if err != nil {
		return queryPlan{}, err
	}
	names := make([]string, len(files))
	headers := make([]string, len(files))
	for index, file := range files {
		names[index] = file.FileName
		header := operands.header(filepath.Join(plan.displayRoot, filepath.FromSlash(file.FileName)))
		headers[index] = filepath.ToSlash(header)
	}
	sets := map[query.Q]query.Q{}
	return rewriteFileNameAtoms(qp, func(atom query.Q) query.Q {
		if set, ok := sets[atom]; ok {
			return set
		}
		filter := newSemanticPathFilter(atom)
		set := &query.FileNameSet{Set: map[string]struct{}{}}
		for index, header := range headers {
			if filter.matches(header) {
				set.Set[names[index]] = struct{}{}
			}
		}
		// The set filters by the header and gives no BM25 term. The clause
		// atom-or-not-atom filters nothing. From it, Zoekt gathers the atom's
		// matches on the index path: the term that the atom gives with path
		// headers. So every layout has one rule, which Zoekt computes.
		scored := query.NewAnd(set, query.NewOr(atom, &query.Not{Child: atom}))
		sets[atom] = scored
		return scored
	}), nil
}

// rewriteFileNameAtoms replaces each explicit file: atom of the strict, relaxed
// and filter queries of qp; a bare word's file-name half stays.
func rewriteFileNameAtoms(qp queryPlan, replace func(query.Q) query.Q) queryPlan {
	rewrite := func(q query.Q) query.Q {
		return query.Map(q, func(atom query.Q) query.Q {
			if _, half := qp.halves[atom]; half || !fileNameAtom(atom) {
				return atom
			}
			return replace(atom)
		})
	}
	out := qp
	out.strict = rewrite(qp.strict)
	if qp.model != nil {
		model := *qp.model
		model.relaxedQ = rewrite(qp.model.relaxedQ)
		model.filters = make([]query.Q, len(qp.model.filters))
		for index, filter := range qp.model.filters {
			model.filters[index] = rewrite(filter)
		}
		out.model = &model
	}
	return out
}
