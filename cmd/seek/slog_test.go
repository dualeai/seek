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
	recorder := &testLogRecorder{}
	oldLogger := slog.Default()
	oldLogWriter := log.Writer()
	oldLogFlags := log.Flags()
	slog.SetDefault(slog.New(&testLogHandler{recorder: recorder, minLevel: minLevel}))
	t.Cleanup(func() {
		slog.SetDefault(oldLogger)
		log.SetOutput(oldLogWriter)
		log.SetFlags(oldLogFlags)
	})
	return recorder
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

type testLogHandler struct {
	recorder *testLogRecorder
	minLevel slog.Level
}

func (h *testLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel
}

func (h *testLogHandler) Handle(_ context.Context, record slog.Record) error {
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
