package credo_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// mountProbe is a mounted stdlib handler that reports what it was handed: the
// decoded path, the raw path (empty when the two spellings agree) and the
// named path values.
func mountProbe(names ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := r.URL.Path + "|" + r.URL.RawPath
		for _, name := range names {
			out += "|" + name + "=" + r.PathValue(name)
		}
		_, _ = w.Write([]byte(out))
	})
}

func serveGET(h http.Handler, target string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code, rec.Body.String()
}

// TestMount_ParametricPrefix: a mount prefix may carry parameters. The child
// gets the path below the prefix, as a static mount hands it over, and the
// prefix's parameters as path values.
func TestMount_ParametricPrefix(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		params []string
		target string
		want   string
	}{
		{
			name:   "remainder",
			prefix: "/t/{tenant}", params: []string{"tenant"},
			target: "/t/acme/x/y", want: "/x/y||tenant=acme",
		},
		{
			name:   "exact match",
			prefix: "/t/{tenant}", params: []string{"tenant"},
			target: "/t/acme", want: "/||tenant=acme",
		},
		{
			name:   "encoded slash in the remainder stays encoded",
			prefix: "/t/{tenant}", params: []string{"tenant"},
			target: "/t/acme/a%2Fb/c", want: "/a/b/c|/a%2Fb/c|tenant=acme",
		},
		{
			name:   "encoded slash inside the prefix parameter is data",
			prefix: "/t/{tenant}", params: []string{"tenant"},
			target: "/t/ac%2Fme/x/y", want: "/x/y||tenant=ac/me",
		},
		{
			name:   "escaped unreserved bytes in the prefix",
			prefix: "/t/{tenant}", params: []string{"tenant"},
			target: "/%74/%61cme/x", want: "/x||tenant=acme",
		},
		{
			name:   "non-ASCII static text before the parameter",
			prefix: "/été/{tenant}", params: []string{"tenant"},
			target: "/%C3%A9t%C3%A9/acme/caf%C3%A9", want: "/café||tenant=acme",
		},
		{
			name:   "empty segment below the prefix",
			prefix: "/t/{tenant}", params: []string{"tenant"},
			target: "/t/acme//x", want: "//x||tenant=acme",
		},
		{
			name:   "regexp parameter whose expression contains a slash",
			prefix: "/f/{pair:[a-z]+/[a-z]+}", params: []string{"pair"},
			target: "/f/a%2Fb/rest", want: "/rest||pair=a/b",
		},
		{
			name:   "regexp parameter whose expression contains a slash, exact match",
			prefix: "/f/{pair:[a-z]+/[a-z]+}", params: []string{"pair"},
			target: "/f/a%2Fb", want: "/||pair=a/b",
		},
		{
			name:   "regexp quantifier braces",
			prefix: "/y/{year:[0-9]{4}}", params: []string{"year"},
			target: "/y/2026/archive", want: "/archive||year=2026",
		},
		{
			name:   "two parameters in one segment",
			prefix: "/v/{major}.{minor}", params: []string{"major", "minor"},
			target: "/v/1.2/docs/intro", want: "/docs/intro||major=1|minor=2",
		},
		{
			name:   "static text after the parameter",
			prefix: "/org/{org}/api", params: []string{"org"},
			target: "/org/acme/api/users", want: "/users||org=acme",
		},
		{
			name:   "parameters in several segments",
			prefix: "/a/{x}/b/{y}", params: []string{"x", "y"},
			target: "/a/1/b/2/rest", want: "/rest||x=1|y=2",
		},
		{
			name:   "parameter as the whole prefix",
			prefix: "/{area}", params: []string{"area"},
			target: "/shop/cart", want: "/cart||area=shop",
		},
		{
			name:   "trailing slash in the mount pattern",
			prefix: "/t/{tenant}/", params: []string{"tenant"},
			target: "/t/acme/x", want: "/x||tenant=acme",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			// The internal catch-all of the mount is not a path value.
			app.Mount(tt.prefix, mountProbe(append(tt.params, "_mount")...))

			status, got := serveGET(app, tt.target)
			if want := tt.want + "|_mount="; status != http.StatusOK || got != want {
				t.Fatalf("GET %s = %d %q, want 200 %q", tt.target, status, got, want)
			}
		})
	}
}

