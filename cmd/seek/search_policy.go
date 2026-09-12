package main

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
