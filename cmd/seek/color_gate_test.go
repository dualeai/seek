package main

import (
	"os"
	"testing"
)

// withCleanColorEnv unsets NO_COLOR, CLICOLOR_FORCE and TERM, and puts their
// values back at cleanup, so each case starts from a known state. t.Setenv
// cannot unset a variable, and the unset branches need that.
func withCleanColorEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"NO_COLOR", "CLICOLOR_FORCE", "TERM"} {
		if orig, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, orig) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Unsetenv(k)
	}
}

// TestUseColor_GatePrecedence checks the order of the color rules of useColor
// on a pipe: NO_COLOR, then CLICOLOR_FORCE, then TERM.
func TestUseColor_GatePrecedence(t *testing.T) {
	// A pipe is never a terminal, so term.IsTerminal is false, and each
	// environment branch gives the same result on every host. The terminal
	// branch needs a real PTY, so this unit test does not check it.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })

	str := func(s string) *string { return &s }
	cases := []struct {
		name        string
		noColor     *string // nil = unset
		clicolorFrc *string
		term        *string
		want        bool
	}{
		{name: "piped, TERM=xterm, no overrides → off", term: str("xterm"), want: false},
		{name: "NO_COLOR=1 → off", noColor: str("1"), term: str("xterm"), want: false},
		{name: "NO_COLOR=0 (non-empty) → off", noColor: str("0"), term: str("xterm"), want: false},
		{name: `NO_COLOR="" (empty) → not disabling, but piped → off`, noColor: str(""), term: str("xterm"), want: false},
		{name: "CLICOLOR_FORCE set → on even when piped", clicolorFrc: str("1"), term: str("xterm"), want: true},
		{name: `CLICOLOR_FORCE="" (present) → on`, clicolorFrc: str(""), term: str("xterm"), want: true},
		{name: "NO_COLOR beats CLICOLOR_FORCE", noColor: str("1"), clicolorFrc: str("1"), term: str("xterm"), want: false},
		{name: "TERM=dumb → off", term: str("dumb"), want: false},
		{name: "TERM unset → off", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withCleanColorEnv(t)
			if tc.noColor != nil {
				_ = os.Setenv("NO_COLOR", *tc.noColor)
			}
			if tc.clicolorFrc != nil {
				_ = os.Setenv("CLICOLOR_FORCE", *tc.clicolorFrc)
			}
			if tc.term != nil {
				_ = os.Setenv("TERM", *tc.term)
			}
			if got := useColor(pw); got != tc.want {
				t.Fatalf("useColor()=%v, want %v", got, tc.want)
			}
		})
	}
}
