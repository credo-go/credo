package credo_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// countingReader counts Read calls so a test can prove a reader was never
// consumed.
type countingReader struct {
	reads int
}

func (r *countingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

// TestResponse_BodilessStatusesSkipBodyAndContentType locks the RFC 9110
// contract for every body-writing helper: status codes that forbid a body
// (1xx, 204, 304) must produce a status-only response — no body bytes, no
// Content-Type header — and return nil instead of surfacing net/http's
// "response status code does not allow body" write error.
func TestResponse_BodilessStatusesSkipBodyAndContentType(t *testing.T) {
	type xmlPayload struct {
		Value string `xml:"value"`
	}
	helpers := []struct {
		name string
		call func(r *credo.Response, code int) error
	}{
		{"JSON", func(r *credo.Response, code int) error {
			return r.JSON(code, map[string]string{"key": "value"})
		}},
		{"Text", func(r *credo.Response, code int) error {
			return r.Text(code, "body")
		}},
		{"HTML", func(r *credo.Response, code int) error {
			return r.HTML(code, "<p>body</p>")
		}},
		{"XML", func(r *credo.Response, code int) error {
			return r.XML(code, xmlPayload{Value: "body"})
		}},
		{"Blob", func(r *credo.Response, code int) error {
			return r.Blob(code, "application/octet-stream", []byte("body"))
		}},
		{"Stream", func(r *credo.Response, code int) error {
			return r.Stream(code, "application/octet-stream", strings.NewReader("body"))
		}},
	}
	codes := []int{http.StatusContinue, http.StatusNoContent, http.StatusNotModified}

	for _, h := range helpers {
		for _, code := range codes {
			t.Run(h.name+"/"+http.StatusText(code), func(t *testing.T) {
				rec := httptest.NewRecorder()
				resp := credo.NewResponse(rec)

				if err := h.call(resp, code); err != nil {
					t.Fatalf("%s(%d) error = %v, want nil", h.name, code, err)
				}
				if rec.Code != code {
					t.Errorf("status = %d, want %d", rec.Code, code)
				}
				if got := rec.Body.Len(); got != 0 {
					t.Errorf("body length = %d, want 0 (body: %q)", got, rec.Body.String())
				}
				if ct := rec.Header().Get("Content-Type"); ct != "" {
					t.Errorf("Content-Type = %q, want unset", ct)
				}
			})
		}
	}
}

// TestResponse_StreamBodilessNeverReadsReader proves Stream skips the reader
// entirely on a bodiless status, so callers may pass a reader whose
// consumption has side effects without it being drained.
func TestResponse_StreamBodilessNeverReadsReader(t *testing.T) {
	rd := &countingReader{}
	resp := credo.NewResponse(httptest.NewRecorder())

	if err := resp.Stream(http.StatusNoContent, "application/octet-stream", rd); err != nil {
		t.Fatalf("Stream error = %v", err)
	}
	if rd.reads != 0 {
		t.Errorf("reader Read calls = %d, want 0", rd.reads)
	}
}

// TestResponse_BodilessPreservesConditionalHeaders locks that the bodiless
// short-circuit only skips body and Content-Type: headers already set by the
// handler (ETag, Cache-Control on a 304) still reach the wire.
func TestResponse_BodilessPreservesConditionalHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := credo.NewResponse(rec)
	resp.Header().Set("ETag", `"v1"`)
	resp.Header().Set("Cache-Control", "max-age=60")

	if err := resp.JSON(http.StatusNotModified, map[string]string{"skip": "me"}); err != nil {
		t.Fatalf("JSON(304) error = %v", err)
	}
	if got := rec.Header().Get("ETag"); got != `"v1"` {
		t.Errorf("ETag = %q, want %q", got, `"v1"`)
	}
	if got := rec.Header().Get("Cache-Control"); got != "max-age=60" {
		t.Errorf("Cache-Control = %q, want %q", got, "max-age=60")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "" {
		t.Errorf("Content-Type = %q, want unset", ct)
	}
}

// TestResponse_NoContentOverRealServerProducesNoSpuriousWarn reproduces the
// original bug end to end on a real net/http server: JSON(204, body) used to
// fail inside net/http after the header was committed, and the returned error
// then hit the pipeline's committed guard as a spurious
// "error after response committed" WARN. With the bodiless short-circuit the
// request must complete silently: 204, empty body, no Content-Type, no WARN.
func TestResponse_NoContentOverRealServerProducesNoSpuriousWarn(t *testing.T) {
	logs := &syncBuffer{}
	app := mustNew(t,
		credo.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		credo.WithoutAccessLog(),
	)
	app.DELETE("/things/{id}", func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusNoContent, map[string]string{"deleted": "true"})
	})

	srv := httptest.NewServer(app)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/things/42", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusNoContent)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "" {
		t.Errorf("Content-Type = %q, want unset", ct)
	}
	if out := logs.String(); strings.Contains(out, "error after response committed") {
		t.Errorf("spurious committed-response WARN still emitted:\n%s", out)
	}
}
