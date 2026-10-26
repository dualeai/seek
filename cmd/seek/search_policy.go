package main

// searchPolicy selects the search stages of one command. With semantic, a
// query also uses the model; --lexical-only turns it off.
type searchPolicy struct {
	semantic bool
}

func defaultSearchPolicy() searchPolicy {
	return searchPolicy{semantic: true}
}

func lexicalOnlySearchPolicy() searchPolicy {
	return searchPolicy{}
}

func (p searchPolicy) semanticEnabled() bool {
	return p.semantic
}
