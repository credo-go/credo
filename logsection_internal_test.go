package credo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// lockedBuffer is a bytes.Buffer safe for the concurrent writes of two
// handler generations, each of which locks only its own writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSectionHandler_DerivedHandlersFollowASwap: a handler derived with
// WithAttrs and WithGroup replays its steps on the handler a format change
// installs, in order, and a derivation made after the swap starts from it.
func TestSectionHandler_DerivedHandlersFollowASwap(t *testing.T) {
	var buf lockedBuffer
	out := newLogOutput(&buf, logConfig{})
	root := slog.New(&sectionHandler{out: out})
	before := root.With("a", 1).WithGroup("g").With("b", 2)

	before.Info("text record", "c", 3)
	if got := buf.String(); !strings.Contains(got, "a=1") || !strings.Contains(got, "g.b=2") || !strings.Contains(got, "g.c=3") {
		t.Fatalf("text record = %q, want a=1 g.b=2 g.c=3", got)
	}

	out.apply(logConfig{Format: "json"})
	mark := len(buf.String())
	before.Info("json record", "c", 3)
	after := before.With("d", 4)
	after.Info("derived after the swap")

	var first, second map[string]any
	lines := strings.Split(strings.TrimSpace(buf.String()[mark:]), "\n")
	if len(lines) != 2 || json.Unmarshal([]byte(lines[0]), &first) != nil || json.Unmarshal([]byte(lines[1]), &second) != nil {
		t.Fatalf("records after the swap are not two JSON lines:\n%s", buf.String()[mark:])
	}
	g, _ := first["g"].(map[string]any)
	if first["a"] != float64(1) || g["b"] != float64(2) || g["c"] != float64(3) {
		t.Errorf("replayed record = %v, want a=1 and g={b:2,c:3}", first)
	}
	g, _ = second["g"].(map[string]any)
	if second["a"] != float64(1) || g["b"] != float64(2) || g["d"] != float64(4) {
		t.Errorf("record derived after the swap = %v, want a=1 and g={b:2,d:4}", second)
	}

	// Empty derivations return the handler itself, as slog's handlers do.
	h := root.Handler()
	if h.WithAttrs(nil) != h || h.WithGroup("") != h {
		t.Error("WithAttrs(nil) or WithGroup(\"\") returned a new handler")
	}
}

// TestSectionHandler_ConcurrentSwap: records written while the level and the
// format change are each written whole, by one generation's handler. Run
// under the race detector.
func TestSectionHandler_ConcurrentSwap(t *testing.T) {
	out := newLogOutput(io.Discard, logConfig{})
	root := slog.New(&sectionHandler{out: out})
	component := root.With("component", "orders")

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for i := range 500 {
				root.Info("root", "i", i)
				component.Debug("component", "i", i)
				component.With("request_id", i).Warn("request")
			}
		})
	}
	wg.Go(func() {
		for i := range 500 {
			cfg := logConfig{Level: "debug", Format: "json"}
			if i%2 == 0 {
				cfg = logConfig{Level: "warn", Format: "text"}
			}
			out.apply(cfg)
		}
	})
	wg.Wait()

	out.apply(logConfig{Level: "error"})
	if component.Enabled(context.Background(), slog.LevelWarn) || !component.Enabled(context.Background(), slog.LevelError) {
		t.Error("the level after the last apply is not error")
	}
}

// TestLogConfig_Validate: the section's two keys, each optional and matched
// without regard to case; an error names the key, the value and the values
// the key takes.
func TestLogConfig_Validate(t *testing.T) {
	tests := []struct {
		cfg  logConfig
		want string
	}{
		{logConfig{}, ""},
		{logConfig{Level: "debug", Format: "json"}, ""},
		{logConfig{Level: "Info", Format: "TEXT"}, ""},
		{logConfig{Level: "warn"}, ""},
		{logConfig{Level: "error"}, ""},
		{logConfig{Level: "warning"}, `log.level "warning" is not a log level; use debug, info, warn or error`},
		{logConfig{Level: "4"}, `log.level "4" is not a log level; use debug, info, warn or error`},
		{logConfig{Format: "logfmt"}, `log.format "logfmt" is not a log format; use text or json`},
	}
	for _, tt := range tests {
		err := tt.cfg.Validate()
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%+v: Validate() = %v, want nil", tt.cfg, err)
		case tt.want != "" && (err == nil || err.Error() != tt.want):
			t.Errorf("%+v: Validate() = %v, want %q", tt.cfg, err, tt.want)
		}
	}
}
