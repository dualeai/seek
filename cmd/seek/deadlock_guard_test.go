package main

import (
	"runtime"
	"testing"

	"go.uber.org/goleak"
)

// goroutineLeakGuard checks a streaming test for leaked goroutines.
//
// On entry: records the running goroutines and raises GOMAXPROCS to at least
// 2. Contention then makes it less likely that the producer, consumer and
// worker run in an order that hides the bug. A test that sets a higher value
// keeps it.
//
// On exit (via the returned cleanup func): restores GOMAXPROCS, then reports
// with goleak each goroutine that started after the entry and still runs. It
// reports with t.Error, not Fatal, so the test continues to drain. goleak
// tries again for a short time before it reports: a goroutine that the code
// does not join can still be on its way out, and no signal tells when it
// ends.
//
// The guard has no time limit: go test -timeout stops a test that hangs and
// prints the stack of every goroutine.
func goroutineLeakGuard(t *testing.T) func() {
	t.Helper()
	prevProcs := runtime.GOMAXPROCS(max(2, runtime.GOMAXPROCS(0)))
	running := goleak.IgnoreCurrent()
	return func() {
		t.Helper()
		runtime.GOMAXPROCS(prevProcs)
		goleak.VerifyNone(t, running)
	}
}
