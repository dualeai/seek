package main

import (
	"bytes"
	"testing"
)

func TestLargeDocumentKeepsContentAndSymbolSearch(t *testing.T) {
	requireTools(t)
	content := []byte("package large\n\nfunc LargeFileSymbol() {}\n// LARGE_FILE_CONTENT_MARKER\n")
	content = append(content, bytes.Repeat([]byte("// stable filler for the large source fixture\n"), shardMax/44+1)...)
	if len(content) <= shardMax || len(content) > maxIndexedDocumentBytes {
		t.Fatalf("fixture size=%d, want one supported document larger than shardMax=%d", len(content), shardMax)
	}

	indexDir := t.TempDir()
	documents := make(chan fileContent, 1)
	documents <- fileContent{name: "large.go", content: content}
	close(documents)
	indexed, err := indexDocuments(t.Context(), indexDir, "large-quality", t.TempDir(), documents, indexParallelism())
	if err != nil || !indexed {
		t.Fatalf("index large document: indexed=%t error=%v", indexed, err)
	}
	for _, queryText := range []string{"LARGE_FILE_CONTENT_MARKER", "sym:LargeFileSymbol"} {
		matches, err := executeUnscopedShardSearchForTest(t.Context(), indexDir, queryText)
		if err != nil {
			t.Fatalf("search %q: %v", queryText, err)
		}
		if len(matches) != 1 || matches[0].FileName != "large.go" {
			t.Fatalf("search %q matches=%v, want large.go", queryText, matches)
		}
	}
}
