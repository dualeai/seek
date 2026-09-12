package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRunJoinedIndexBuildsStartsBothBranches(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	branch := func(name string) func() {
		return func() {
			started <- name
			<-release
		}
	}
	done := make(chan struct{})
	go func() {
		runJoinedIndexBuilds(branch("lexical"), branch("semantic"))
		close(done)
	}()

	seen := make(map[string]bool, 2)
	for len(seen) < 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(5 * time.Second):
			unblock()
			t.Fatalf("joined builds started=%v, want both branches", seen)
		}
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("joined builds did not finish")
	}
}

func TestJoinedGenerationDescriptorBindsBothIndexParts(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	indexDir := filepath.Join(root, "index")
	for _, dir := range []string{cacheDir, indexDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const state = "folder-state-17"
	const source = "semantic-source-23"
	family := []byte("seek_v17.00000.zoekt 481\nseek_v17.00000.zoekt.meta 92\n")
	if err := os.WriteFile(filepath.Join(indexDir, familyManifestFile), family, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJoinedGeneration(cacheDir, indexDir, state, source); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(filepath.Join(cacheDir, joinedGenerationFile))
	if err != nil {
		t.Fatal(err)
	}
	var descriptor struct {
		Format      uint32 `json:"format"`
		State       string `json:"state"`
		FamilySHA   string `json:"family_sha256"`
		SemanticKey string `json:"semantic_key"`
	}
	if err := json.Unmarshal(content, &descriptor); err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(family)
	if descriptor.Format != 1 || descriptor.State != state ||
		descriptor.FamilySHA != hex.EncodeToString(wantDigest[:]) ||
		descriptor.SemanticKey == "" {
		t.Fatalf("joined descriptor=%+v", descriptor)
	}
	semanticDir := semanticGenerationDir(indexDir, source)
	if descriptor.SemanticKey != filepath.Base(semanticDir)[len(semanticGenerationPrefix):] {
		t.Fatalf("semantic key=%q, want directory %q", descriptor.SemanticKey, semanticDir)
	}
	units, embeddings := testSemanticRowsAndVectors(t)
	if _, err := writeTestSemanticGeneration(
		t.Context(),
		semanticDir,
		source,
		units,
		embeddings,
	); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(semanticDir, semanticManifestFile)
	if !joinedGenerationMatches(cacheDir, indexDir, state, source) {
		t.Fatal("the descriptor did not activate its lexical and semantic data")
	}

	if err := os.WriteFile(filepath.Join(indexDir, familyManifestFile), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if joinedGenerationMatches(cacheDir, indexDir, state, source) {
		t.Fatal("a changed lexical family kept the joined generation active")
	}
	if err := os.WriteFile(filepath.Join(indexDir, familyManifestFile), family, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if joinedGenerationMatches(cacheDir, indexDir, state, source) {
		t.Fatal("a damaged semantic manifest kept the joined generation active")
	}
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(semanticDir, semanticRowsFile), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := openJoinedSemanticGeneration(cacheDir, indexDir, source); err == nil {
		t.Fatal("a damaged semantic artifact opened")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, joinedGenerationFile)); !os.IsNotExist(err) {
		t.Fatalf("damaged semantic data kept its joined descriptor: %v", err)
	}
}

func TestReadJoinedGenerationRejectsInvalidJSON(t *testing.T) {
	const digest = "0000000000000000000000000000000000000000000000000000000000000000"
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "duplicate field",
			content: `{"format":1,"format":1,"state":"s","family_sha256":"` +
				digest + `","semantic_key":"k"}`,
		},
		{
			name: "unknown field",
			content: `{"format":1,"state":"s","family_sha256":"` + digest +
				`","semantic_key":"k","extra":true}`,
		},
		{
			name: "trailing value",
			content: `{"format":1,"state":"s","family_sha256":"` + digest +
				`","semantic_key":"k"}{}`,
		},
		{
			name: "wrong format",
			content: `{"format":2,"state":"s","family_sha256":"` + digest +
				`","semantic_key":"k"}`,
		},
		{name: "incomplete", content: `{"format":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			if err := os.WriteFile(
				filepath.Join(cacheDir, joinedGenerationFile),
				[]byte(test.content),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := readJoinedGeneration(cacheDir); err == nil {
				t.Fatal("invalid joined descriptor was accepted")
			}
		})
	}
}
