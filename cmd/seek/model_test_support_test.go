package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// processLateOn is the LateOn model that the tests of this process share. A
// model start costs an ONNX Runtime session and, on macOS, a Core ML compile
// and a provider check. With one model, each later test pays only its
// inference. No test closes it: the process ends after the last test, and
// TestLateOnModelCloseAtEachStage tests Close on models of its own.
var processLateOn struct {
	mu       sync.Mutex
	model    *lateOnModel
	provider string
}

// processProviderPin is the provider pin of the test process, read before any
// test sets SEEK_PROVIDER. A test that pins another provider uses its own
// model.
var processProviderPin = os.Getenv(lateOnProviderEnv)

// sharedLateOnModel starts the process model on first use, so a test run that
// does not use the model does not pay its start.
//
// A test that sets SEEK_PROVIDER to a value that is not the process value gets
// an error at every call: the process model keeps the provider of its start,
// so without the error the test would run that provider in silence.
//
// The start builds the query encoder and then the index-build encoder, under a
// context that no test cancels. The model keeps the error of its first encoder
// build for its life (semanticRowEncoder), so a lazy build in a test whose
// context ends would break the model for every later test. The query encoder
// comes first, as in a search command, so the CPU sessions get the thread
// counts of a search.
//
// Only a started model stays. A start that fails, for example in a test whose
// cache root is not usable, returns its error, and the next call starts again,
// so one bad start does not fail every later test.
//
// The runtime and the compiled model files go to the cache root of the test
// that starts the model. The model reads them only while it starts. A test
// that lists or counts the files of its cache root uses its own model or no
// model.
func sharedLateOnModel() (*lateOnModel, error) {
	if pin := os.Getenv(lateOnProviderEnv); pin != processProviderPin {
		return nil, fmt.Errorf("%s is %q in this test and %q in the process: a test that pins a provider uses its own model",
			lateOnProviderEnv, pin, processProviderPin)
	}
	processLateOn.mu.Lock()
	defer processLateOn.mu.Unlock()
	if processLateOn.model != nil {
		return processLateOn.model, nil
	}
	ctx := context.Background()
	model, err := newLateOnSemanticModel(ctx)
	if err != nil {
		return nil, err
	}
	lateOn := model.(*lateOnModel)
	for _, interactive := range []bool{true, false} {
		if _, err := lateOn.semanticRowEncoder(ctx, interactive); err != nil {
			return nil, errors.Join(err, lateOn.Close())
		}
	}
	processLateOn.model = lateOn
	processLateOn.provider = semanticProviderName()
	return lateOn, nil
}

// sharedModelProvider returns the provider of the process model, or "" before
// it starts. Use it in place of semanticProviderName for the process model: a
// test that builds its own model changes semanticProviderName.
func sharedModelProvider() string {
	processLateOn.mu.Lock()
	defer processLateOn.mu.Unlock()
	return processLateOn.provider
}

// processModel is the process model as one test or command sees it. Close does
// nothing, because every test shares the model.
type processModel struct{ *lateOnModel }

func (processModel) Close() error { return nil }

// sharedModel returns the process model, or stops the test if the model cannot
// start.
func sharedModel(tb testing.TB) processModel {
	tb.Helper()
	model, err := sharedLateOnModel()
	if err != nil {
		tb.Fatalf("start the LateOn model: %v", err)
	}
	return processModel{model}
}

// stableModelRoot returns a new folder, t.TempDir()/fixture/src/app, for a
// folder node whose real-model scores a test checks. The model reads the path
// of each file of a folder node from the search root (modelPathBase) and keeps
// its last rerankPathComponentLimit parts (rerankPathTail). Under a bare
// t.TempDir(), these parts hold the random name of the temporary folder, so a
// score changes from run to run. Under this folder, the model reads
// fixture/src/app/<name> in each run.
func stableModelRoot(tb testing.TB) string {
	tb.Helper()
	root := filepath.Join(tb.TempDir(), "fixture", "src", "app")
	if err := os.MkdirAll(root, 0o755); err != nil {
		tb.Fatal(err)
	}
	return root
}

// observedScore is one document that the search sent to the model, with the
// raw score that the model returned for it. The score is NaN when the call
// failed or gave no score for each document. It does not hold the MeanMaxSim:
// acceptance divides the score by its own rule, and a copy of that rule here
// could show a value that acceptance does not use.
type observedScore struct {
	document rerankDocument
	score    float32
}

