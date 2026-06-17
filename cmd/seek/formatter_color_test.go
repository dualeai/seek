package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sourcegraph/zoekt"
)

// ansiCodeRE matches any SGR escape, for proving color is presentation-only.
var ansiCodeRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// fmtOneFile formats a single git-corpus file match with the given palette and
// hideCorpusContext (relative paths), the common shape for these unit tests.
func fmtOneFile(fm zoekt.FileMatch, pal palette) string {
	res := []corpusSearchResult{{corpusID: corpusID("c"), kind: corpusKindGit, file: fm}}
	return formatCorpusResultsWithContext(res, nil, 0, 0, hideCorpusContext, pal)
}

func lineMatch(line string, lineNum uint32, frags ...zoekt.LineFragmentMatch) zoekt.LineMatch {
	return zoekt.LineMatch{Line: []byte(line), LineNumber: int(lineNum), LineFragments: frags}
}

func frag(offset, length int) zoekt.LineFragmentMatch {
	return zoekt.LineFragmentMatch{LineOffset: offset, MatchLength: length}
}

// --- Color -----------------------------------------------------------------

func TestColor_PlainHasNoEscapes(t *testing.T) {
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("the needle here\n", 2, frag(4, 6))}}
	out := fmtOneFile(fm, plainPalette)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("plain palette must not emit ANSI, got:\n%q", out)
	}
	if out != "## a.go (Go)\n2 the needle here" {
		t.Fatalf("unexpected plain output:\n%q", out)
	}
}

func TestColor_MatchSpanWrapped(t *testing.T) {
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("the needle here\n", 2, frag(4, 6))}}
	out := fmtOneFile(fm, ansiPalette)
	if !strings.Contains(out, ansiPalette.match+"needle"+ansiPalette.reset) {
		t.Fatalf("expected matched token wrapped in color, got:\n%q", out)
	}
	if !strings.Contains(out, ansiPalette.file+"a.go"+ansiPalette.reset) {
		t.Fatalf("expected colored filename, got:\n%q", out)
	}
}

func TestColor_AdjacentFragmentsCoalesce(t *testing.T) {
	// "foo" [0,3) and "bar" [3,6) are touching → one color pair, not two.
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("foobar baz\n", 1, frag(0, 3), frag(3, 3))}}
	out := fmtOneFile(fm, ansiPalette)
	if n := strings.Count(out, ansiPalette.match); n != 1 {
		t.Fatalf("expected 1 coalesced color span, got %d in:\n%q", n, out)
	}
	if !strings.Contains(out, ansiPalette.match+"foobar"+ansiPalette.reset) {
		t.Fatalf("expected coalesced span to cover foobar, got:\n%q", out)
	}
}

func TestColor_MultibyteAroundMatchNotSplit(t *testing.T) {
	// "café " is 6 bytes (é = 2); "needle" starts at byte offset 6.
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("café needle\n", 1, frag(6, 6))}}
	out := fmtOneFile(fm, ansiPalette)
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8 (rune split): %q", out)
	}
	if !strings.Contains(out, "café ") || !strings.Contains(out, ansiPalette.match+"needle") {
		t.Fatalf("expected intact café and colored needle, got:\n%q", out)
	}
}

// TestColor_AnsiPaletteCodes pins the literal escape sequences. Without this,
// the behavioral color tests (which reference ansiPalette.* by name) would still
// pass if the palette were set to wrong/empty codes — this is their oracle.
func TestColor_AnsiPaletteCodes(t *testing.T) {
	want := palette{file: "\x1b[36m", lineNo: "\x1b[2m", match: "\x1b[1;31m", reset: "\x1b[0m"}
	if ansiPalette != want {
		t.Fatalf("ansiPalette codes changed: got %+q want %+q", ansiPalette, want)
	}
	if plainPalette != (palette{}) {
		t.Fatalf("plainPalette must be the empty (no-op) palette, got %+q", plainPalette)
	}
}

// TestColor_AnsiStrippedEqualsPlain proves color is presentation-only: removing
// the SGR escapes from the colored output yields byte-identical plain output.
func TestColor_AnsiStrippedEqualsPlain(t *testing.T) {
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{
			{Line: []byte("func Handle() {\n"), LineNumber: 10, Before: []byte("// doc\n"), After: []byte("    return\n"),
				LineFragments: []zoekt.LineFragmentMatch{{LineOffset: 5, MatchLength: 6, SymbolInfo: &zoekt.Symbol{Kind: "function"}}}},
		}}
	plain := fmtOneFile(fm, plainPalette)
	colored := fmtOneFile(fm, ansiPalette)
	if stripped := ansiCodeRE.ReplaceAllString(colored, ""); stripped != plain {
		t.Fatalf("color not presentation-only:\nplain=    %q\nstripped= %q", plain, stripped)
	}
}

