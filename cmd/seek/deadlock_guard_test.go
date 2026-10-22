package main

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

// defaultLeakTimeout is the watchdog budget used by light-weight node
// tests (no real indexing, no real corpus pressure). Tests that
// exercise indexing, semaphore pressure, or large fixtures pass an
// explicit longer duration directly.
const defaultLeakTimeout = 10 * time.Second

// goroutineLeakGuard arms a deadlock watchdog and a goroutine-leak
// checker around a streaming test.
//
// On entry: snapshots runtime.NumGoroutine() and bumps GOMAXPROCS to 2
// (amplifies contention so the producer / consumer / worker triple is
// less likely to serialise in a way that masks the bug).
//
// On exit (via the returned cleanup func): stops the timer, restores
// GOMAXPROCS, gives goroutines up to 2s to exit naturally, then reports
// any leak via t.Errorf (not Fatal — let the test continue to drain).
//
// If the deadline fires before cleanup, the watchdog dumps every
// goroutine stack to os.Stderr and panics. It panics, not t.Fatal,
// because `time.AfterFunc` runs in a separate goroutine, and t.Fatal
// from there would not stop the blocked goroutines.
func goroutineLeakGuard(t *testing.T, d time.Duration) func() {
	t.Helper()
	prevProcs := runtime.GOMAXPROCS(2)
	before := runtime.NumGoroutine()
	name := t.Name()
	timer := time.AfterFunc(d, func() {
		buf := make([]byte, 4*1024*1024)
		n := runtime.Stack(buf, true)
		fmt.Fprintf(os.Stderr, "\n=== DEADLOCK (%s) — full goroutine dump ===\n%s\n", name, buf[:n])
		panic("goroutineLeakGuard: deadline " + d.String() + " exceeded for " + name)
	})
	return func() {
		t.Helper()
		timer.Stop()
		runtime.GOMAXPROCS(prevProcs)
		// Give goroutines a brief grace period to exit naturally.
		// 100 ms steps; cap at 2 s.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if runtime.NumGoroutine() <= before {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		if got := runtime.NumGoroutine(); got > before {
			t.Errorf("goroutine leak: before=%d after=%d", before, got)
			dumpGoroutines(t)
		}
	}
}

// No test covers the goroutineLeakGuard watchdog itself: the watchdog
// panics in a time.AfterFunc goroutine, and an unrecovered panic there
// stops the test binary; recover() in a test goroutine cannot catch it.
// Such a test would need the panic body in a function of its own.

// dumpGoroutines writes every goroutine's stack to the test log (t.Logf),
// so the dump stays with the output of the failing test. Tests call it
// when an assertion other than the watchdog fails.
func dumpGoroutines(t *testing.T) {
	t.Helper()
	buf := make([]byte, 4*1024*1024)
	n := runtime.Stack(buf, true)
	t.Logf("=== goroutine dump (%s) ===\n%s", t.Name(), buf[:n])
}
