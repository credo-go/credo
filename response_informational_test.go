package credo_test

import (
	"bytes"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"testing"

	"github.com/credo-go/credo"
)

// liveResult is what the client of a live exchange saw: the informational
// statuses, then the final status, headers and body.
type liveResult struct {
	hints  []int
	status int
	header http.Header
	body   []byte
}

// liveExchange sends one GET with the given headers to srv. A request that
// sets Accept-Encoding itself gets the body as sent, without the client's
// transparent decompression.
func liveExchange(t *testing.T, srv *httptest.Server, header http.Header) liveResult {
	t.Helper()
	var got liveResult
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			got.hints = append(got.hints, code)
			return nil
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace),
		http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	got.status, got.header = res.StatusCode, res.Header
	got.body, err = io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return got
}

// decodedBody returns the body as the client would read it: gunzipped when
// the response declares gzip, as is otherwise. A gzip declaration over a body
// that is not gzip fails the test.
func (r liveResult) decodedBody(t *testing.T) string {
	t.Helper()
	if r.header.Get("Content-Encoding") == "gzip" {
		return gunzipBody(t, bytes.NewReader(r.body))
	}
	return string(r.body)
}

// TestResponse_InformationalStatusIsNotFinal: a 1xx other than 101 is an
// interim response. net/http sends it at once and leaves the header unwritten
// for the final status. Response, and the compression writer below it, used
// to record the 1xx as the final status, so the handler's real status was
// dropped and the client saw 103 followed by an implicit 200.
func TestResponse_InformationalStatusIsNotFinal(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "identity"
		if compressed {
			name = "compressed"
		}
		t.Run(name, func(t *testing.T) {
			app := mustNew(t)
			if compressed {
				app.UseCompress()
			}
			var statusAfterHint int
			var committedAfterHint bool
			app.GET("/", func(ctx *credo.Context) error {
				res := ctx.Response()
				res.Header().Set("Link", "</app.css>; rel=preload; as=style")
				res.WriteHeader(http.StatusEarlyHints)
				statusAfterHint, committedAfterHint = res.Status(), res.Committed()
				return res.JSON(http.StatusCreated, map[string]string{"state": "created"})
			})
			srv := httptest.NewServer(app)
			defer srv.Close()

			got := liveExchange(t, srv, http.Header{"Accept-Encoding": {"gzip"}})

			if !slices.Equal(got.hints, []int{http.StatusEarlyHints}) {
				t.Errorf("informational responses = %v, want [103]", got.hints)
			}
			if got.status != http.StatusCreated {
				t.Fatalf("status = %d, want 201", got.status)
			}
			wantEncoding := ""
			if compressed {
				wantEncoding = "gzip"
			}
			if encoding := got.header.Get("Content-Encoding"); encoding != wantEncoding {
				t.Errorf("Content-Encoding = %q, want %q", encoding, wantEncoding)
			}
			if body := got.decodedBody(t); body != `{"state":"created"}` {
				t.Errorf("body = %q, want the JSON document", body)
			}
			if statusAfterHint != 0 || committedAfterHint {
				t.Errorf("after 103: Status() = %d, Committed() = %t, want 0, false",
					statusAfterHint, committedAfterHint)
			}
		})
	}
}

// TestResponse_SwitchingProtocolsIsFinal: 101 ends the HTTP exchange, so it is
// recorded as the response status like any final one.
func TestResponse_SwitchingProtocolsIsFinal(t *testing.T) {
	rec := httptest.NewRecorder()
	res := credo.NewResponse(rec)
	res.WriteHeader(http.StatusSwitchingProtocols)
	if res.Status() != http.StatusSwitchingProtocols || !res.Committed() {
		t.Fatalf("Status() = %d, Committed() = %t, want 101, true", res.Status(), res.Committed())
	}
}

// TestResponse_InvalidStatusLeavesResponseUncommitted: net/http panics on a
// status outside 100–999 before it writes anything. The panic now comes
// before Response or the compression writer records or changes anything, so
// recovery still renders its 500. Response used to record the code first:
// recovery found the response committed, the access record carried the
// invalid code, and net/http finished the exchange with an empty 200.
func TestResponse_InvalidStatusLeavesResponseUncommitted(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "identity"
		if compressed {
			name = "compressed"
		}
		t.Run(name, func(t *testing.T) {
			app := mustNew(t, credo.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
			if compressed {
				app.UseCompress()
			}
			app.GET("/", func(ctx *credo.Context) error {
				ctx.Response().Header().Set("Content-Type", "text/plain; charset=utf-8")
				ctx.Response().WriteHeader(42)
				return nil
			})
			srv := httptest.NewServer(app)
			defer srv.Close()

			got := liveExchange(t, srv, http.Header{"Accept-Encoding": {"gzip"}})

			if got.status != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", got.status)
			}
			const want = `{"success":false,"error":{"code":"internal_server_error","message":"Internal Server Error"}}`
			if body := got.decodedBody(t); body != want {
				t.Errorf("body = %q, want %q", body, want)
			}
		})
	}
}