func TestColor_NonAdjacentMatchesBothColored(t *testing.T) {
	// "x ONE y TWO z": ONE@2, TWO@8 — separated, both must be colored.
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("x ONE y TWO z\n", 1, frag(2, 3), frag(8, 3))}}
	out := fmtOneFile(fm, ansiPalette)
	if n := strings.Count(out, ansiPalette.match); n != 2 {
		t.Fatalf("expected 2 separate spans, got %d in:\n%q", n, out)
	}
	if !strings.Contains(out, ansiPalette.match+"ONE"+ansiPalette.reset) ||
		!strings.Contains(out, ansiPalette.match+"TWO"+ansiPalette.reset) {
		t.Fatalf("both ONE and TWO must be highlighted, got:\n%q", out)
	}
}

// --- Match-aware long-line windowing ---------------------------------------

func TestLongLine_MatchDeepStaysVisible(t *testing.T) {
	line := strings.Repeat("x", 2000) + "NEEDLE" + strings.Repeat("y", 2000) + "\n"
	fm := zoekt.FileMatch{FileName: "min.js", Language: "JavaScript", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch(line, 1, frag(2000, 6))}}
	out := fmtOneFile(fm, plainPalette)
	if !strings.Contains(out, "NEEDLE") {
		t.Fatalf("match dropped by windowing, got:\n%q", out)
	}
	if !strings.Contains(out, "…+") {
		t.Fatalf("expected truncation markers, got:\n%q", out)
	}
	// Window keeps ~matchCtxBytes on each side of a 6-byte match; the bound is
	// tied to the const so it actually catches a too-wide window (a loose
	// `> 2000` would pass for any window ≥ 512 and test nothing).
	if maxBody := 2*matchCtxBytes + 256; len(out) > maxBody {
		t.Fatalf("windowed output should be ≤ %d bytes, got %d", maxBody, len(out))
	}
}

func TestLongLine_TwoMatchesFarApartBothVisible(t *testing.T) {
	line := strings.Repeat("a", 1500) + "FIRST" + strings.Repeat("b", 3000) + "SECOND" + strings.Repeat("c", 1500) + "\n"
	first := 1500
	second := 1500 + 5 + 3000
	fm := zoekt.FileMatch{FileName: "min.js", Language: "JavaScript", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch(line, 1, frag(first, 5), frag(second, 6))}}
	out := fmtOneFile(fm, plainPalette)
	if !strings.Contains(out, "FIRST") || !strings.Contains(out, "SECOND") {
		t.Fatalf("both matches must stay visible, got:\n%q", out)
	}
}

func TestLongLine_MatchAtStartNoHeadMarker(t *testing.T) {
	line := "HEAD" + strings.Repeat("z", 2000) + "\n"
	fm := zoekt.FileMatch{FileName: "min.js", Language: "JavaScript", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch(line, 1, frag(0, 4))}}
	out := fmtOneFile(fm, plainPalette)
	// Match at offset 0 → windowStart clamps to 0 → no leading "…+N bytes ".
	body := strings.SplitN(out, "\n", 2)[1]
	if strings.HasPrefix(strings.TrimPrefix(body, "1 "), "…+") {
		t.Fatalf("unexpected head marker for start-anchored match, got:\n%q", out)
	}
	if !strings.Contains(out, "HEAD") {
		t.Fatalf("expected HEAD visible, got:\n%q", out)
	}
}

func TestLongLine_ZeroLengthAndEOLFragmentsSkipped(t *testing.T) {
	// Zero-length and past-trimmed-newline fragments must not crash or color.
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("hello world\n", 1, frag(5, 0), frag(11, 1), frag(99, 4))}}
	out := fmtOneFile(fm, ansiPalette)
	if strings.Contains(out, ansiPalette.match) {
		t.Fatalf("no visible spans → no match color, got:\n%q", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("content must be intact, got:\n%q", out)
	}
}

