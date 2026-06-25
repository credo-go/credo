package credo_test

import (
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

func registeredAt(t *testing.T, routes []credo.RouteInfo, method, pattern string) string {
	t.Helper()
	for _, r := range routes {
		if r.Method == method && r.Pattern == pattern {
			return r.RegisteredAt
		}
	}
	t.Fatalf("route %s %q not found in %d routes", method, pattern, len(routes))
	return ""
}

// here returns "file:lineOfCaller+delta" — used to name the exact registration
// line a following call sits on.
func here(delta int) string {
	_, file, line, _ := runtime.Caller(1)
	return file + ":" + strconv.Itoa(line+delta)
}

func TestRegisteredAt_NormalRoutePointsAtUserCallSite(t *testing.T) {
	app := mustNew(t)
	want := here(1)
	app.GET("/x", func(c *credo.Context) error { return nil })

	if got := registeredAt(t, app.Routes(), "GET", "/x"); got != want {
		t.Errorf("RegisteredAt = %q, want %q (user call site, not framework internals)", got, want)
	}
}

func TestRegisteredAt_AutoHeadTwinSharesGetCallSite(t *testing.T) {
	app := mustNew(t)
	want := here(1)
	app.GET("/x", func(c *credo.Context) error { return nil })

	routes := app.Routes()
	if got := registeredAt(t, routes, "GET", "/x"); got != want {
		t.Errorf("GET RegisteredAt = %q, want %q", got, want)
	}
	if got := registeredAt(t, routes, "HEAD", "/x"); got != want {
		t.Errorf("auto HEAD twin RegisteredAt = %q, want %q (must match the GET call site)", got, want)
	}
}

func TestRegisteredAt_GroupRoute(t *testing.T) {
	app := mustNew(t)
	g := app.Group("/api")
	want := here(1)
	g.POST("/users", func(c *credo.Context) error { return nil })

	if got := registeredAt(t, app.Routes(), "POST", "/api/users"); got != want {
		t.Errorf("group route RegisteredAt = %q, want %q", got, want)
	}
}

// Static funnels through App.Static → Group.Static → addGetRoute → addRoute
// (twice, plus HEAD twins): a deeper funnel than a plain GET. Every derived
// route must still resolve to the single user Static() call site, proving the
// capture is funnel-depth independent.
func TestRegisteredAt_StaticAllDerivedRoutesShareCallSite(t *testing.T) {
	app := mustNew(t)
	want := here(1)
	app.Static("/assets", os.DirFS("."))

	var seen int
	for _, r := range app.Routes() {
		if !strings.HasPrefix(r.Pattern, "/assets") {
			continue
		}
		seen++
		if r.RegisteredAt != want {
			t.Errorf("static route %s %q RegisteredAt = %q, want %q", r.Method, r.Pattern, r.RegisteredAt, want)
		}
	}
	if seen == 0 {
		t.Fatal("no /assets routes found")
	}
}

func TestRegisteredAt_HostRoute(t *testing.T) {
	app := mustNew(t)
	h := app.Host("api.example.com")
	want := here(1)
	h.GET("/v1", func(c *credo.Context) error { return nil })

	for _, r := range app.Routes() {
		if r.Host == "api.example.com" && r.Method == "GET" && r.Pattern == "/v1" {
			if r.RegisteredAt != want {
				t.Errorf("host route RegisteredAt = %q, want %q", r.RegisteredAt, want)
			}
			return
		}
	}
	t.Fatal("host route GET api.example.com /v1 not found")
}

func TestRegisteredAt_Mount(t *testing.T) {
	app := mustNew(t)
	want := here(1)
	app.Mount("/admin", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, r := range app.Routes() {
		if r.Kind == credo.RouteKindMount && r.Pattern == "/admin" {
			if r.RegisteredAt != want {
				t.Errorf("mount RegisteredAt = %q, want %q", r.RegisteredAt, want)
			}
			return
		}
	}
	t.Fatal("mount /admin not found")
}

func TestDuplicateRoute_PanicNamesBothLocations(t *testing.T) {
	app := mustNew(t)
	firstLoc := here(1)
	app.GET("/dup", func(c *credo.Context) error { return nil })

	var secondLoc string
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic for duplicate route registration")
		}
		msg, ok := v.(string)
		if !ok {
			t.Fatalf("panic = %#v, want string", v)
		}
		for _, want := range []string{
			"duplicate route",
			"first registered at " + firstLoc,
			"now registered at " + secondLoc,
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message = %q\n  missing substring %q", msg, want)
			}
		}
	}()

	secondLoc = here(1)
	app.GET("/dup", func(c *credo.Context) error { return nil })
}
