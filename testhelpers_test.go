package credo_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
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
	for _, line := range bytes.Split(data, []byte("\n")) {
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