// TestMount_RootWithTrailingSlash: the prefix followed by a slash is the
// mounted handler's root. The mount's catch-all used to need a non-empty rest,
// so "/admin/" never reached the handler: the router answered 301 to "/admin",
// or 404 with the trailing-slash redirect off.
func TestMount_RootWithTrailingSlash(t *testing.T) {
	for _, redirect := range []bool{true, false} {
		name := "redirect on"
		if !redirect {
			name = "redirect off"
		}
		t.Run(name, func(t *testing.T) {
			app := mustNew(t, credo.WithRedirectTrailingSlash(redirect))
			app.Mount("/admin", mountProbe("tenant", "_mount"))
			app.Mount("/t/{tenant}", mountProbe("tenant", "_mount"))

			for target, want := range map[string]string{
				"/admin":   "/||tenant=|_mount=",
				"/admin/":  "/||tenant=|_mount=",
				"/t/acme":  "/||tenant=acme|_mount=",
				"/t/acme/": "/||tenant=acme|_mount=",
			} {
				if status, got := serveGET(app, target); status != http.StatusOK || got != want {
					t.Errorf("GET %s = %d %q, want 200 %q", target, status, got, want)
				}
			}
		})
	}
}

// TestMount_ParametricPrefix_URLParam: URLParam is the accessor for stdlib
// handlers mounted through App.Mount, and reads the prefix's parameters.
func TestMount_ParametricPrefix_URLParam(t *testing.T) {
	app := mustNew(t)
	app.Mount("/t/{tenant}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(credo.URLParam(r, "tenant") + "|" + credo.URLParam(r, "nope") + "|" + credo.URLParam(r, "_mount")))
	}))

	for _, target := range []string{"/t/acme", "/t/acme/x/y"} {
		if status, got := serveGET(app, target); status != http.StatusOK || got != "acme||" {
			t.Errorf("GET %s = %d %q, want 200 %q", target, status, got, "acme||")
		}
	}
}

// TestMount_ParametricPrefix_CredoChild: a mounted Credo app keeps its own
// route parameters apart from the prefix's, which it reads through PathValue.
func TestMount_ParametricPrefix_CredoChild(t *testing.T) {
	child := mustNew(t)
	child.GET("/items/{id}", func(ctx *credo.Context) error {
		req := ctx.Request()
		params := req.RouteParams()
		for _, name := range []string{"tenant", "_mount"} {
			if _, ok := params[name]; ok {
				return ctx.Response().Text(http.StatusOK, "FAIL: "+name+" in RouteParams")
			}
		}
		return ctx.Response().Text(http.StatusOK, strings.Join([]string{
			"tenant=" + req.PathValue("tenant"),
			"id=" + req.PathValue("id"),
			"route tenant=" + req.RouteParam("tenant"),
			"route id=" + req.RouteParam("id"),
			"_mount=" + req.PathValue("_mount"),
		}, " "))
	})
	child.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "root of "+ctx.Request().PathValue("tenant"))
	})

	parent := mustNew(t)
	parent.Mount("/t/{tenant}", child)

	tests := []struct{ target, want string }{
		{"/t/acme/items/42", "tenant=acme id=42 route tenant= route id=42 _mount="},
		{"/t/acme", "root of acme"},
	}
	for _, tt := range tests {
		if status, got := serveGET(parent, tt.target); status != http.StatusOK || got != tt.want {
			t.Errorf("GET %s = %d %q, want 200 %q", tt.target, status, got, tt.want)
		}
	}
}

