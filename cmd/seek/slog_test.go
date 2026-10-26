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

// captureTestLogs records each log record at minLevel or above until the test
// ends. It replaces the default logger, so only sequential tests may call it.
func captureTestLogs(t *testing.T, minLevel slog.Level) *testLogRecorder {
	t.Helper()
	return installTestLogHandler(t, &testLogHandler{recorder: &testLogRecorder{}, minLevel: minLevel})
}

// captureTestLogsWithSignal is captureTestLogs with a signal for one lock
// wait. pollLock writes one debug line for each lock wait. The channel gets a
// value for each such line whose message is reason, a lockWait constant, and
// whose "lock" attribute is lock. A wait on another lock sends nothing. The
// channel holds one value: a record that comes before the test receives the
// last value adds none.
func captureTestLogsWithSignal(t *testing.T, minLevel slog.Level, reason, lock string) (*testLogRecorder, <-chan struct{}) {
	t.Helper()
	signaled := make(chan struct{}, 1)
	handler := &testLogHandler{recorder: &testLogRecorder{}, minLevel: minLevel, signal: reason, signalLock: lock, signaled: signaled}
	return installTestLogHandler(t, handler), signaled
}

// captureTestLogsWithAction is captureTestLogs that also runs act once, in the
// goroutine that writes the first record whose message is message, at any
// level. So act runs at that point of the code, before the code goes on.
func captureTestLogsWithAction(t *testing.T, minLevel slog.Level, message string, act func()) *testLogRecorder {
	t.Helper()
	handler := &testLogHandler{recorder: &testLogRecorder{}, minLevel: minLevel, action: message, act: sync.OnceFunc(act)}
	return installTestLogHandler(t, handler)
}

// installTestLogHandler makes handler the default logger until the test ends.
// At cleanup it also restores the output and flags of the log package, which
// slog.SetDefault changes.
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
// channel or an act function, it also reads the records below minLevel. It
// sends on signaled for each record whose message is signal and whose "lock"
// attribute is signalLock. It calls act for each record whose message is
// action.
type testLogHandler struct {
	recorder   *testLogRecorder
	minLevel   slog.Level
	signal     string
	signalLock string
	signaled   chan struct{}
	action     string
	act        func()
}

func (h *testLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel || h.signaled != nil || h.act != nil
}

func (h *testLogHandler) Handle(_ context.Context, record slog.Record) error {
	if h.act != nil && record.Message == h.action {
		h.act()
	}
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