func TestLongLine_ContextLineCapped(t *testing.T) {
	// A long before-context line is tail-trimmed (no match logic).
	big := strings.Repeat("z", 2000)
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{{
			Line:       []byte("match here\n"),
			LineNumber: 5,
			Before:     []byte(big + "\n"),
		}}}
	out := fmtOneFile(fm, plainPalette)
	if strings.Contains(out, big) {
		t.Fatalf("long context line should be capped, got %d bytes", len(out))
	}
	// 2000 source bytes, all ASCII → cut at maxLineBytes (no rune backup), so the
	// marker reports exactly 2000-maxLineBytes dropped. Tied to the const.
	wantMarker := fmt.Sprintf("…+%d bytes", 2000-maxLineBytes)
	if !strings.Contains(out, wantMarker) {
		t.Fatalf("expected context cap marker %q, got:\n%q", wantMarker, out)
	}
}

// --- Sanitization ----------------------------------------------------------

func TestSanitize_ControlBytesStrippedTabKept(t *testing.T) {
	fm := zoekt.FileMatch{
		FileName: "ev\x1bil.go", Language: "G\x07o", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("a\tb\x1b]0;x\x07 café\n", 1,
			zoekt.LineFragmentMatch{LineOffset: 0, MatchLength: 1, SymbolInfo: &zoekt.Symbol{Kind: "fu\x1bnc"}})},
	}
	out := fmtOneFile(fm, plainPalette)
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\x07") {
		t.Fatalf("control bytes leaked, got:\n%q", out)
	}
	if !strings.Contains(out, "evil.go") || !strings.Contains(out, "[func]") || !strings.Contains(out, "(Go)") {
		t.Fatalf("sanitized header/kind wrong, got:\n%q", out)
	}
	if !strings.Contains(out, "a\tb") || !strings.Contains(out, "café") {
		t.Fatalf("tab and multibyte must be preserved, got:\n%q", out)
	}
}

func TestSanitize_ControlInsideMatchColorStillValid(t *testing.T) {
	// ESC embedded inside the matched span is dropped; color codes stay paired
	// and the only escapes present are our palette codes.
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("x ne\x1bedle y\n", 1, frag(2, 7))}}
	out := fmtOneFile(fm, ansiPalette)
	// The embedded ESC is dropped; the only ESC bytes left are our palette
	// codes, and every match-start is paired with a reset.
	if strings.Contains(out, "ne\x1bedle") {
		t.Fatalf("embedded ESC must be stripped, got:\n%q", out)
	}
	if !strings.Contains(out, ansiPalette.match+"needle"+ansiPalette.reset) {
		t.Fatalf("expected sanitized colored match, got:\n%q", out)
	}
	if strings.Count(out, ansiPalette.match) != 1 {
		t.Fatalf("expected exactly one match span, got:\n%q", out)
	}
}

// --- Truncation notices ----------------------------------------------------

func TestNotices_FileLimit(t *testing.T) {
	var files []zoekt.FileMatch
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		files = append(files, zoekt.FileMatch{FileName: name, Language: "Go", Score: 1,
			LineMatches: []zoekt.LineMatch{lineMatch("x\n", 1)}})
	}
	out := formatGitCorpusResultsForTest(files, nil, 2, 0)
	if !strings.Contains(out, "… 1 more files (showing 2 of 3)") {
		t.Fatalf("expected file-limit notice, got:\n%q", out)
	}
}

func TestNotices_PerFileMatchLimit(t *testing.T) {
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("m1\n", 1), lineMatch("m2\n", 2), lineMatch("m3\n", 3)}}
	out := formatGitCorpusResultsForTest([]zoekt.FileMatch{fm}, nil, 0, 2)
	if !strings.Contains(out, "… 1 more matches in this file") {
		t.Fatalf("expected per-file match notice, got:\n%q", out)
	}
}

func TestNotices_NoneWhenNothingDropped(t *testing.T) {
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("only\n", 1)}}
	out := formatGitCorpusResultsForTest([]zoekt.FileMatch{fm}, nil, 5, 5)
	if strings.Contains(out, "more files") || strings.Contains(out, "more matches") {
		t.Fatalf("no notice expected when nothing dropped, got:\n%q", out)
	}
}

func TestLongLine_WindowCutLandsOnRuneBoundary(t *testing.T) {
	// Place a 4-byte emoji so the computed window start (matchStart-matchCtxBytes)
	// lands MID-rune. Without backupToRuneBoundary the window would slice the
	// emoji and emit broken UTF-8.
	const emoji = "🎉" // U+1F389, 4 bytes
	matchStart := matchCtxBytes + 100
	pre := strings.Repeat("a", matchCtxBytes-2) // emoji begins 2 bytes before winStart
	gap := strings.Repeat("b", matchStart-len(pre)-len(emoji))
	line := pre + emoji + gap + "MATCH" + strings.Repeat("c", matchCtxBytes+200) + "\n"
	if len(line) <= maxLineBytes {
		t.Fatalf("fixture must exceed maxLineBytes to trigger windowing")
	}
	fm := zoekt.FileMatch{FileName: "min.js", Language: "JavaScript", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch(line, 1, frag(matchStart, 5))}}
	out := fmtOneFile(fm, plainPalette)
	if !utf8.ValidString(out) {
		t.Fatalf("window cut split a rune (invalid UTF-8): %q", out)
	}
	if !strings.Contains(out, emoji) {
		t.Fatalf("the emoji straddling the window cut must survive intact, got:\n%q", out)
	}
	if !strings.Contains(out, "MATCH") {
		t.Fatalf("match must stay visible, got:\n%q", out)
	}
}

