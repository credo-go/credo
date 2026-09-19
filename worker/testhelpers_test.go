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

func mustSchedule(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := ParseSchedule(expr)
	if err != nil {
		t.Fatalf("ParseSchedule(%q) = %v", expr, err)
	}
	return s
}

type fakeRawConfig struct {
	worker poolConfig
	err    error
	exists bool
}

func (c fakeRawConfig) Unmarshal(key string, dst any) error {
	if key != "worker" {
		return fmt.Errorf("unknown key %q", key)
	}
	if c.err != nil {
		return c.err
	}
	config, ok := dst.(*poolConfig)
	if !ok {
		return fmt.Errorf("unsupported destination %T", dst)
	}
	*config = c.worker
	return nil
}

func (c fakeRawConfig) Exists(key string) bool {
	return key == "worker" && c.exists
}

func newTestPool() *Pool {
	return newPool(slog.New(slog.DiscardHandler), DefaultRestartDelay)
}

func shutdownPool(t *testing.T, p *Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}

// finalize closes DI registration so Resolve becomes available. Register
// calls must precede it.
func finalize(t *testing.T, app *credo.App) {
	t.Helper()
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize() = %v", err)
	}
}

// mustDefinition builds the definition Register would build for w under
// opts, with the default restart delay.
func mustDefinition(t *testing.T, name string, w Worker, opts ...Option) *definition {
	t.Helper()
	o, schedule, err := validateOptions(opts)
	if err != nil {
		t.Fatalf("validateOptions(%s) = %v", name, err)
	}
	def := buildDefinition(name, o, schedule, DefaultRestartDelay)
	def.resolve = instance(w)
	return def
}

// startPool adds defs to p and starts it under t.Context().
func startPool(t *testing.T, p *Pool, defs ...*definition) {
	t.Helper()
	for _, def := range defs {
		if err := p.addDefinition(def); err != nil {
			t.Fatalf("addDefinition(%s) = %v", def.name, err)
		}
	}
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
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