// observedModel passes every call to its model and records the call. Each
// stage method counts the call and records its input before it does more, so
// a test sees the input of a call that fails. Then the embed and score stages
// run the hook of the stage if the test set one: a hook error that is not nil
// is the result of the stage, and the model does not run. A hook can also act
// during the call, for example edit a file, and return nil. The wrapper
// changes no input and no result, so the inner model can be the real model
// (sharedModel(t)) or a test model. Set the hooks before the search starts.
//
// Close counts the call and does nothing more, so the test controls the life
// of the inner model: each command closes its model at its end, and a test can
// give one observed model to more than one command.
type observedModel struct {
	semanticModel

	starts, prepares, embeds, scores, closes atomic.Int32

	embed func(ctx context.Context, units []semanticUnit) error
	score func(ctx context.Context, documents []rerankDocument) error

	mu sync.Mutex
	// queries holds the text of each prepared query, units a copy of each
	// unit to embed, and scored each document of each score call, all in call
	// order.
	queries []string
	units   []semanticUnit
	scored  []observedScore
}

// observeModel records the calls of inner.
func observeModel(inner semanticModel) *observedModel {
	return &observedModel{semanticModel: inner}
}

// factory returns a factory that counts its calls in starts and gives o.
func (o *observedModel) factory() semanticModelFactory {
	return func(context.Context) (semanticModel, error) {
		o.starts.Add(1)
		return o, nil
	}
}

func (o *observedModel) PrepareSemanticQuery(ctx context.Context, query string) (*semanticQueryEmbedding, error) {
	o.prepares.Add(1)
	o.mu.Lock()
	o.queries = append(o.queries, query)
	o.mu.Unlock()
	return o.semanticModel.PrepareSemanticQuery(ctx, query)
}

// EmbedSemanticUnits records a copy of each unit with its own text and model
// input. The text of a unit is a view of the file bytes, which the build holds
// only while a model call reads them, and the build clears both fields in the
// rows that it keeps.
func (o *observedModel) EmbedSemanticUnits(ctx context.Context, units []semanticUnit) ([]semanticUnitEmbedding, error) {
	o.embeds.Add(1)
	o.mu.Lock()
	for _, unit := range units {
		unit.text = bytes.Clone(unit.text)
		unit.modelInput = slices.Clone(unit.modelInput)
		o.units = append(o.units, unit)
	}
	o.mu.Unlock()
	if o.embed != nil {
		if err := o.embed(ctx, units); err != nil {
			return nil, err
		}
	}
	return o.semanticModel.EmbedSemanticUnits(ctx, units)
}

// ScoresWithSemanticQuery records each document with a NaN score, then sets
// the scores when the model gives one score for each document.
func (o *observedModel) ScoresWithSemanticQuery(
	ctx context.Context,
	prepared *semanticQueryEmbedding,
	documents []rerankDocument,
) ([]float32, error) {
	o.scores.Add(1)
	o.mu.Lock()
	first := len(o.scored)
	for _, document := range documents {
		o.scored = append(o.scored, observedScore{document: document, score: float32(math.NaN())})
	}
	o.mu.Unlock()
	if o.score != nil {
		if err := o.score(ctx, documents); err != nil {
			return nil, err
		}
	}
	scores, err := o.semanticModel.ScoresWithSemanticQuery(ctx, prepared, documents)
	if err == nil && len(scores) == len(documents) {
		o.mu.Lock()
		for index, score := range scores {
			o.scored[first+index].score = score
		}
		o.mu.Unlock()
	}
	return scores, err
}

func (o *observedModel) Close() error {
	o.closes.Add(1)
	return nil
}

// documentPaths returns the sorted path of each document. A document that
// the search sent twice shows twice.
func documentPaths(documents []rerankDocument) []string {
	paths := make([]string, 0, len(documents))
	for _, document := range documents {
		paths = append(paths, document.Path)
	}
	slices.Sort(paths)
	return paths
}

