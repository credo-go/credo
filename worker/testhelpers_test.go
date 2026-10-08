package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

func newTestApp(t *testing.T, opts ...credo.Option) *credo.App {
	t.Helper()
	app, err := credo.New(opts...)
	if err != nil {
		t.Fatalf("credo.New() = %v", err)
	}
	return app
}

// newTestSupervisor returns a supervisor without an App, for driving worker
// components directly — inside synctest bubbles too.
func newTestSupervisor() *Supervisor {
	return newTestSupervisorWithLogger(slog.New(slog.DiscardHandler))
}

func newTestSupervisorWithLogger(logger *slog.Logger) *Supervisor {
	return &Supervisor{logger: logger, jitter: uniformJitter}
}

// continuousDef builds the definition Continuous builds for name and cfg.
func continuousDef(name string, cfg ...ContinuousConfig) *definition {
	call := fmt.Sprintf("Continuous(%q)", name)
	return &definition{name: name, kind: KindContinuous, continuous: resolveContinuous(call, oneConfig(call, cfg))}
}

// scheduledDef builds the definition Scheduled builds for name, expr and cfg.
func scheduledDef(name, expr string, cfg ...ScheduledConfig) *definition {
	call := fmt.Sprintf("Scheduled(%q)", name)
	return &definition{
		name:      name,
		kind:      KindScheduled,
		scheduled: resolveScheduled(call, oneConfig(call, cfg)),
		schedule:  mustSchedule(call, expr),
	}
}

// addWorker adds w's component to s without an App, as register does after
// the App accepted it, and returns it.
func addWorker(s *Supervisor, def *definition, w Worker) *component {
	c := &component{s: s, def: def, r: newRunner(def, w)}
	s.mu.Lock()
	s.components = append(s.components, c)
	s.mu.Unlock()
	return c
}

// startWorker starts c under t.Context() and stops it when the test ends.
func startWorker(t *testing.T, c *component) {
	t.Helper()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start(%s) = %v", c.def.name, err)
	}
	t.Cleanup(func() { stopWorker(t, c) })
}

// stopWorker shuts c down, failing the test when its loop has not returned
// within two seconds.
func stopWorker(t *testing.T, c *component) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Shutdown(context.WithoutCancel(t.Context()))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Shutdown(%s) did not return", c.def.name)
	}
}

// startApp runs the App's start phase — which starts its workers — and shuts
// the App down when the test ends.
func startApp(t *testing.T, app *credo.App) {
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

// mustPanic runs fn and returns the text of its panic, failing the test when
// it does not panic.
func mustPanic(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("did not panic")
		}
		msg = fmt.Sprint(r)
	}()
	fn()
	return ""
}

// capturedLog is one record seen by a logCapture, with its attributes
// (including those added through Logger.With) flattened by key.
type capturedLog struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

// logCapture is a slog.Handler that records every record at every level. It
// is safe for concurrent use and inside synctest bubbles. An observe callback,
// when set, sees each record synchronously on the logging goroutine, after
// the record is stored and outside the capture's lock.
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

// finalize closes DI registration; registrations must precede it.
func finalize(t *testing.T, app *credo.App) {
	t.Helper()
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize() = %v", err)
	}
}
