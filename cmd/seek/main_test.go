package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrNoMatch_IsDistinct(t *testing.T) {
	// errNoMatch must not be confused with generic errors.
	generic := errors.New("something went wrong")
	if errors.Is(generic, errNoMatch) {
		t.Error("generic error should not match errNoMatch")
	}
}

func TestErrNoMatch_WrappedIsDetectable(t *testing.T) {
	// Even when wrapped, errors.Is must still detect errNoMatch so that
	// callers (main) can reliably map it to exit code 1.
	wrapped := fmt.Errorf("search: %w", errNoMatch)
	if !errors.Is(wrapped, errNoMatch) {
		t.Error("wrapped errNoMatch should be detectable via errors.Is")
	}
}
