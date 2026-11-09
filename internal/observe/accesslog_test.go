package observe

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestBelowMinLevel(t *testing.T) {
	if BelowMinLevel(200, slog.LevelInfo) {
		t.Fatal("200 at Info must not be below")
	}
	if !BelowMinLevel(200, slog.LevelWarn) {
		t.Fatal("200 at Warn must be below")
	}
	if BelowMinLevel(404, slog.LevelWarn) || BelowMinLevel(503, slog.LevelError) {
		t.Fatal("4xx/5xx at their own level must not be below")
	}
}

func TestAccessLogAttrs(t *testing.T) {
	base := AccessLogRecord{
		Method: "GET", Path: "/x", Status: 200, Bytes: 12, Duration: time.Second,
		RemoteAddr: "1.2.3.4", UserAgent: "curl",
	}

	t.Run("base attrs", func(t *testing.T) {
		if _, n := AccessLogAttrs(base, ""); n != 7 {
			t.Fatalf("n = %d, want 7", n)
		}
	})
	t.Run("path_original when different", func(t *testing.T) {
		rec := base
		rec.Path, rec.OriginalPath = "/new", "/old"
		attrs, n := AccessLogAttrs(rec, "")
		if n != 8 || attrs[7].Key != "path_original" || attrs[7].Value.String() != "/old" {
			t.Fatalf("attrs = %v (n=%d)", attrs[:n], n)
		}
	})
	t.Run("path_original omitted when equal", func(t *testing.T) {
		rec := base
		rec.OriginalPath = rec.Path
		if _, n := AccessLogAttrs(rec, ""); n != 7 {
			t.Fatalf("n = %d, want 7", n)
		}
	})
	t.Run("request_id when provided", func(t *testing.T) {
		attrs, n := AccessLogAttrs(base, "req-1")
		if n != 8 || attrs[7].Key != "request_id" || attrs[7].Value.String() != "req-1" {
			t.Fatalf("attrs = %v (n=%d)", attrs[:n], n)
		}
	})
	t.Run("both", func(t *testing.T) {
		rec := base
		rec.Path, rec.OriginalPath = "/new", "/old"
		if _, n := AccessLogAttrs(rec, "req-1"); n != 9 {
			t.Fatalf("n = %d, want 9", n)
		}
	})
}

func TestEmitAccessLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	rec := AccessLogRecord{
		Method: "GET", Path: "/x", OriginalPath: "/orig", Status: 503, Bytes: 42,
		Duration: time.Second, RemoteAddr: "1.2.3.4", UserAgent: "curl",
	}
	EmitAccessLog(t.Context(), logger, rec, "req-9")
	out := buf.String()
	for _, want := range []string{
		"level=ERROR", "request completed", "method=GET", "path=/x", "status=503",
		"bytes=42", "path_original=/orig", "request_id=req-9",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log line missing %q: %s", want, out)
		}
	}
}

func TestEmitAccessLog_SkipsDisabledLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	EmitAccessLog(t.Context(), logger, AccessLogRecord{Method: "GET", Path: "/x", Status: 200}, "")
	if buf.Len() != 0 {
		t.Fatalf("Info record was emitted through a Warn-only logger: %s", buf.String())
	}
	EmitAccessLog(t.Context(), logger, AccessLogRecord{Method: "GET", Path: "/x", Status: 404}, "")
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("Warn record missing: %s", buf.String())
	}
}
