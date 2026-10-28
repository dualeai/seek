package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
)

// TestRunJoinedIndexBuildsStartsBothBranches gives two branches that wait
// for the test. Both must start before either ends. The test runs in a
// synctest bubble: synctest.Wait returns when every branch that started
// waits.
func TestRunJoinedIndexBuildsStartsBothBranches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		synctest.Wait()
		if count := len(started); count != 2 {
			unblock()
			<-done
			t.Fatalf("joined builds started %d branches before either ended, want both", count)
		}
		unblock()
		<-done
	})
}

// TestJoinedGenerationDescriptorBindsBothIndexParts writes the joined
// descriptor of a state: it names the state, the SHA-256 of the family
// manifest, and the semantic generation directory. A changed family or a
// damaged semantic manifest stops the match. An open that finds damaged rows
// removes the descriptor.
func TestJoinedGenerationDescriptorBindsBothIndexParts(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	indexDir := filepath.Join(root, "index")
	store := corpusStore{cacheDir: cacheDir, indexDir: indexDir}
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
	if descriptor.Format != joinedGenerationVersion || descriptor.State != state ||
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
	if !store.joinedGenerationsMatch(state, source, "") {
		t.Fatal("the descriptor did not activate its lexical and semantic data")
	}

	if err := os.WriteFile(filepath.Join(indexDir, familyManifestFile), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if store.joinedGenerationsMatch(state, source, "") {
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
	if store.joinedGenerationsMatch(state, source, "") {
		t.Fatal("a damaged semantic manifest kept the joined generation active")
	}
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(semanticDir, semanticRowsFile), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.openJoinedSemanticGeneration(source); err == nil {
		t.Fatal("a damaged semantic artifact opened")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, joinedGenerationFile)); !os.IsNotExist(err) {
		t.Fatalf("damaged semantic data kept its joined descriptor: %v", err)
	}
}

// TestJoinedGenerationInvalidatesDescriptorOnLazyGraphDamage damages a graph
// shard of a bound generation. The open does not hash the shard, so the
// descriptor stays. The first check of the shard finds the damage and removes
// the descriptor.
func TestJoinedGenerationInvalidatesDescriptorOnLazyGraphDamage(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	indexDir := filepath.Join(root, "index")
	store := corpusStore{cacheDir: cacheDir, indexDir: indexDir}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, familyManifestFile), []byte("family\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const source = "lazy-graph-source"
	semanticDir := semanticGenerationDir(indexDir, source)
	units, embeddings := testSemanticRowsAndVectors(t)
	if _, err := writeTestSemanticGeneration(
		t.Context(), semanticDir, source, units, embeddings,
	); err != nil {
		t.Fatal(err)
	}
	if err := writeJoinedGeneration(cacheDir, indexDir, "state", source); err != nil {
		t.Fatal(err)
	}
	graphPath := filepath.Join(semanticDir, semanticUSearchShardName(0))
	if err := os.WriteFile(graphPath, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := store.openJoinedSemanticGeneration(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = generation.Close() })
	if _, err := os.Stat(filepath.Join(cacheDir, joinedGenerationFile)); err != nil {
		t.Fatal("lazy open removed the descriptor before graph use")
	}
	if err := generation.validateUSearchShard(0); err == nil {
		t.Fatal("lazy graph validation accepted damage")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, joinedGenerationFile)); !os.IsNotExist(err) {
		t.Fatalf("graph damage kept its joined descriptor: %v", err)
	}
}

