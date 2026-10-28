package main

import (
	"math"
	"slices"
	"testing"
)

// TestMeanMaxSimPassesStrictBoundary proves that a lift equal to the
// threshold (pipelineTauFile) fails and the next float32 above it passes. It
// also proves that the rule subtracts the empty-document score and then
// divides by the token count.
func TestMeanMaxSimPassesStrictBoundary(t *testing.T) {
	const threshold = pipelineTauFile
	if meanMaxSimPasses(threshold, 0, 1) {
		t.Fatal("threshold equality passed")
	}
	if meanMaxSimPasses(math.Nextafter32(threshold, 0), 0, 1) {
		t.Fatal("the float32 below the threshold passed")
	}
	if !meanMaxSimPasses(math.Nextafter32(threshold, 1), 0, 1) {
		t.Fatal("the float32 above the threshold did not pass")
	}
	// Over 4 tokens, (3.6 - 3.2) / 4 = 0.1 is under the threshold of 0.25.
	// (3.6 - 2.0) / 4 = 0.4 is above it. The score alone, 3.6 / 4 = 0.9, and
	// the difference alone, 0.4, are above it.
	if meanMaxSimPasses(3.6, 3.2, 4) {
		t.Fatal("a score with a lift under the threshold passed")
	}
	if !meanMaxSimPasses(3.6, 2.0, 4) {
		t.Fatal("a score with a lift above the threshold did not pass")
	}
}

// TestRerankAcceptanceCalibrationContract pins the inputs of the per-file
// acceptance: tau_file, the retrieval delta, the rank fusion, and the model
// compatibility of their measurement. When one changes, measure them again.
func TestRerankAcceptanceCalibrationContract(t *testing.T) {
	const wantCompatibility = "lateon-code-edge-proxy-no-symbol-centroids-v5-76339ee2c2f5a452923bf85c0a72d6bb"
	if pipelineTauFile != 0.25 {
		t.Fatalf("tau_file=%g, want 0.25", pipelineTauFile)
	}
	if semanticRetrievalDelta != 0.1158 {
		t.Fatalf("delta=%g, want 0.1158", semanticRetrievalDelta)
	}
	if rerankRRFConstant != 60 || rerankLexicalWeight != 2 {
		t.Fatalf("rank fusion k=%g weight=%g, want 60 and 2", rerankRRFConstant, rerankLexicalWeight)
	}
	if got := semanticModelCompatibility(); got != wantCompatibility {
		t.Fatalf("bundled compatibility=%q, want %q", got, wantCompatibility)
	}
}

// acceptanceRetryGo is one retry function, the text that follows the package
// line. acceptanceRealFiles holds it at four paths.
const acceptanceRetryGo = "\n// retryOp runs op until it succeeds, and doubles the wait after each failed attempt.\nfunc retryOp(ctx context.Context, op func() error) error {\n\twait := 100 * time.Millisecond\n\tfor attempt := 0; attempt < 5; attempt++ {\n\t\tif err := op(); err == nil {\n\t\t\treturn nil\n\t\t}\n\t\ttime.Sleep(wait)\n\t\twait *= 2\n\t}\n\treturn errors.New(\"gave up\")\n}\n"

// acceptanceRetryCopies are the paths of acceptanceRetryGo in
// acceptanceRealFiles.
var acceptanceRetryCopies = []string{"internal/x.go", "net/backoff.go", "net/retry.go", "rank/r1.go"}

