package credo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/store"
)

func newTestLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	return logger, buf
}

// parseJSONLines splits the buffer by newlines and parses each non-empty
// line as a JSON object. Useful when multiple log entries are written.
func parseJSONLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Logf("skipping non-JSON line: %s", line)
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// mustFinalize finalizes the DI container so constructor-backed Resolve calls
// are admitted; registration is complete at that point.
func mustFinalize(t *testing.T, app *credo.App) {
	t.Helper()
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize() = %v", err)
	}
}

// startServing runs the App's start phase, so it can be served through
// ServeHTTP, and shuts it down when the test ends.
func startServing(t *testing.T, app *credo.App) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = app.Shutdown(ctx)
	})
	if err := app.Start(t.Context()); err != nil {
		t.Fatalf("App.Start() = %v", err)
	}
}

// readinessStore is a store whose health is fixed.
type readinessStore struct{ health store.Health }

func (*readinessStore) Ping(context.Context) error            { return nil }
func (*readinessStore) Shutdown(context.Context) error        { return nil }
func (s *readinessStore) Health(context.Context) store.Health { return s.health.Clone() }

// registerReadinessStore binds a store with the given health and registers
// it under name.
func registerReadinessStore(app *credo.App, name string, health store.Health) {
	app.ProvideValue(&readinessStore{health: health})
	store.Register[*readinessStore](app, store.WithName(name))
}
