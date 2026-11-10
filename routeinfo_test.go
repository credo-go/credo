package credo_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/credo-go/credo"
)

var keyViewScope = credo.NewKey[string]("routeinfo.scope")

// serveView serves one request and returns its recorder.
func serveView(app *credo.App, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

// TestRouteInfo_MiddlewareReadsMeta: a middleware reads the matched route's
// meta as c.Route().GetMeta(key), with the route's own value over its
// group's and the group's over the App's.
func TestRouteInfo_MiddlewareReadsMeta(t *testing.T) {
	app := mustNew(t)
	app.SetMeta(keyViewScope, "app")
	app.Middleware(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			scope, ok := c.Route().GetMeta(keyViewScope)
			if !ok {
				scope = "(unset)"
			}
			c.Response().Header().Set("X-Scope", scope)
			return next(c)
		}
	})
	ok := func(c *credo.Context) error { return c.Response().NoContent(http.StatusNoContent) }
	app.GET("/plain", ok)
	admin := app.Group("/admin").SetMeta(keyViewScope, "group")
	admin.GET("/users", ok)
	admin.GET("/audit", ok).SetMeta(keyViewScope, "route")

	tests := []struct{ target, want string }{
		{"/plain", "app"},
		{"/admin/users", "group"},
		{"/admin/audit", "route"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			if got := serveView(app, http.MethodGet, tt.target).Header().Get("X-Scope"); got != tt.want {
				t.Errorf("c.Route().GetMeta = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRouteInfo_OneViewForEveryReader: for one request, Context.Route,
// Exchange.Route, App.GetRoute and the route's entry of App.Routes are equal,
// for a route and for a mount.
func TestRouteInfo_OneViewForEveryReader(t *testing.T) {
	tests := []struct {
		name   string
		target string
		// named is the route's name, for App.GetRoute; a mount has none.
		named string
		want  credo.RouteInfo
	}{
		{
			name: "a route", target: "/users/42", named: "user.show",
			want: credo.RouteInfo{Method: "GET", Pattern: "/users/{id}", Name: "user.show", Kind: credo.RouteKindRoute},
		},
		{
			name: "a host-scoped route", target: "http://acme.myapp.com/items", named: "tenant.items",
			want: credo.RouteInfo{Method: "GET", Pattern: "/items", Host: "{tenant}.myapp.com", Name: "tenant.items", Kind: credo.RouteKindRoute},
		},
		{
			name: "a mount", target: "/legacy/reports/7",
			want: credo.RouteInfo{
				Pattern: "/legacy", Kind: credo.RouteKindMount,
				Methods: []string{"DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "QUERY"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fromContext, fromExchange credo.RouteInfo
			app := mustNew(t)
			app.SetMeta(keyViewScope, "app")
			app.Middleware(func(next credo.Handler) credo.Handler {
				return func(c *credo.Context) error {
					fromContext = c.Route()
					return next(c)
				}
			})
			app.AddObserver(observerFunc(func(ex *credo.Exchange) { fromExchange = ex.Route() }))
			ok := func(c *credo.Context) error { return c.Response().NoContent(http.StatusNoContent) }
			app.GET("/users/{id}", ok).Name("user.show")
			app.Host("{tenant}.myapp.com").GET("/items", ok).Name("tenant.items")
			app.Mount("/legacy", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))

			if w := serveView(app, http.MethodGet, tt.target); w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", w.Code)
			}

			// The exported fields are the route's; RegisteredAt is this file.
			got := fromContext
			if !strings.Contains(got.RegisteredAt, "routeinfo_test.go:") {
				t.Errorf("RegisteredAt = %q, want a line of this file", got.RegisteredAt)
			}
			if got.Method != tt.want.Method || got.Pattern != tt.want.Pattern || got.Host != tt.want.Host ||
				got.Name != tt.want.Name || got.Kind != tt.want.Kind || got.AutoHead || !slices.Equal(got.Methods, tt.want.Methods) {
				t.Errorf("Context.Route() = %+v, want the fields of %+v", got, tt.want)
			}
			if scope, _ := got.GetMeta(keyViewScope); scope != "app" {
				t.Errorf("GetMeta = %q, want the App's value", scope)
			}

			if !credo.SameRouteView(fromExchange, fromContext) {
				t.Errorf("Exchange.Route() = %+v, want what Context.Route() returned, %+v", fromExchange, fromContext)
			}
			idx := slices.IndexFunc(app.Routes(), func(ri credo.RouteInfo) bool {
				return ri.Pattern == tt.want.Pattern && ri.Method == tt.want.Method && ri.Host == tt.want.Host
			})
			if idx < 0 {
				t.Fatalf("App.Routes() has no entry for %s %q", tt.want.Method, tt.want.Pattern)
			}
			if listed := app.Routes()[idx]; !credo.SameRouteView(listed, fromContext) {
				t.Errorf("App.Routes() entry = %+v, want what Context.Route() returned, %+v", listed, fromContext)
			}
			if tt.named != "" {
				named, found := app.GetRoute(tt.named)
				if !found || !credo.SameRouteView(named, fromContext) {
					t.Errorf("App.GetRoute(%q) = %+v, %v; want what Context.Route() returned, %+v", tt.named, named, found, fromContext)
				}
			}
		})
	}
}

// TestRouteInfo_NoRouteIsTheZeroView: where no route matched — pre-routing
// middleware and a custom 404 handler — HasRoute is false and Route is the
// zero RouteInfo: empty fields, no meta, and URL generation that fails with
// an error naming what is missing and where a usable view comes from.
func TestRouteInfo_NoRouteIsTheZeroView(t *testing.T) {
	check := func(t *testing.T, where string, has bool, ri credo.RouteInfo) {
		t.Helper()
		if has {
			t.Errorf("%s: HasRoute() = true, want false", where)
		}
		if !credo.SameRouteView(ri, credo.RouteInfo{}) {
			t.Errorf("%s: Route() = %+v, want the zero RouteInfo", where, ri)
		}
		if v, ok := ri.GetMeta(keyViewScope); ok || v != "" {
			t.Errorf("%s: GetMeta = %q, %v; want the zero value and false", where, v, ok)
		}
	}

	var (
		preHas, notFoundHas, observedHas bool
		preView, notFoundView, observed  credo.RouteInfo
	)
	observedHas = true
	app := mustNew(t)
	app.SetMeta(keyViewScope, "app")
	app.PreRouting(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			preHas, preView = c.HasRoute(), c.Route()
			return next(c)
		}
	})
	app.StatusHandler(http.StatusNotFound, func(c *credo.Context) error {
		notFoundHas, notFoundView = c.HasRoute(), c.Route()
		return credo.ErrNotFound
	})
	app.AddObserver(observerFunc(func(ex *credo.Exchange) {
		observed = ex.Route()
		observedHas = observed.Pattern != "" || observed.Kind != ""
	}))
	app.GET("/known", func(c *credo.Context) error { return c.Response().NoContent(http.StatusNoContent) })

	if w := serveView(app, http.MethodGet, "/unknown"); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	check(t, "pre-routing middleware", preHas, preView)
	check(t, "custom 404 handler", notFoundHas, notFoundView)
	check(t, "observer of a 404", observedHas, observed)

	if _, ok := app.GetRoute("no.such.name"); ok {
		t.Error(`GetRoute("no.such.name") reported a route`)
	}

	for _, build := range []struct {
		name string
		call func(...string) (string, error)
	}{
		{"BuildURI", credo.RouteInfo{}.BuildURI},
		{"BuildURL", credo.RouteInfo{}.BuildURL},
	} {
		t.Run(build.name+" of the zero view", func(t *testing.T) {
			got, err := build.call("42")
			if err == nil || got != "" {
				t.Fatalf("%s = %q, %v; want an error", build.name, got, err)
			}
			for _, want := range []string{
				"credo: " + build.name + ": the RouteInfo describes no route", // what failed
				"App.GetRoute or App.Routes",                                  // the remedy
				"HasRoute is true",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestRouteInfo_ViewFollowsRegistration: before the App is prepared the view
// is read from the route as it stands, so a name or a meta value set after a
// first read shows in the next; a request then sees the final values.
func TestRouteInfo_ViewFollowsRegistration(t *testing.T) {
	app := mustNew(t)
	var served credo.RouteInfo
	route := app.GET("/reports", func(c *credo.Context) error {
		served = c.Route()
		return c.Response().NoContent(http.StatusNoContent)
	}).Name("reports")

	early, ok := app.GetRoute("reports")
	if _, has := early.GetMeta(keyViewScope); !ok || has {
		t.Fatalf("GetRoute before SetMeta = %+v, %v; want the route without the meta value", early, ok)
	}
	route.SetMeta(keyViewScope, "reports:read").Name("reports.list")

	if _, stale := app.GetRoute("reports"); stale {
		t.Error("the old name still resolves after a rename")
	}
	serveView(app, http.MethodGet, "/reports")
	if scope, _ := served.GetMeta(keyViewScope); served.Name != "reports.list" || scope != "reports:read" {
		t.Errorf("the request's view = name %q, scope %q; want reports.list and reports:read", served.Name, scope)
	}
	if listed, _ := app.GetRoute("reports.list"); !credo.SameRouteView(listed, served) {
		t.Errorf("GetRoute after start = %+v, want the request's view %+v", listed, served)
	}
}

// TestRouteInfo_RouteAllocatesNothing: the view is built once, when the
// route's chain is compiled, and returned by value, for every kind of route
// a request can match.
func TestRouteInfo_RouteAllocatesNothing(t *testing.T) {
	tests := []struct {
		name, method, target string
		want                 credo.RouteInfo
	}{
		{"a route", http.MethodGet, "/users/42", credo.RouteInfo{Method: "GET", Pattern: "/users/{id}", Kind: credo.RouteKindRoute}},
		{"the HEAD twin of a GET route", http.MethodHead, "/users/42", credo.RouteInfo{Method: "HEAD", Pattern: "/users/{id}", Kind: credo.RouteKindRoute, AutoHead: true}},
		{"a host-scoped route", http.MethodGet, "http://acme.myapp.com/items", credo.RouteInfo{Method: "GET", Pattern: "/items", Host: "{tenant}.myapp.com", Kind: credo.RouteKindRoute}},
		{"a mount", http.MethodPost, "/legacy/reports", credo.RouteInfo{Pattern: "/legacy", Kind: credo.RouteKindMount}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				seen   credo.RouteInfo
				allocs float64
			)
			app := mustNew(t)
			app.SetMeta(keyViewScope, "app")
			app.Middleware(func(next credo.Handler) credo.Handler {
				return func(c *credo.Context) error {
					seen = c.Route()
					allocs = testing.AllocsPerRun(100, func() {
						ri := c.Route()
						if ri.Pattern == "" {
							panic("no view")
						}
						if _, ok := ri.GetMeta(keyViewScope); !ok {
							panic("no meta")
						}
					})
					return next(c)
				}
			})
			ok := func(c *credo.Context) error { return c.Response().NoContent(http.StatusNoContent) }
			app.GET("/users/{id}", ok)
			app.Host("{tenant}.myapp.com").GET("/items", ok)
			app.Mount("/legacy", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))

			if w := serveView(app, tt.method, tt.target); w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", w.Code)
			}
			if seen.Method != tt.want.Method || seen.Pattern != tt.want.Pattern || seen.Host != tt.want.Host ||
				seen.Kind != tt.want.Kind || seen.AutoHead != tt.want.AutoHead {
				t.Errorf("Context.Route() = %+v, want the fields of %+v", seen, tt.want)
			}
			if allocs != 0 {
				t.Errorf("Context.Route() and GetMeta allocate %v times per call, want 0", allocs)
			}
		})
	}
}

// TestRouteInfo_ReadersAreSafeDuringPreparation: App.GetRoute and App.Routes
// may run while another goroutine makes the first ServeHTTP call, which
// compiles the routes. Run with -race, this fails when either call reads what
// compile writes.
func TestRouteInfo_ReadersAreSafeDuringPreparation(t *testing.T) {
	const routes = 50
	app := mustNew(t)
	app.SetMeta(keyViewScope, "app")
	ok := func(c *credo.Context) error { return c.Response().NoContent(http.StatusNoContent) }
	for i := range routes {
		app.GET(fmt.Sprintf("/r%d/{id}", i), ok).Name(fmt.Sprintf("r%d", i)).SetMeta(keyViewScope, "route")
	}
	app.Host("{tenant}.myapp.com").GET("/items", ok).Name("tenant.items")
	app.Host("api.myapp.com").GET("/items", ok).Name("api.items")
	app.Mount("/legacy", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	// Each GET has a HEAD twin; the mount is one entry.
	const entries = 2*routes + 2*2 + 1

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		if w := serveView(app, http.MethodGet, "/r0/1"); w.Code != http.StatusNoContent {
			t.Errorf("first ServeHTTP: status = %d, want 204", w.Code)
		}
	})
	wg.Go(func() {
		<-start
		for i := range 2000 {
			name := fmt.Sprintf("r%d", i%routes)
			ri, found := app.GetRoute(name)
			scope, _ := ri.GetMeta(keyViewScope)
			if !found || ri.Name != name || scope != "route" {
				t.Errorf("GetRoute(%q) = %+v, %v, scope %q; want the named route with its meta", name, ri, found, scope)
				return
			}
			if i%400 == 0 {
				if got := len(app.Routes()); got != entries {
					t.Errorf("len(Routes()) = %d, want %d", got, entries)
					return
				}
			}
		}
	})
	close(start)
	wg.Wait()

	// What the readers saw during preparation is what they see after it.
	if ri, found := app.GetRoute("tenant.items"); !found || ri.Host != "{tenant}.myapp.com" {
		t.Errorf(`GetRoute("tenant.items") after preparation = %+v, %v`, ri, found)
	}
}

// TestRouteInfo_RoutesOwnsMethods: the caller of App.Routes owns each entry's
// Methods slice, before and after preparation, so writing to it changes
// neither what a request reads nor what a later call returns.
func TestRouteInfo_RoutesOwnsMethods(t *testing.T) {
	var fromRequest []string
	app := mustNew(t)
	app.Middleware(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			fromRequest = slices.Clone(c.Route().Methods)
			return next(c)
		}
	})
	app.Mount("/legacy", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	want := []string{"DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "QUERY"}

	scribble := func(t *testing.T, when string) {
		t.Helper()
		listed := app.Routes()
		if len(listed) != 1 || !slices.Equal(listed[0].Methods, want) {
			t.Fatalf("%s: Routes() = %+v, want the mount with its forwarded methods", when, listed)
		}
		listed[0].Methods[0] = "X"
		if again := app.Routes()[0].Methods; !slices.Equal(again, want) {
			t.Errorf("%s: a later Routes() returned %v after the caller wrote to an earlier result, want %v", when, again, want)
		}
	}

	scribble(t, "before preparation")
	serveView(app, http.MethodGet, "/legacy/x")
	scribble(t, "after preparation")
	serveView(app, http.MethodGet, "/legacy/x")
	if !slices.Equal(fromRequest, want) {
		t.Errorf("a request read Methods = %v after a caller wrote to a Routes() result, want %v", fromRequest, want)
	}
}