// acceptanceRealFiles is a small repository of real text: seven files on other
// topics and the four copies of acceptanceRetryGo. Each copy has the package
// line of its folder, and net/backoff.go also has a copyright line, so no two
// copies have the same bytes. Copies with the same bytes become one result,
// and the model scores only one of them.
var acceptanceRealFiles = [][2]string{
	{"auth/token.go", "package auth\n\n// authenticateRequest checks the bearer token of an incoming request.\nfunc authenticateRequest(r *http.Request) error {\n\ttoken := strings.TrimPrefix(r.Header.Get(\"Authorization\"), \"Bearer \")\n\treturn validateToken(token)\n}\n"},
	{"ui/colors.go", "package ui\n\n// terminalColorCode maps a color name to its ANSI code.\nfunc terminalColorCode(name string) int { return ansiColors[name] }\n"},
	{"cache/lru.go", "package cache\n\n// evict removes the least recently used entry when the cache is full.\nfunc (c *LRU) evict() {\n\toldest := c.list.Back()\n\tc.list.Remove(oldest)\n\tdelete(c.items, oldest.Value.(*entry).key)\n}\n"},
	{"config/load.go", "package config\n\n// Load reads the YAML configuration file and fills in the defaults.\nfunc Load(path string) (*Config, error) {\n\tdata, err := os.ReadFile(path)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tvar c Config\n\treturn &c, yaml.Unmarshal(data, &c)\n}\n"},
	{"docs/install.md", "# Install\n\nInstall the tool with Homebrew. Configure Universal Ctags on the PATH.\n"},
	{"db/migrate.go", "package db\n\n// Migrate applies every pending schema migration in a transaction.\nfunc Migrate(tx *sql.Tx, steps []string) error {\n\tfor _, step := range steps {\n\t\tif _, err := tx.Exec(step); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n\treturn nil\n}\n"},
	{"gen/zq.go", "package gen\n\n// zqnear30 zqmodel9 is a generated identifier table.\nvar zqTable = map[string]int{\"zqnear30\": 1}\n"},
	{"net/retry.go", "package net\n" + acceptanceRetryGo},
	{"net/backoff.go", "// Copyright 2026 The Authors.\n\npackage net\n" + acceptanceRetryGo},
	{"rank/r1.go", "package rank\n" + acceptanceRetryGo},
	{"internal/x.go", "package internal\n" + acceptanceRetryGo},
}

// TestModelAcceptanceOnRealText runs two descriptions with the real model on a
// Git repository of real text (acceptanceRealFiles). The model paths are
// relative to the repository, so no temporary name enters the model input.
//
// A sentence of 51 tokens with no topic admits no file, and the model scores
// no candidate. Each of its tokens is about as similar to a file as to the
// empty document. So no row has a fine lift in the radius. A threshold on the
// score per token alone admits each file for it (0.742 to 0.756 per token).
// The cause is that each token with no topic adds to the score of any text.
//
// No file holds the word "operations", so no file is a strict match for
// "retry failed operations", and only the model adds the retry function. The
// model scores each of its four copies and no other file, because no other
// file has a row in the retrieval radius. The four copies print, and no other
// file prints: each copy gets the same verdict. Their lifts are 0.310 to
// 0.335 per token on the CPU and Core ML providers.
//
// The test costs 4 model batches: the build, two queries, and one score call.
func TestModelAcceptanceOnRealText(t *testing.T) {
	requireTools(t)
	repo := initEmptyGitRepo(t)
	for _, file := range acceptanceRealFiles {
		writeFileAt(t, repo, file[0], file[1])
	}
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "real text")
	setTestUserCache(t)
	t.Chdir(repo)
	obs := observeModel(sharedModel(t))

	const noTopic = "I want to find the code that does the thing that I am looking for here, and I would like to see where it is done and how it works in this project, so please show me the part of the code that does it."
	output, err := seekWith(t, obs.factory(), noTopic, ".")
	_, report := obs.scoredPaths()
	if exitCodeForError(err) != 1 || output != "" {
		t.Fatalf("a sentence with no topic: exit status %d (err=%v), want 1 and no output; %s\n%s",
			exitCodeForError(err), err, report, output)
	}
	if obs.prepares.Load() == 0 || obs.scores.Load() != 0 {
		t.Fatalf("a sentence with no topic: the model prepared %d queries and made %d score calls, want no score call; %s",
			obs.prepares.Load(), obs.scores.Load(), report)
	}

	output, err = seekWith(t, obs.factory(), "retry failed operations", ".")
	_, report = obs.scoredPaths()
	if got := printedFiles(output); err != nil || !slices.Equal(got, acceptanceRetryCopies) {
		t.Fatalf("retry failed operations: err=%v printed %v, want the copies %v; %s\n%s",
			err, got, acceptanceRetryCopies, report, output)
	}
	obs.mu.Lock()
	var scored []string
	for _, record := range obs.scored {
		scored = append(scored, record.document.Path)
	}
	obs.mu.Unlock()
	slices.Sort(scored)
	if !slices.Equal(scored, acceptanceRetryCopies) {
		t.Fatalf("retry failed operations: the model scored %v, want each copy once %v; %s\n%s",
			scored, acceptanceRetryCopies, report, output)
	}
}
