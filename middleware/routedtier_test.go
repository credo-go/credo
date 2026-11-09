package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func corsPreflight() *http.Request {
	r := httptest.NewRequest(http.MethodOptions, "/items", nil)
	r.Header.Set("Origin", "https://example.com")
	r.Header.Set("Access-Control-Request-Method", http.MethodPost)
	return r
}

// CORS answers a preflight — an OPTIONS request the router has no route for —
// only before routing; on the routed tier the router's 405 answers it.
func TestCORS_PreRoutingAnswersAPreflightTheRoutedTierCannot(t *testing.T) {
	cors := func() credo.Middleware {
		return middleware.CORS(middleware.CORSConfig{
			AllowOrigins: []string{"https://example.com"},
			AllowMethods: []string{http.MethodGet, http.MethodPost},
		})
	}

	pre := mustNew(t)
	pre.PreRouting(cors())
	pre.GET("/items", contractOK)
	w := httptest.NewRecorder()
	pre.ServeHTTP(w, corsPreflight())
	if w.Code != http.StatusNoContent {
		t.Fatalf("PreRouting: status = %d, want 204", w.Code)
	}

	routed := mustNew(t)
	routed.Middleware(cors())
	routed.GET("/items", contractOK)
	w = httptest.NewRecorder()
	routed.ServeHTTP(w, corsPreflight())
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("routed tier: status = %d, want 405", w.Code)
	}
}

func TestContractGuard_PreRoutingFailsClosed(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	app.PreRouting(middleware.ContractGuard())
	app.GET("/open", contractOK)

	for _, target := range []string{"/open", "/open", "/missing"} {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("GET %s: status = %d, want 500", target, w.Code)
		}
	}

	const msg = "contractguard: no route is matched, so no contract can be checked: " +
		"the guard is registered with App.PreRouting, which runs before the match; " +
		"register it with app.Middleware, on a group or on a route; request denied"
	var guardErrors int
	for line := range strings.Lines(buf.String()) {
		if strings.Contains(line, `"level":"ERROR"`) && strings.Contains(line, `"msg":"`+msg+`"`) {
			guardErrors++
		}
	}
	if guardErrors != 1 {
		t.Fatalf("the guard logged %d Errors with its message, want 1:\n%s", guardErrors, buf.String())
	}
}

func TestContractGuard_RoutedTierReadsEveryRoutesMeta(t *testing.T) {
	app := mustNew(t)
	app.Middleware(middleware.ContractGuard())
	app.POST("/json", contractOK).SetMeta(middleware.MetaAccept, []string{"application/json"})
	app.Group("/g").POST("/json", contractOK).SetMeta(middleware.MetaAccept, []string{"application/json"})

	for _, target := range []string{"/json", "/g/json"} {
		r := httptest.NewRequest(http.MethodPost, target, strings.NewReader("x"))
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("POST %s: status = %d, want 415", target, w.Code)
		}
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /missing: status = %d, want 404: the routed tier does not run for it", w.Code)
	}
}