// TestReadJoinedGenerationRejectsInvalidJSON reads a valid descriptor, then
// descriptors that each differ from it by one defect. The format number comes
// from joinedGenerationVersion, so each defect stays the only one after a
// format change.
func TestReadJoinedGenerationRejectsInvalidJSON(t *testing.T) {
	const digest = "0000000000000000000000000000000000000000000000000000000000000000"
	format := fmt.Sprintf(`"format":%d`, joinedGenerationVersion)
	const fields = `"state":"s","family_sha256":"` + digest + `","semantic_key":"k"`
	valid := "{" + format + "," + fields + "}"
	read := func(t *testing.T, content string) error {
		t.Helper()
		cacheDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cacheDir, joinedGenerationFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := readJoinedGeneration(cacheDir)
		return err
	}
	if err := read(t, valid); err != nil {
		t.Fatalf("valid joined descriptor was rejected: %v", err)
	}
	tests := []struct {
		name    string
		content string
	}{
		{name: "duplicate field", content: "{" + format + "," + format + "," + fields + "}"},
		{name: "unknown field", content: "{" + format + "," + fields + `,"extra":true}`},
		{name: "trailing value", content: valid + "{}"},
		{
			name:    "wrong format",
			content: fmt.Sprintf(`{"format":%d,`, joinedGenerationVersion-1) + fields + "}",
		},
		{name: "incomplete", content: "{" + format + "}"},
		{name: "no generation", content: "{" + format + `,"state":"s","family_sha256":"` + digest + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if read(t, test.content) == nil {
				t.Fatal("invalid joined descriptor was accepted")
			}
		})
	}
}

// TestJoinedGenerationBindsTheOverlay binds a base and an overlay: the match
// needs both keys, and a node with no commit binds an overlay alone.
func TestJoinedGenerationBindsTheOverlay(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	indexDir := filepath.Join(root, "index")
	store := corpusStore{cacheDir: cacheDir, indexDir: indexDir}
	for _, dir := range []string{cacheDir, indexDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(indexDir, familyManifestFile), []byte("seek_v17.00000.zoekt 481\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const state, base = "git-state-3", "head-3"
	overlay := semanticOverlaySource(state)
	units, embeddings := testSemanticRowsAndVectors(t)
	for _, source := range []string{base, overlay} {
		if _, err := writeTestSemanticGeneration(t.Context(), semanticGenerationDir(indexDir, source), source, units, embeddings); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.writeJoinedGenerations(state, base, overlay); err != nil {
		t.Fatal(err)
	}
	if !store.joinedGenerationsMatch(state, base, overlay) {
		t.Fatal("base and overlay binding does not match")
	}
	if store.joinedGenerationsMatch(state, base, "") {
		t.Fatal("a base-only read matched a binding with an overlay")
	}
	if store.joinedGenerationsMatch(state, base, semanticOverlaySource("other-state")) {
		t.Fatal("another state's overlay matched")
	}
	if err := store.writeJoinedGenerations(state, "", overlay); err != nil {
		t.Fatal(err)
	}
	if !store.joinedGenerationsMatch(state, "", overlay) {
		t.Fatal("an overlay-only binding does not match")
	}
}

// TestOldOverlayWithAGraph reads an overlay generation that an older build
// wrote with a USearch graph. The manifest check of the build and of the open
// rejects it, so the build replaces it before a search reads it. Reuse still
// reads its rows.
func TestOldOverlayWithAGraph(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "generation")
	units, embeddings := testSemanticRowsAndVectors(t)
	if _, err := writeTestSemanticGeneration(t.Context(), dir, "head-old", units, embeddings); err != nil {
		t.Fatal(err)
	}
	overlay := semanticOverlaySource("state-old")
	path := filepath.Join(dir, semanticManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["source"] = overlay
	if data, err = json.Marshal(manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSemanticManifest(dir, overlay); err == nil {
		t.Fatal("the manifest check accepted an overlay with a graph")
	}
	generation, err := openSemanticGenerationRows(dir, overlay)
	if err != nil {
		t.Fatalf("reuse cannot read the old overlay: %v", err)
	}
	defer func() { _ = generation.Close() }()
	if len(generation.rows) != len(units) || len(generation.usearch) != 0 {
		t.Fatalf("rows=%d usearch=%d, want %d rows and no shard", len(generation.rows), len(generation.usearch), len(units))
	}
}

// writeJoinedGeneration binds state to source with no overlay.
func writeJoinedGeneration(
	cacheDir string,
	indexDir string,
	state string,
	source string,
) error {
	return corpusStore{cacheDir: cacheDir, indexDir: indexDir}.writeJoinedGenerations(state, source, "")
}
