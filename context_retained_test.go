package credo_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"

	"github.com/credo-go/credo"
)

// TestContext_KeptPastItsRequestReadsThatRequest: a *Context kept after its
// request returns still reads that request — its values, its user, its path
// and its parameters — after the next request on the same connection has run.
func TestContext_KeptPastItsRequestReadsThatRequest(t *testing.T) {
	app := mustNew(t)
	var kept []*credo.Context
	app.GET("/items/{id}", func(c *credo.Context) error {
		id := c.Request().RouteParam("id")
		c.Set(keyTenant, "tenant-"+id)
		c.SetUser(&headUser{name: "user-" + id})
		kept = append(kept, c)
		return c.Response().NoContent(http.StatusNoContent)
	})
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	var reused []bool
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = append(reused, info.Reused) }}
	for _, id := range []string{"1", "2"} {
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, srv.URL+"/items/"+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if len(kept) != 2 || len(reused) != 2 || !reused[1] {
		t.Fatalf("contexts = %d, connection reuse = %v; want two requests on one connection", len(kept), reused)
	}

	first := kept[0]
	if first == kept[1] {
		t.Fatal("the second request was given the first request's Context")
	}
	if v, _ := first.Get(keyTenant); v != "tenant-1" {
		t.Errorf("kept Get = %q, want tenant-1", v)
	}
	if u, ok := first.GetUser[*headUser](); !ok || u.name != "user-1" {
		t.Errorf("kept GetUser = %v, %v; want user-1", u, ok)
	}
	if p, id := first.Request().URL.Path, first.Request().RouteParam("id"); p != "/items/1" || id != "1" {
		t.Errorf("kept path, id = %q, %q; want /items/1 and 1", p, id)
	}
	if v, _ := first.Context().Value(keyTenant).(string); v != "tenant-1" {
		t.Errorf("kept Context().Value = %q, want tenant-1", v)
	}
	if s := first.Response().Status(); s != http.StatusNoContent {
		t.Errorf("kept response status = %d, want 204", s)
	}
}

// TestContext_KeptContextsStayDistinct: requests served one after another on
// one goroutine — where a pooled Context would be handed back each time —
// each keep a Context of their own.
func TestContext_KeptContextsStayDistinct(t *testing.T) {
	app := mustNew(t)
	var kept []*credo.Context
	app.GET("/items/{id}", func(c *credo.Context) error {
		c.Set(keyTenant, c.Request().RouteParam("id"))
		kept = append(kept, c)
		return c.Response().NoContent(http.StatusNoContent)
	})
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, id := range ids {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/"+id, nil))
	}
	for i, c := range kept {
		v, _ := c.Get(keyTenant)
		if v != ids[i] || c.Request().RouteParam("id") != ids[i] {
			t.Fatalf("kept context %d reads tenant %q, id %q; want %q", i, v, c.Request().RouteParam("id"), ids[i])
		}
	}
}