// TestMount_ParametricPrefix_Nested: every parametric prefix on the way down
// adds its parameters, so the innermost handler sees all of them. A name used
// twice holds the value of the prefix closest to the handler.
func TestMount_ParametricPrefix_Nested(t *testing.T) {
	t.Run("the innermost handler sees every prefix", func(t *testing.T) {
		middle := mustNew(t)
		middle.Mount("/p/{id}", mountProbe("tenant", "id"))
		outer := mustNew(t)
		outer.Mount("/t/{tenant}", middle)

		tests := []struct{ target, want string }{
			{"/t/acme/p/7/docs/intro", "/docs/intro||tenant=acme|id=7"},
			{"/t/acme/p/7", "/||tenant=acme|id=7"},
			{"/t/ac%2Fme/p/7/a%2Fb", "/a/b|/a%2Fb|tenant=ac/me|id=7"},
		}
		for _, tt := range tests {
			if status, got := serveGET(outer, tt.target); status != http.StatusOK || got != tt.want {
				t.Errorf("GET %s = %d %q, want 200 %q", tt.target, status, got, tt.want)
			}
		}
	})

	t.Run("a Credo route below two prefixes", func(t *testing.T) {
		inner := mustNew(t)
		inner.GET("/docs/{page}", func(ctx *credo.Context) error {
			req := ctx.Request()
			return ctx.Response().Text(http.StatusOK,
				req.PathValue("tenant")+"/"+req.PathValue("id")+"/"+req.RouteParam("page"))
		})
		middle := mustNew(t)
		middle.Mount("/p/{id}", inner)
		outer := mustNew(t)
		outer.Mount("/t/{tenant}", middle)

		if status, got := serveGET(outer, "/t/acme/p/7/docs/intro"); status != http.StatusOK || got != "acme/7/intro" {
			t.Fatalf("GET = %d %q, want 200 %q", status, got, "acme/7/intro")
		}
	})

	t.Run("the closest prefix wins a shared name", func(t *testing.T) {
		middle := mustNew(t)
		middle.Mount("/p/{id}", mountProbe("id"))
		outer := mustNew(t)
		outer.Mount("/t/{id}", middle)

		if status, got := serveGET(outer, "/t/outer/p/inner/x"); status != http.StatusOK || got != "/x||id=inner" {
			t.Fatalf("GET = %d %q, want 200 %q", status, got, "/x||id=inner")
		}
	})
}

// TestMount_ParametricPrefix_LeavesTheCallersPathValues: the child's path
// values are its own. An http.ServeMux in front of the app has matched the
// request with a wildcard of the same name; the mount must not write through
// to the request that mux still holds.
func TestMount_ParametricPrefix_LeavesTheCallersPathValues(t *testing.T) {
	app := mustNew(t)
	app.Mount("/x/{seg}", mountProbe("seg", "rest"))

	var after string
	front := http.NewServeMux()
	front.HandleFunc("/{seg}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		app.ServeHTTP(w, r)
		after = r.PathValue("seg") + "|" + r.PathValue("rest")
	})

	status, got := serveGET(front, "/x/y/z")
	// The child inherits the path values the front mux set and overrides the
	// one its own prefix names.
	if want := "/z||seg=y|rest=y/z"; status != http.StatusOK || got != want {
		t.Fatalf("GET /x/y/z = %d %q, want 200 %q", status, got, want)
	}
	if want := "x|y/z"; after != want {
		t.Fatalf("path values of the front mux's request after the mount = %q, want %q", after, want)
	}
}

// TestMount_ParametricPrefix_MethodScopeAndRoutes: a parametric prefix changes
// neither the methods a mount answers nor its introspection entry.
func TestMount_ParametricPrefix_MethodScopeAndRoutes(t *testing.T) {
	app := mustNew(t)
	app.Mount("/t/{tenant}", mountProbe("tenant"))

	for _, method := range []string{http.MethodPost, http.MethodDelete, "QUERY"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/t/acme/x", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "/x||tenant=acme" {
			t.Errorf("%s /t/acme/x = %d %q, want 200 %q", method, rec.Code, rec.Body.String(), "/x||tenant=acme")
		}
	}
	for _, method := range []string{http.MethodTrace, http.MethodConnect} {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(method, "/t/acme/x", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /t/acme/x = %d, want 405", method, rec.Code)
		}
	}

	var mounts []credo.RouteInfo
	for _, ri := range app.Routes() {
		if strings.Contains(ri.Pattern, "_mount") {
			t.Errorf("internal mount pattern leaked into introspection: %q", ri.Pattern)
		}
		if ri.Kind == credo.RouteKindMount {
			mounts = append(mounts, ri)
		}
	}
	if len(mounts) != 1 || mounts[0].Pattern != "/t/{tenant}" {
		t.Fatalf("mount entries = %+v, want exactly one with Pattern %q", mounts, "/t/{tenant}")
	}
}