// scoredPaths returns the sorted, distinct node-relative paths of the
// documents that the search sent to the model, the documents of a failed call
// included, and, for a failure message, the provider of the process model and
// the raw score of each document: NaN for a document of a failed call.
func (o *observedModel) scoredPaths() (paths []string, report string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var lines strings.Builder
	fmt.Fprintf(&lines, "provider %s, raw model scores:", sharedModelProvider())
	documents := make([]rerankDocument, 0, len(o.scored))
	for _, scored := range o.scored {
		documents = append(documents, scored.document)
		fmt.Fprintf(&lines, "\n  %s %.4f", scored.document.Path, scored.score)
	}
	return slices.Compact(documentPaths(documents)), lines.String()
}

// seek runs args through the root command as main does, with the process
// model, and returns what the command writes to stdout. A test of the lexical
// search passes --lexical-only.
func seek(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return seekWith(t, nil, args...)
}

// seekWith is seek with newModel in place of the process model. A nil
// newModel gives the process model.
func seekWith(t *testing.T, newModel semanticModelFactory, args ...string) (string, error) {
	t.Helper()
	return captureStdout(t, func() error { return seekRoot(t.Context(), t, newModel, args...) })
}

// modelFactory returns a factory that gives model at each call. The command
// closes the model at its end, and a test model keeps its counters and records
// after Close, so the test reads them after the command. It is a function and
// not a method of hybridTestScorer: a type that embeds hybridTestScorer would
// get the method, and the factory would give the inner model.
func modelFactory(model semanticModel) semanticModelFactory {
	return func(context.Context) (semanticModel, error) { return model, nil }
}

// failingModelFactory returns a factory that counts its calls in starts and
// fails each call with a test error. A command that must not start the model
// leaves starts at 0; a search whose model cannot start asks once and prints
// its strict results.
func failingModelFactory(starts *atomic.Int32) semanticModelFactory {
	return func(context.Context) (semanticModel, error) {
		starts.Add(1)
		return nil, errors.New("the test model factory fails")
	}
}

// stubSearch runs args through the root command, as main does, with a new
// hybridTestScorer in mode, and returns the output, the model and the error.
// With payloadAccept every row is a candidate and the model accepts every
// candidate, so a test proves which rows a search reads and which files print,
// not which files a real model finds. With payloadReject every row is a
// candidate and the model rejects each one, and with payloadFar no row is a
// candidate: only the strict hits print, and a test proves which rows a build
// writes and which documents the model scores.
func stubSearch(t *testing.T, mode payloadMode, args ...string) (string, *hybridTestScorer, error) {
	t.Helper()
	scorer := &hybridTestScorer{mode: mode}
	output, err := seekWith(t, modelFactory(scorer), args...)
	return output, scorer, err
}

// printedResult is one result in the output of a search.
type printedResult struct {
	// header is the text of the header after "## ": the path, the language
	// in parentheses and the tags in brackets.
	header string
	// path is the path at the start of the header. It can hold spaces.
	path string
	// also holds the paths of the "also:" line under the header: the copies
	// with the same content.
	also []string
	// body holds each line under the header, with no newline, up to the blank
	// line before the next header: the "also:" line, then the lines of the
	// file. A blank line ("") in body separates two regions of the file. The
	// body of the last result also holds what the search prints after it,
	// each part after a blank line: the "… N more files" footer and the
	// name-only notice.
	body []string
}

// printedHeader matches the text of a result header after "## " as the
// formatter writes it (formatCorpusFileMatch): the path (group 1), " (", the
// language, ")", and a " [tag]" for each tag. A language name can hold one
// pair of parentheses, as "Graphviz (DOT)" does. The path is the longest
// prefix that leaves a language and tags, so a path can hold " (".
var printedHeader = regexp.MustCompile(`^(.+) \((?:[^()]|\([^()]*\))+\)(?: \[[a-z]+\])*$`)

// printedResults returns each result of output, in print order. The tests read
// the results of the search output through it, so they agree on one parse of
// the format. A "## " line that does not match printedHeader is a defect of
// the output: its result keeps the whole header text as its path, so a test
// that compares paths fails and shows the header.
func printedResults(output string) []printedResult {
	var results []printedResult
	for line := range strings.Lines(output) {
		line = strings.TrimSuffix(line, "\n")
		if header, ok := strings.CutPrefix(line, "## "); ok {
			if last := len(results) - 1; last >= 0 {
				// The blank line before a header ends the result above.
				body := results[last].body
				if len(body) > 0 && body[len(body)-1] == "" {
					results[last].body = body[:len(body)-1]
				}
			}
			result := printedResult{header: header, path: header}
			if match := printedHeader.FindStringSubmatch(header); match != nil {
				result.path = match[1]
			}
			results = append(results, result)
			continue
		}
		if len(results) == 0 {
			continue
		}
		result := &results[len(results)-1]
		if also, ok := strings.CutPrefix(line, "also: "); ok && len(result.body) == 0 {
			result.also = strings.Split(also, ", ")
		}
		result.body = append(result.body, line)
	}
	return results
}

