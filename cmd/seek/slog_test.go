package main

import (
	"context"
	"log"
	"log/slog"
	"sync"
	"testing"
)

type testLogRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func captureTestLogs(t *testing.T, minLevel slog.Level) *testLogRecorder {
	t.Helper()
	return installTestLogHandler(t, &testLogHandler{recorder: &testLogRecorder{}, minLevel: minLevel})
}

// captureTestLogsWithSignal is captureTestLogs with a signal for one lock
// wait. It sends on its channel for each debug line of a lock wait (pollLock)
// whose message is reason, one of the lockWait constants, and whose "lock"
// attribute is lock. So a wait on another lock does not send. The channel
// holds one value: a record that comes before the test receives the last
// value adds none.
func captureTestLogsWithSignal(t *testing.T, minLevel slog.Level, reason, lock string) (*testLogRecorder, <-chan struct{}) {
	t.Helper()
	signaled := make(chan struct{}, 1)
	handler := &testLogHandler{recorder: &testLogRecorder{}, minLevel: minLevel, signal: reason, signalLock: lock, signaled: signaled}
	return installTestLogHandler(t, handler), signaled
}

// installTestLogHandler makes handler the default logger until the test ends.
func installTestLogHandler(t *testing.T, handler *testLogHandler) *testLogRecorder {
	t.Helper()
	oldLogger := slog.Default()
	oldLogWriter := log.Writer()
	oldLogFlags := log.Flags()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(oldLogger)
		log.SetOutput(oldLogWriter)
		log.SetFlags(oldLogFlags)
	})
	return handler.recorder
}

func (r *testLogRecorder) Records() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	records := make([]slog.Record, len(r.records))
	for i, record := range r.records {
		records[i] = record.Clone()
	}
	return records
}

// testLogHandler records each record at minLevel or above. With a signaled
// channel, it also reads the records below minLevel and sends on signaled for
// each record whose message is signal and whose "lock" attribute is
// signalLock.
type testLogHandler struct {
	recorder   *testLogRecorder
	minLevel   slog.Level
	signal     string
	signalLock string
	signaled   chan struct{}
}

func (h *testLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel || h.signaled != nil
}

func (h *testLogHandler) Handle(_ context.Context, record slog.Record) error {
	if h.signaled != nil && record.Message == h.signal && testLogAttrs(record)["lock"] == h.signalLock {
		select {
		case h.signaled <- struct{}{}:
		default:
		}
	}
	if record.Level < h.minLevel {
		return nil
	}
	record = record.Clone()
	h.recorder.mu.Lock()
	h.recorder.records = append(h.recorder.records, record)
	h.recorder.mu.Unlock()
	return nil
}

func (h *testLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	panic("testLogHandler does not support logger attributes")
}

func (h *testLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	panic("testLogHandler does not support groups")
}

func testLogAttrs(record slog.Record) map[string]any {
	attrs := make(map[string]any, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Resolve().Any()
		return true
	})
	return attrs
}
