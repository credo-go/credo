package credo_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/credo-go/credo"
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