func TestSanitize_DisplayRootControlBytesStripped(t *testing.T) {
	// A control byte in the corpus root must not leak through the absolute path.
	results := []corpusSearchResult{{
		corpusID: corpusID("c"), kind: corpusKindFolder, displayRoot: "/tmp/ev\x1bil",
		file: zoekt.FileMatch{FileName: "f.txt", Language: "Text", Score: 1,
			LineMatches: []zoekt.LineMatch{{Line: []byte("x\n"), LineNumber: 1}}},
	}}
	out := formatCorpusResultsWithContext(results, nil, 0, 0, showCorpusContext, plainPalette)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("control byte in displayRoot leaked, got:\n%q", out)
	}
	if !strings.Contains(out, "## /tmp/evil/f.txt") {
		t.Fatalf("expected sanitized absolute path, got:\n%q", out)
	}
}

// --- Trojan Source / BiDi -------------------------------------------------

func TestSanitize_BidiOverridesStripped(t *testing.T) {
	// U+202E (RLO) + U+202C (PDF): the Trojan-Source visual-spoofing pair must
	// be removed; the surrounding text stays intact.
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("access = \u202Euser\u202C x\n", 1)}}
	out := fmtOneFile(fm, plainPalette)
	for _, r := range []rune{'\u202E', '\u202C', '\u2066', '\u2028'} {
		if strings.ContainsRune(out, r) {
			t.Fatalf("BiDi/separator U+%04X must be stripped, got:\n%q", r, out)
		}
	}
	if !strings.Contains(out, "access = user x") {
		t.Fatalf("surrounding text must survive, got:\n%q", out)
	}
}

func TestSanitize_ZeroWidthJoinerKept(t *testing.T) {
	// ZWJ (U+200D) is NOT stripped — legitimate in emoji/script sequences.
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("x = \"a\u200Db\"\n", 1)}}
	out := fmtOneFile(fm, plainPalette)
	if !strings.ContainsRune(out, '\u200D') {
		t.Fatalf("ZWJ must be preserved, got:\n%q", out)
	}
}

// --- Defensive: unsorted fragments ----------------------------------------

func TestColor_UnsortedFragmentsNoSpanLoss(t *testing.T) {
	// "aXbbbY c": X@1, Y@5. Deliver fragments OUT OF ORDER (Y then X). Without
	// the defensive sort the second span would be silently coalesced away.
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("aXbbbY c\n", 1, frag(5, 1), frag(1, 1))}}
	out := fmtOneFile(fm, ansiPalette)
	if n := strings.Count(out, ansiPalette.match); n != 2 {
		t.Fatalf("expected both out-of-order spans colored, got %d in:\n%q", n, out)
	}
	if !strings.Contains(out, ansiPalette.match+"X"+ansiPalette.reset) ||
		!strings.Contains(out, ansiPalette.match+"Y"+ansiPalette.reset) {
		t.Fatalf("expected X and Y each highlighted, got:\n%q", out)
	}
}

func TestColor_UnsortedFragmentsMultipleSwaps(t *testing.T) {
	// Three fragments delivered fully reversed (offsets 8,3,1) so the inline
	// insertion sort must back up more than one position — a single-swap-only
	// sort would leave them out of order and drop/merge a span.
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("ABCDEFGHIJ\n", 1, frag(8, 1), frag(3, 1), frag(1, 1))}}
	out := fmtOneFile(fm, ansiPalette)
	if n := strings.Count(out, ansiPalette.match); n != 3 {
		t.Fatalf("expected 3 spans after multi-swap sort, got %d in:\n%q", n, out)
	}
	for _, ch := range []string{"B", "D", "I"} { // offsets 1, 3, 8
		if !strings.Contains(out, ansiPalette.match+ch+ansiPalette.reset) {
			t.Fatalf("expected %q highlighted, got:\n%q", ch, out)
		}
	}
}