// headerPaths returns the path of each result header of output, sorted.
func headerPaths(output string) []string {
	var paths []string
	for _, result := range printedResults(output) {
		paths = append(paths, result.path)
	}
	slices.Sort(paths)
	return paths
}

// headerOrder returns the text of each result header of output, with its
// language and tags, in print order.
func headerOrder(output string) []string {
	var headers []string
	for _, result := range printedResults(output) {
		headers = append(headers, result.header)
	}
	return headers
}

// headerTexts returns the text of each result header of output, with its
// language and tags, sorted.
func headerTexts(output string) []string {
	headers := headerOrder(output)
	slices.Sort(headers)
	return headers
}

// firstRegions maps the header text of each result of output to the lines of
// its body up to the first blank line, each with its newline: the "also:" line
// and the first region of the file.
func firstRegions(output string) map[string]string {
	regions := make(map[string]string)
	for _, result := range printedResults(output) {
		var text strings.Builder
		for _, line := range result.body {
			if line == "" {
				break
			}
			text.WriteString(line + "\n")
		}
		regions[result.header] = text.String()
	}
	return regions
}

// bodyTexts maps the path of each result of output to each line of its body
// that is not blank, with its newline: the "also:" line and every region of
// the file, and, for the last result, the footer and the notice after it.
func bodyTexts(output string) map[string]string {
	texts := make(map[string]string)
	for _, result := range printedResults(output) {
		var text strings.Builder
		for _, line := range result.body {
			if line != "" {
				text.WriteString(line + "\n")
			}
		}
		texts[result.path] = text.String()
	}
	return texts
}

// printedFiles returns the path of each file that output prints, sorted: the
// path of each result header and of each copy on its "also:" line.
func printedFiles(output string) []string {
	var paths []string
	for _, result := range printedResults(output) {
		paths = append(paths, result.path)
		paths = append(paths, result.also...)
	}
	slices.Sort(paths)
	return paths
}

