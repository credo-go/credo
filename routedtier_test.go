package credo_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/credo-go/credo"
)

var keyTier = credo.NewKey[string]("test.tier")

// tierTrace records, per request, the middleware tiers that ran and the meta
// the routed tier read.
type tierTrace struct{ steps []string }

func (tr *tierTrace) mw(name string) credo.Middleware {
	return func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			tr.steps = append(tr.steps, name)
			return next(c)
		}
	}
}

func (tr *tierTrace) routed() credo.Middleware {
	return func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			step := "routed:no-route"
			if c.HasRoute() {
				v, _ := c.Route().GetMeta(keyTier)
				step = "routed:" + c.Route().GetPattern() + ":" + v
			}
			tr.steps = append(tr.steps, step)
			return next(c)
		}
	}
}

func okHandler(c *credo.Context) error { return c.Response().Text(http.StatusOK, "ok") }

func TestRoutedTier_RunsOnEveryRouteOfTheApp(t *testing.T) {
	app := mustNew(t)
	tr := &tierTrace{}
	app.SetMeta(keyTier, "app")
	app.PreRouting(tr.mw("pre"))

	g := app.Group("/g").Middleware(tr.mw("group"))
	g.GET("/b", okHandler).Middleware(tr.mw("route"))
	app.GET("/a", okHandler)
	app.Host("h.example").GET("/c", okHandler)
	app.Static("/s", fstest.MapFS{"f.txt": {Data: []byte("file")}})
	app.Mount("/m", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Added after every route above was registered: it still applies.
	app.Middleware(tr.routed())

	cases := []struct {
		method, target, host string
		want                 []string
		status               int
	}{
		{http.MethodGet, "/a", "", []string{"pre", "routed:/a:app"}, http.StatusOK},
		{http.MethodGet, "/g/b", "", []string{"pre", "routed:/g/b:app", "group", "route"}, http.StatusOK},
		{http.MethodGet, "/c", "h.example", []string{"pre", "routed:/c:app"}, http.StatusOK},
		{http.MethodGet, "/s/f.txt", "", []string{"pre", "routed:/s/{_static...}:app"}, http.StatusOK},
		{http.MethodGet, "/m/x", "", []string{"pre", "routed:/m:app"}, http.StatusOK},
		{http.MethodGet, "/m", "", []string{"pre", "routed:/m:app"}, http.StatusOK},
		{http.MethodGet, "/missing", "", []string{"pre"}, http.StatusNotFound},
		{http.MethodPost, "/a", "", []string{"pre"}, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			tr.steps = nil
			r := httptest.NewRequest(tc.method, tc.target, nil)
			if tc.host != "" {
				r.Host = tc.host
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			if !slices.Equal(tr.steps, tc.want) {
				t.Fatalf("tiers = %q, want %q", tr.steps, tc.want)
			}
		})
	}
}

func TestRoutedTier_PanicsAfterPreparation(t *testing.T) {
	app := mustNew(t)
	app.GET("/", okHandler)
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	defer func() {
		got, _ := recover().(string)
		if want := "credo: App.Middleware called after app was compiled or shut down"; got != want {
			t.Fatalf("panic = %q, want %q", got, want)
		}
	}()
	app.Middleware(func(next credo.Handler) credo.Handler { return next })
}

func TestRoutedTier_AuthenticationCoversAMount(t *testing.T) {
	app := mustNew(t)
	app.Middleware(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			if c.Request().Header.Get("Authorization") == "" {
				return credo.NewHTTPError(http.StatusUnauthorized)
			}
			return next(c)
		}
	})
	app.Mount("/admin", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/users", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without credentials: status = %d, want 401", w.Code)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	r.Header.Set("Authorization", "Bearer x")
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("with credentials: status = %d, want 200", w.Code)
	}
}

func TestMount_IsARoute(t *testing.T) {
	app := mustNew(t)
	app.SetMeta(keyTier, "app")
	var entries []credo.AccessLogEntry
	app.UseAccessLog(credo.AccessLogConfig{
		Logger: slog.New(slog.DiscardHandler),
		ResultFilter: func(_ *credo.Context, e credo.AccessLogEntry) bool {
			entries = append(entries, e)
			return false
		},
	})
	var hasRoute bool
	var method, meta string
	app.Middleware(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			hasRoute = c.HasRoute()
			method = c.Route().GetMethod()
			meta, _ = c.Route().GetMeta(keyTier)
			return next(c)
		}
	})
	var childPath, tenant string
	app.Mount("/t/{tenant}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		childPath, tenant = r.URL.Path, credo.URLParam(r, "tenant")
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/t/acme/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !hasRoute || method != "" || meta != "app" {
		t.Fatalf("route: HasRoute=%v method=%q meta=%q, want true, \"\", \"app\"", hasRoute, method, meta)
	}
	if childPath != "/x" || tenant != "acme" {
		t.Fatalf("child request: path=%q tenant=%q, want \"/x\", \"acme\"", childPath, tenant)
	}
	if len(entries) != 1 || entries[0].Route != "/t/{tenant}" {
		t.Fatalf("access entries = %+v, want one naming /t/{tenant}", entries)
	}

	var info *credo.RouteInfo
	for _, ri := range app.Routes() {
		if ri.Kind == credo.RouteKindMount {
			info = &ri
		}
	}
	if info == nil {
		t.Fatal("Routes() has no mount")
	}
	if v, ok := info.GetMeta(keyTier); !ok || v != "app" {
		t.Fatalf("mount RouteInfo.GetMeta = %q, %v, want the App's meta", v, ok)
	}
}

func TestMount_MetaSilencesItsAccessRecord(t *testing.T) {
	app := mustNew(t)
	app.SetMeta(credo.MetaAccessLog, false)
	var buf strings.Builder
	app.UseAccessLog(credo.AccessLogConfig{Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	app.Mount("/m", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/m/x", nil))
	if buf.Len() != 0 {
		t.Fatalf("App-level MetaAccessLog=false did not silence the mount: %s", buf.String())
	}
}

func TestPreRouting_RunsForNotFoundAndMethodNotAllowed(t *testing.T) {
	app := mustNew(t)
	var ran int
	app.PreRouting(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			ran++
			if c.HasRoute() {
				t.Error("PreRouting saw a route")
			}
			return next(c)
		}
	})
	app.GET("/a", okHandler)
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/missing", nil),
		httptest.NewRequest(http.MethodPost, "/a", nil),
	} {
		app.ServeHTTP(httptest.NewRecorder(), r)
	}
	if ran != 2 {
		t.Fatalf("PreRouting ran %d times, want 2", ran)
	}
}
