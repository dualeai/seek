package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCLIProcess_FirstInterruptEndsOnlyLockWaits searches a warm folder and a
// cold folder whose build lock another process holds. The search waits for
// the lock with no deadline. The first interrupt ends that wait: the search
// skips the cold node with one warning, the warm node prints, and the exit
// code is 0.
func TestCLIProcess_FirstInterruptEndsOnlyLockWaits(t *testing.T) {
	requireTools(t)
	cache := t.TempDir()
	warm := t.TempDir()
	cold := t.TempDir()
	writeFileAt(t, warm, "warm.go", "package warm\n// zqtwo warm\n")
	writeFileAt(t, cold, "cold.go", "package cold\n// zqtwo cold\n")
	if result := runCLIProcessWithCache(t, cache, t.TempDir(), []string{"--lexical-only", "zqtwo", warm}, nil); result.code != 0 {
		t.Fatalf("warm-up: exit %d, stderr %q", result.code, result.stderr)
	}
	t.Setenv("SEEK_CACHE_DIR", cache)
	info, err := os.Stat(cold)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(cold, info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(plan.cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	holder, ok, err := tryBuildLock(plan.cacheDir)
	if err != nil || !ok {
		t.Fatalf("hold the build lock: ok=%v err=%v", ok, err)
	}
	defer releaseLock(holder)

	code, stdout, stderr := interruptAtLockWait(t, cache, holder.Name(), "--lexical-only", "--verbose", "zqtwo", warm, cold)
	if code != 0 || !strings.Contains(stdout, "warm.go") || strings.Contains(stdout, "cold.go") ||
		strings.Count(stderr, "Skipped a search node") != 1 {
		t.Fatalf("exit %d, want 0 with the warm hit and one skip:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// TestWatchInterruptsEndsWaitsThenTheCommand feeds signals to the handler: a
// first SIGINT with a running lock wait ends the wait only; the next SIGINT
// ends the command. A SIGINT with no wait, and a SIGTERM, end the command.
func TestWatchInterruptsEndsWaitsThenTheCommand(t *testing.T) {
	run := func(t *testing.T, waiting bool, signals ...os.Signal) (waitEnded, cancelled bool) {
		t.Helper()
		waits := newLockWaits()
		var end <-chan struct{}
		if waiting {
			end = waits.start()
		}
		feed := make(chan os.Signal, len(signals))
		for _, sig := range signals {
			feed <- sig
		}
		close(feed)
		var cancelCalls int
		watchInterrupts(feed, func() { cancelCalls++ }, waits)
		select {
		case <-end:
			waitEnded = true
		default:
		}
		return waitEnded, cancelCalls > 0
	}
	for name, test := range map[string]struct {
		waiting             bool
		signals             []os.Signal
		waitEnded, canceled bool
	}{
		"first interrupt with a wait": {true, []os.Signal{syscall.SIGINT}, true, false},
		"second interrupt":            {true, []os.Signal{syscall.SIGINT, syscall.SIGINT}, true, true},
		"interrupt with no wait":      {false, []os.Signal{syscall.SIGINT}, false, true},
		"terminate with a wait":       {true, []os.Signal{syscall.SIGTERM}, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			if ended, canceled := run(t, test.waiting, test.signals...); ended != test.waitEnded || canceled != test.canceled {
				t.Fatalf("wait ended=%v command canceled=%v, want %v and %v", ended, canceled, test.waitEnded, test.canceled)
			}
		})
	}
}

// TestCLIProcess_InterruptWithOnlyAWaitingNodeIsACancel searches one cold
// folder whose build lock another process holds. The interrupt ends the
// wait, and no node gives an answer. So the command ends as canceled (exit
// 2), not as a search with no result (exit 1).
func TestCLIProcess_InterruptWithOnlyAWaitingNodeIsACancel(t *testing.T) {
	requireTools(t)
	cache := t.TempDir()
	cold := t.TempDir()
	writeFileAt(t, cold, "cold.go", "package cold\n// zqone cold\n")
	t.Setenv("SEEK_CACHE_DIR", cache)
	info, err := os.Stat(cold)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFolderCorpus(cold, info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(plan.cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	holder, ok, err := tryBuildLock(plan.cacheDir)
	if err != nil || !ok {
		t.Fatalf("hold the build lock: ok=%v err=%v", ok, err)
	}
	defer releaseLock(holder)
	code, stdout, stderr := interruptAtLockWait(t, cache, holder.Name(), "--lexical-only", "--verbose", "zqone", cold)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "context canceled") {
		t.Fatalf("exit %d, want 2 and a cancel:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// interruptAtLockWait runs the CLI with args and sends one SIGINT when it logs
// a wait for the build lock at lock. It returns the exit code and the output.
// A CLI that ends with no such wait fails the test.
func interruptAtLockWait(t *testing.T, cache, lock string, args ...string) (int, string, string) {
	t.Helper()
	result, waited := runCLIProcessOnLine(t, cache, t.TempDir(), args, lockWaitLine(lockWaitBuild, lock), func(process *os.Process) {
		if err := process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
	})
	if !waited {
		t.Fatalf("the search never waited for the build lock: exit %d\nstdout:\n%s\nstderr:\n%s", result.code, result.stdout, result.stderr)
	}
	return result.code, result.stdout, result.stderr
}

// TestLockWaitsEndOnlyTheRunningWaits ends the waits of a command while one
// wait runs. That wait ends. A wait that starts later, such as the publish of
// a build that was running, waits until its lock is free.
func TestLockWaitsEndOnlyTheRunningWaits(t *testing.T) {
	waits := newLockWaits()
	running := waits.start()
	if !waits.endRunning() {
		t.Fatal("endRunning found no running wait")
	}
	select {
	case <-running:
	default:
		t.Fatal("the running wait did not end")
	}
	waits.stop()
	ctx := withLockWaits(t.Context(), waits)
	tries := 0
	err := pollLock(ctx, "/cache/.lock", "", func() error {
		if tries++; tries < 3 {
			return syscall.EWOULDBLOCK
		}
		return nil
	}, time.Millisecond)
	if err != nil || tries != 3 {
		t.Fatalf("a later wait gave %v after %d tries, want the lock after 3", err, tries)
	}
}

// TestLockWaitIsCountedBeforeItsLogLine checks that a wait counts when its
// debug line is written. So an interrupt that a reader of the line sends ends
// the wait.
func TestLockWaitIsCountedBeforeItsLogLine(t *testing.T) {
	waits := newLockWaits()
	canceled, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := withLockWaits(canceled, waits)
	counted := make(chan bool, 1)
	captureTestLogsWithAction(t, slog.LevelError, "Waiting for a test lock", func() {
		ended := waits.endRunning()
		if !ended {
			// The interrupt cannot end a wait that is not counted: the
			// context ends it, so the test fails instead of hanging.
			cancel()
		}
		counted <- ended
	})
	err := pollLock(ctx, "/cache/.lock", "Waiting for a test lock", func() error { return syscall.EWOULDBLOCK },
		time.Millisecond)
	if !<-counted || !errors.Is(err, errLockWaitEnded) {
		t.Fatalf("the wait was not counted at its log line (err=%v)", err)
	}
}