// TestPrintedResultsReadsSearchOutput parses the output of a search of a Git
// node and a folder node, whose file names hold spaces. The output has each
// form of result: the [uncommitted], [git] and [folder] tags, a language with
// parentheses, an "also:" line, a name-only result (a binary file that
// matches by name), and a file with two regions of lines. The description
// takes the model route, where the copies with the same bytes print as one
// result. The test model in its far mode adds no file, so the strict hits
// print. A "-n 1" search then puts the footer in the body of its last result,
// and a header with no text gives no panic.
func TestPrintedResultsReadsSearchOutput(t *testing.T) {
	requireTools(t)
	f := newGraphFixture(t)
	repo := f.GitRepo("tree/repo", "README.md", "readme\n")
	writeFixtureFile(t, filepath.Join(repo, "net", "retry file.go"), "package net\n// zqparse zqmark retry\n")
	writeFixtureFile(t, filepath.Join(repo, "copy a.txt"), "zqparse zqmark copy\n")
	writeFixtureFile(t, filepath.Join(repo, "copy b.txt"), "zqparse zqmark copy\n")
	gitRunIn(t, repo, "add", ".")
	gitRunIn(t, repo, "commit", "-q", "-m", "parse")
	writeFixtureFile(t, filepath.Join(repo, "new file.go"), "package repo\n// zqparse zqmark new\n")
	loose := f.path("tree/loose")
	writeFixtureFile(t, filepath.Join(loose, "graph (v1).gv"), "digraph g {\n  // zqparse zqmark graph\n  a -> b\n}\n")
	writeFixtureFile(t, filepath.Join(loose, "zqparse zqmark.bin"), "\x00\x01binary\x00")
	writeFixtureFile(t, filepath.Join(loose, "gap.txt"),
		"zqparse zqmark one\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\nzqparse zqmark two\n")
	setTestUserCache(t)
	t.Chdir(t.TempDir())

	output, _, err := stubSearch(t, payloadFar, "zqparse zqmark", repo, loose)
	if err != nil {
		t.Fatalf("search: %v\n%s", err, output)
	}
	path := func(node, name string) string { return filepath.Join(node, filepath.FromSlash(name)) }
	result := func(file, header string, also []string, body ...string) printedResult {
		return printedResult{header: file + header, path: file, also: also, body: body}
	}
	binary := path(loose, "zqparse zqmark.bin")
	want := []printedResult{
		result(path(loose, "gap.txt"), " (Text) [folder]", nil,
			"1 zqparse zqmark one", "2 2", "3 3", "4 4", "", "10 10", "11 11", "12 12", "13 zqparse zqmark two"),
		result(path(loose, "graph (v1).gv"), " (Graphviz (DOT)) [folder]", nil,
			"1 digraph g {", "2   // zqparse zqmark graph", "3   a -> b", "4 }"),
		result(binary, " (unknown) [folder]", nil, "0 "+binary),
		result(path(repo, "copy a.txt"), " (Text) [git]", []string{path(repo, "copy b.txt")},
			"also: "+path(repo, "copy b.txt"), "1 zqparse zqmark copy"),
		result(path(repo, "net/retry file.go"), " (Go) [git]", nil, "1 package net", "2 // zqparse zqmark retry"),
		result(path(repo, "new file.go"), " (Go) [uncommitted] [git]", nil, "1 package repo", "2 // zqparse zqmark new"),
	}
	got := printedResults(output)
	slices.SortFunc(got, func(left, right printedResult) int { return strings.Compare(left.path, right.path) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed\n%q\nwant\n%q\nfrom the output:\n%s", got, want, output)
	}

	limited, _, err := stubSearch(t, payloadFar, "-n", "1", "zqparse zqmark", repo, loose)
	results := printedResults(limited)
	if err != nil || len(results) != 1 || !slices.Equal(results[0].body[max(0, len(results[0].body)-2):],
		[]string{"", "… 5 more files (showing 1 of 6)"}) {
		t.Fatalf("-n 1: err=%v, parsed %q, want one result whose body ends with the footer:\n%s", err, results, limited)
	}

	if got := printedResults("## \n1 text\n"); len(got) != 1 || got[0].path != "" || !slices.Equal(got[0].body, []string{"1 text"}) {
		t.Fatalf("a header with no text gave %q, want one result with no path", got)
	}
}

// testLoggerMu makes the logger swap of seekRoot one step, so searches that run
// at the same time cannot leave the CLI logger in place.
var testLoggerMu sync.Mutex

// seekRoot runs the root command as main does, under ctx: the separator splice,
// the flag parse, PersistentPreRunE, PreRunE and RunE. It writes the results to
// os.Stdout. Use it for a search that the test cancels or runs beside another
// search: captureStdout changes the process stdout, so it cannot capture two
// searches at the same time.
//
// A nil newModel gives the process model. Its factory starts the model only
// when the search asks for it, so a lexical search pays nothing. A model that
// cannot start fails tb: else the search would fall back to the strict order
// and could pass a test of a model result.
//
// The root command installs the CLI logger, which writes to the process
// stderr. seekRoot puts back the logger of the test after PersistentPreRunE,
// so captureTestLogs keeps its records and the log output that TestMain
// discards stays discarded. The process tests test the CLI logger.
func seekRoot(ctx context.Context, tb testing.TB, newModel semanticModelFactory, args ...string) error {
	tb.Helper()
	if newModel == nil {
		newModel = func(context.Context) (semanticModel, error) {
			model, err := sharedLateOnModel()
			if err != nil {
				tb.Errorf("start the LateOn model: %v", err)
				return nil, err
			}
			return processModel{model}, nil
		}
	}
	root := newRootCmd(newModel)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	preRun := root.PersistentPreRunE
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		testLoggerMu.Lock()
		defer testLoggerMu.Unlock()
		logger, writer, flags := slog.Default(), log.Writer(), log.Flags()
		err := preRun(cmd, args)
		slog.SetDefault(logger)
		log.SetOutput(writer)
		log.SetFlags(flags)
		return err
	}
	root.SetArgs(splicePassthroughSeparator(args, root.Flags()))
	return root.ExecuteContext(ctx)
}
