package main

import (
	"context"
	"path/filepath"
	"sort"

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

// displayOperands is deepest first.
type displayOperands []displayOperand

// newDisplayOperands resolves the path operands, or "." when there are none,
// deepest root first. An operand with no absolute path is skipped.
func newDisplayOperands(operands []string) displayOperands {
	if len(operands) == 0 {
		operands = []string{"."}
	}
	out := make(displayOperands, 0, len(operands))
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
		out = append(out, displayOperand{typed: typed, root: root})
	}
	sort.SliceStable(out, func(left, right int) bool { return len(out[left].root) > len(out[right].root) })
	return out
}

// header is the display path of the file at abs.
func (operands displayOperands) header(abs string) string {
	for _, operand := range operands {
		rel, ok := relWithin(operand.root, abs)
		if !ok {
			continue
		}
		if rel == "." {
			return operand.typed
		}
		return filepath.Join(operand.typed, rel)
	}
	return abs
}

// headersArePaths reports whether every file of the node at root shows its
// node-relative path, so a file: filter needs no rewrite. That is so when
// three conditions hold:
//   - The operand at the root, if any, is ".".
//   - No operand above the root is the deepest one that holds it.
//   - Every operand below the root is typed as its path below the root.
func (operands displayOperands) headersArePaths(root string) bool {
	holder, below := false, false
	for _, operand := range operands {
		rel, inside := relWithin(root, operand.root)
		switch {
		case inside && rel == ".":
			if operand.typed != "" {
				return false
			}
			holder = true
		case inside:
			if operand.typed != rel {
				return false
			}
			below = true
		case !holder && pathWithin(operand.root, root):
			// The deepest operand that holds the root is above it.
			return false
		}
	}
	return holder || below
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
			// A highlight in a part that the header drops is removed.
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
		if q == nil {
			continue
		}
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
	if len(operands) == 0 || operands.headersArePaths(plan.displayRoot) {
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
	var compileErr error
	out := rewriteFileNameAtoms(qp, func(atom query.Q) query.Q {
		if compileErr != nil {
			return atom
		}
		if set, ok := sets[atom]; ok {
			return set
		}
		var filter semanticPathFilter
		filter, compileErr = newSemanticPathFilter(atom)
		if compileErr != nil {
			return atom
		}
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
	})
	if compileErr != nil {
		return queryPlan{}, compileErr
	}
	return out, nil
}

// rewriteFileNameAtoms replaces each explicit file: atom of the strict, relaxed
// and filter queries of qp; a bare word's file-name half stays.
func rewriteFileNameAtoms(qp queryPlan, replace func(query.Q) query.Q) queryPlan {
	rewrite := func(q query.Q) query.Q {
		if q == nil {
			return nil
		}
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

// displayHeader is the header of a result. For a result with no header, it is
// the path joined to its display root with showRoot, else the display name,
// else the node-relative path.
func displayHeader(result corpusSearchResult, showRoot bool) string {
	switch {
	case result.header != "":
		return result.header
	case showRoot:
		return filepath.Join(result.displayRoot, result.file.FileName)
	case result.displayName != "":
		return result.displayName
	}
	return result.file.FileName
}
