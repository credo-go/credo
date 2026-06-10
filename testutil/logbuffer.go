package testutil

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// LogBuffer captures structured log output for assertions in tests. It
// implements io.Writer and feeds a slog JSON handler, so every attribute,
// group, and With-derived field (such as the request_id added by the built-in
// request middleware) is recorded exactly as slog renders it.
//
// Wire a LogBuffer into a test App with [WithLogBuffer]:
//
//	buf := testutil.NewLogBuffer()
//	app := testutil.NewApp(t, testutil.WithLogBuffer(buf))
//	// ... exercise the app ...
//	buf.AssertHas(t, testutil.LogEntry{Level: "INFO", Message: "request completed"})
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// NewLogBuffer returns an empty LogBuffer ready to be wired with [WithLogBuffer].
func NewLogBuffer() *LogBuffer {
	return &LogBuffer{}
}

// Write implements io.Writer. It is safe for concurrent use: slog handlers may
// be invoked from multiple goroutines serving requests.
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Handler returns a slog.Handler that writes JSON log records to the buffer at
// debug level, so all records are captured. It delegates to
// [slog.NewJSONHandler], which keeps attribute, group, and WithAttrs semantics
// correct without a hand-written handler.
func (b *LogBuffer) Handler() slog.Handler {
	return slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})
}

// Reset discards all captured log records.
func (b *LogBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// Entries parses and returns every captured log record as a decoded JSON
// object, in the order written. Lines that fail to parse as JSON are skipped.
func (b *LogBuffer) Entries() []map[string]any {
	b.mu.Lock()
	data := bytes.Clone(b.buf.Bytes())
	b.mu.Unlock()

	var entries []map[string]any
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// LogEntry is a partial matcher for [LogBuffer.AssertHas]. Only the non-zero
// fields participate: an empty Level or Message is ignored, and Attrs matches
// as a subset (extra attributes on the actual record are allowed). Attribute
// values are compared after JSON normalization, so LogEntry{Attrs: {"status":
// 200}} matches a record whose status decoded as float64(200).
type LogEntry struct {
	// Level matches the slog "level" field case-insensitively (e.g. "INFO").
	Level string
	// Message matches the slog "msg" field exactly.
	Message string
	// Attrs matches as a subset; each key must be present with an equal value.
	Attrs map[string]any
}

// AssertHas fails the test (via tb.Errorf) unless at least one captured log
// record matches want. The matching rules are described on [LogEntry]. On
// failure it reports the wanted matcher and every captured record.
func (b *LogBuffer) AssertHas(tb testing.TB, want LogEntry) {
	tb.Helper()
	entries := b.Entries()
	for _, e := range entries {
		if matchesEntry(e, want) {
			return
		}
	}
	tb.Errorf("log assertion failed: no record matched %s\ncaptured %d record(s):\n%s",
		describeWant(want), len(entries), describeEntries(entries))
}

// AssertNotHas fails the test (via tb.Errorf) when at least one captured log
// record matches want — the negative counterpart of [AssertHas], for
// asserting that something was NOT logged (a skipped access log, a masked
// error detail). The matching rules are described on [LogEntry].
func (b *LogBuffer) AssertNotHas(tb testing.TB, want LogEntry) {
	tb.Helper()
	for _, e := range b.Entries() {
		if matchesEntry(e, want) {
			data, _ := json.Marshal(e)
			tb.Errorf("log assertion failed: expected no record matching %s, found:\n  %s",
				describeWant(want), data)
			return
		}
	}
}

// AssertEmpty fails the test (via tb.Errorf) when any log records were
// captured at all. Useful after [LogBuffer.Reset] or for code paths that
// must stay silent.
func (b *LogBuffer) AssertEmpty(tb testing.TB) {
	tb.Helper()
	entries := b.Entries()
	if len(entries) > 0 {
		tb.Errorf("log assertion failed: expected no records, captured %d:\n%s",
			len(entries), describeEntries(entries))
	}
}

func matchesEntry(got map[string]any, want LogEntry) bool {
	if want.Level != "" {
		lvl, _ := got["level"].(string)
		if !strings.EqualFold(lvl, want.Level) {
			return false
		}
	}
	if want.Message != "" {
		msg, _ := got["msg"].(string)
		if msg != want.Message {
			return false
		}
	}
	for k, wantVal := range want.Attrs {
		gotVal, ok := got[k]
		if !ok {
			return false
		}
		if !reflect.DeepEqual(jsonNormalize(wantVal), gotVal) {
			return false
		}
	}
	return true
}

// jsonNormalize round-trips v through JSON so that want values use the same
// decoded representation as captured records (for example int becomes float64).
func jsonNormalize(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return v
	}
	return out
}

func describeWant(want LogEntry) string {
	var sb strings.Builder
	sb.WriteByte('{')
	first := true
	add := func(k, v string) {
		if !first {
			sb.WriteString(", ")
		}
		first = false
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(v)
	}
	if want.Level != "" {
		add("level", want.Level)
	}
	if want.Message != "" {
		add("msg", strconv.Quote(want.Message))
	}
	if len(want.Attrs) > 0 {
		data, _ := json.Marshal(want.Attrs)
		add("attrs", string(data))
	}
	sb.WriteByte('}')
	return sb.String()
}

func describeEntries(entries []map[string]any) string {
	if len(entries) == 0 {
		return "  (none)"
	}
	var sb strings.Builder
	for i, e := range entries {
		if i > 0 {
			sb.WriteByte('\n')
		}
		data, _ := json.Marshal(e)
		sb.WriteString("  ")
		sb.Write(data)
	}
	return sb.String()
}
