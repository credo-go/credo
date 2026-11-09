package credo_test

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// workerFunc adapts a function to credo.Worker.
type workerFunc func(context.Context) error

func (f workerFunc) Run(ctx context.Context) error { return f(ctx) }

// startWorkersApp runs the App's start phase — which starts its workers —
// and shuts the App down when the test ends, failing the test when Shutdown
// reports an error.
func startWorkersApp(t *testing.T, app *credo.App) {
	t.Helper()
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("App.Shutdown() = %v", err)
		}
	})
}

// workersAwait waits for ch to close, failing the test after five seconds.
// It is a safety net against hangs, not a timing assertion.
func workersAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// capturedLog is one record seen by a logCapture, with its attributes
// (including those added through Logger.With) flattened by key.
type capturedLog struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

// logCapture is a slog.Handler that records every record at every level. It
// is safe for concurrent use. An observe callback, when set, sees each record
// synchronously on the logging goroutine, after the record is stored and
// outside the capture's lock.
type logCapture struct {
	state *logCaptureState
	attrs []slog.Attr
}

type logCaptureState struct {
	mu      sync.Mutex
	records []capturedLog
	observe func(capturedLog)
}

func newLogCapture() *logCapture {
	return &logCapture{state: &logCaptureState{}}
}

func (h *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *logCapture) Handle(_ context.Context, record slog.Record) error {
	entry := capturedLog{Level: record.Level, Message: record.Message, Attrs: map[string]any{}}
	for _, attr := range h.attrs {
		entry.Attrs[attr.Key] = attr.Value.Resolve().Any()
	}
	record.Attrs(func(attr slog.Attr) bool {
		entry.Attrs[attr.Key] = attr.Value.Resolve().Any()
		return true
	})

	h.state.mu.Lock()
	h.state.records = append(h.state.records, entry)
	observe := h.state.observe
	h.state.mu.Unlock()

	if observe != nil {
		observe(entry)
	}
	return nil
}

func (h *logCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logCapture{state: h.state, attrs: append(slices.Clone(h.attrs), attrs...)}
}

func (h *logCapture) WithGroup(string) slog.Handler { return h }

func (h *logCapture) logger() *slog.Logger { return slog.New(h) }

// setObserve installs fn as the observe callback; call it before logging starts.
func (h *logCapture) setObserve(fn func(capturedLog)) {
	h.state.mu.Lock()
	h.state.observe = fn
	h.state.mu.Unlock()
}

// all returns a copy of every record captured so far.
func (h *logCapture) all() []capturedLog {
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	return slices.Clone(h.state.records)
}

// withMessage returns the captured records whose message is msg.
func (h *logCapture) withMessage(msg string) []capturedLog {
	var out []capturedLog
	for _, entry := range h.all() {
		if entry.Message == msg {
			out = append(out, entry)
		}
	}
	return out
}

// awaitWorkerLog returns a channel closed when capture sees msg from the
// component of the worker named name. It replaces the capture's observe
// callback, so call it before the worker starts.
func awaitWorkerLog(capture *logCapture, msg, name string) <-chan struct{} {
	seen := make(chan struct{})
	var once sync.Once
	capture.setObserve(func(entry capturedLog) {
		if entry.Message == msg && entry.Attrs["component"] == "worker:"+name {
			once.Do(func() { close(seen) })
		}
	})
	return seen
}