func TestColor_OverlappingFragmentsCoalesce(t *testing.T) {
	// Overlapping (not merely adjacent) spans [0,5) and [2,7) must merge into a
	// single [0,7) run — catches a merge that uses sp.start instead of max(end).
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch("0123456789\n", 1, frag(0, 5), frag(2, 5))}}
	out := fmtOneFile(fm, ansiPalette)
	if n := strings.Count(out, ansiPalette.match); n != 1 {
		t.Fatalf("expected 1 coalesced span, got %d in:\n%q", n, out)
	}
	if !strings.Contains(out, ansiPalette.match+"0123456"+ansiPalette.reset) {
		t.Fatalf("expected merged span to cover [0,7), got:\n%q", out)
	}
}

func TestColor_ManyFragmentsSpillToHeap(t *testing.T) {
	// >16 fragments on one line forces the stack-backed [16]span buffer to spill
	// to a heap-grown slice; all spans must still render. Exercises the spill
	// path that the [16] capacity bound otherwise leaves untested.
	const count = 20
	line := strings.Repeat("x ", count) + "\n" // "x" at every even offset 0,2,4,…
	frags := make([]zoekt.LineFragmentMatch, count)
	for i := range count {
		frags[i] = frag(i*2, 1)
	}
	fm := zoekt.FileMatch{FileName: "a.go", Language: "Go", Score: 1,
		LineMatches: []zoekt.LineMatch{lineMatch(line, 1, frags...)}}
	out := fmtOneFile(fm, ansiPalette)
	if n := strings.Count(out, ansiPalette.match); n != count {
		t.Fatalf("expected %d spans after heap spill, got %d in:\n%q", count, n, out)
	}
}

// --- stripRune: direct unit oracle for the security predicate -------------

func TestStripRune(t *testing.T) {
	cases := []struct {
		name string
		r    rune
		want bool
	}{
		{"NUL C0", 0x00, true},
		{"US C0 boundary", 0x1f, true},
		{"tab kept", '\t', false},
		{"space kept", 0x20, false},
		{"ascii letter", 'a', false},
		{"tilde kept", 0x7e, false},
		{"DEL", 0x7f, true},
		{"C1 start U+0080", 0x80, true},
		{"C1 ST U+009C", 0x9c, true},
		{"C1 end U+009F", 0x9f, true},
		{"U+00A0 NBSP kept", 0xa0, false},
		{"ALM U+061C", 0x061c, true},
		{"ZWJ U+200D kept", 0x200d, false},
		{"LRM U+200E", 0x200e, true},
		{"RLO U+202E", 0x202e, true},
		{"line sep U+2028", 0x2028, true},
		{"para sep U+2029", 0x2029, true},
		{"PDI U+2069", 0x2069, true},
		{"é kept", 'é', false},
		{"CJK kept", '日', false},
		{"emoji kept", '🎉', false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripRune(tc.r); got != tc.want {
				t.Errorf("stripRune(U+%04X) = %v, want %v", tc.r, got, tc.want)
			}
		})
	}
}

// --- Companion to BenchmarkCorpusFormatting_Palettes: the benchmark only
// MEASURES the realistic fixture; this VERIFIES its output is correct.

func TestCorpusFormatting_Realistic(t *testing.T) {
	results := benchmarkGitResults(benchmarkRealisticFiles(2))
	plain := formatCorpusResultsWithContext(results, nil, 0, 0, hideCorpusContext, plainPalette)
	for _, pc := range []struct {
		name string
		pal  palette
	}{
		{"plain", plainPalette},
		{"ansi", ansiPalette},
	} {
		t.Run(pc.name, func(t *testing.T) {
			out := formatCorpusResultsWithContext(results, nil, 0, 0, hideCorpusContext, pc.pal)
			if !utf8.ValidString(out) {
				t.Fatalf("output is not valid UTF-8")
			}
			if !strings.Contains(out, "needle") {
				t.Errorf("windowed match 'needle' missing")
			}
			if !strings.Contains(out, "[method]") {
				t.Errorf("symbol tag '[method]' missing")
			}
			if !strings.Contains(out, "café") {
				t.Errorf("multibyte 'café' corrupted/missing")
			}
			if !strings.Contains(out, "…+") {
				t.Errorf("expected windowing truncation marker on the long line")
			}
			if pc.pal.match != "" { // colored: stripping SGR must equal plain
				if stripped := ansiCodeRE.ReplaceAllString(out, ""); stripped != plain {
					t.Errorf("color not presentation-only:\nplain=    %q\nstripped= %q", plain, stripped)
				}
			}
		})
	}
}
