package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

// workerFunc adapts a function to Worker.
type workerFunc func(context.Context) error

func (f workerFunc) Run(ctx context.Context) error { return f(ctx) }

// testEngine is an engine with the registration list the root registry
// keeps, for driving worker components directly without an App — inside
// synctest bubbles too.
type testEngine struct {
	*Engine

	mu         sync.Mutex
	components []*Component // registration order
}

// newTestEngine returns an engine that logs nothing.
func newTestEngine() *testEngine {
	return newTestEngineWithLogger(slog.New(slog.DiscardHandler))
}

func newTestEngineWithLogger(logger *slog.Logger) *testEngine {
	return &testEngine{Engine: NewEngine(logger)}
}

// Snapshot returns one Info per added worker, in registration order.
func (s *testEngine) Snapshot() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	infos := make([]Info, 0, len(s.components))
	for _, c := range s.components {
		infos = append(infos, c.Snapshot())
	}
	return infos
}

// Lookup returns the added worker named exactly name, or false.
func (s *testEngine) Lookup(name string) (Info, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.components {
		if c.Definition().Name() == name {
			return c.Snapshot(), true
		}
	}
	return Info{}, false
}

// continuousDef builds the definition Workers.Continuous builds for name and
// cfg.
func continuousDef(name string, cfg ...ContinuousConfig) *Definition {
	return NewContinuous(fmt.Sprintf("Workers.Continuous(%q)", name), name, oneTestConfig(cfg))
}

// scheduledDef builds the definition Workers.Scheduled builds for name, expr
// and cfg.
func scheduledDef(name, expr string, cfg ...ScheduledConfig) *Definition {
	return NewScheduled(fmt.Sprintf("Workers.Scheduled(%q)", name), name, expr, oneTestConfig(cfg))
}

// oneTestConfig returns the single configuration of cfgs, or the zero value.
func oneTestConfig[C any](cfgs []C) C {
	var zero C
	switch len(cfgs) {
	case 0:
		return zero
	case 1:
		return cfgs[0]
	}
	panic(fmt.Sprintf("%d configurations given; pass at most one", len(cfgs)))
}

// addWorker adds w's component to s without an App, as the root registry
// does after the App accepted it, and returns it.
func addWorker(s *testEngine, def *Definition, w Worker) *Component {
	c := NewComponent(s.Engine, def, w)
	s.mu.Lock()
	s.components = append(s.components, c)
	s.mu.Unlock()
	return c
}

// startWorker starts c under t.Context() and stops it when the test ends.
func startWorker(t *testing.T, c *Component) {
	t.Helper()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start(%s) = %v", c.def.name, err)
	}
	t.Cleanup(func() { stopWorker(t, c) })
}

// stopWorker shuts c down, failing the test when its loop has not returned
// within two seconds.
func stopWorker(t *testing.T, c *Component) {
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

// capturedLog is one record seen by a logCapture, with its attributes
// (including those added through Logger.With) flattened by key.
type capturedLog struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

// logCapture is a slog.Handler that records every record at every level. It
// is safe for concurrent use and inside synctest bubbles.
type logCapture struct {
	state *logCaptureState
	attrs []slog.Attr
}

type logCaptureState struct {
	mu      sync.Mutex
	records []capturedLog
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
	h.state.mu.Unlock()
	return nil
}

func (h *logCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logCapture{state: h.state, attrs: append(slices.Clone(h.attrs), attrs...)}
}

func (h *logCapture) WithGroup(string) slog.Handler { return h }

func (h *logCapture) logger() *slog.Logger { return slog.New(h) }

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
