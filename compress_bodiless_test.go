package credo_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
)

// TestCompress_BodilessStatusNeverStartsCompressor: a 204/304 with a
// compressible Content-Type must not create a compressor. An empty gzip
// stream still carries a header and trailer, and net/http rejects that body
// on a bodiless status, which used to abort the connection (the client saw
// EOF). Verified against a real server, where the recorder would not have
// noticed.
func TestCompress_BodilessStatusNeverStartsCompressor(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			app := mustNew(t)
			app.UseCompress()
			app.GET("/", func(ctx *credo.Context) error {
				ctx.Response().Header().Set("Content-Type", "application/json")
				return ctx.Response().NoContent(status)
			})

			srv := httptest.NewServer(app)
			defer srv.Close()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept-Encoding", "gzip")
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer res.Body.Close()
			if res.StatusCode != status {
				t.Fatalf("status = %d, want %d", res.StatusCode, status)
			}
			if got := res.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want empty", got)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if len(body) != 0 {
				t.Fatalf("body = %q, want empty", body)
			}
		})
	}
}
