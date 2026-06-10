package middleware_test

import (
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func TestRewrite_SimplePath(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		From: "/old",
		To:   "/new",
	}))
	app.GET("/new", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "new")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "new" {
		t.Errorf("body = %q, want 'new'", w.Body.String())
	}
}

func TestRewriteWithConfig_Skipper(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.RewriteWithConfig(middleware.RewriteConfig{
		Skipper: func(ctx *credo.Context) bool {
			return ctx.Request().Header.Get("X-Skip-Rewrite") == "1"
		},
		Rules: []middleware.RewriteRule{{From: "/old", To: "/new"}},
	}))
	app.GET("/new", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "new")
	})
	app.GET("/old", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "old")
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/old", nil))
	if w.Body.String() != "new" {
		t.Errorf("without skip: body = %q, want 'new'", w.Body.String())
	}

	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old", nil)
	r.Header.Set("X-Skip-Rewrite", "1")
	app.ServeHTTP(w, r)
	if w.Body.String() != "old" {
		t.Errorf("with skip: body = %q, want 'old'", w.Body.String())
	}
}

func TestRewrite_WithParam(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		From: "/old/{id}",
		To:   "/new/{id}",
	}))
	app.GET("/new/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "id:"+ctx.Request().RouteParams()["id"])
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old/42", nil)
	app.ServeHTTP(w, r)

	if w.Body.String() != "id:42" {
		t.Errorf("body = %q, want 'id:42'", w.Body.String())
	}
}

func TestRewrite_CatchAll(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		From: "/old/{path...}",
		To:   "/new/{path}",
	}))
	app.GET("/new/{path...}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "path:"+ctx.Request().RouteParams()["path"])
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old/a/b/c", nil)
	app.ServeHTTP(w, r)

	if w.Body.String() != "path:a/b/c" {
		t.Errorf("body = %q, want 'path:a/b/c'", w.Body.String())
	}
}

func TestRewrite_RegexConstraint(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		From: "/users/{id:[0-9]+}",
		To:   "/api/users/{id}",
	}))
	app.GET("/api/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "user:"+ctx.Request().RouteParams()["id"])
	})
	app.GET("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "fallback")
	})

	// Numeric ID → rewrite
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/42", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "user:42" {
		t.Errorf("body = %q, want 'user:42'", w.Body.String())
	}

	// Non-numeric → no rewrite, fallback
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/users/abc", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "fallback" {
		t.Errorf("body = %q, want 'fallback'", w.Body.String())
	}
}

func TestRewrite_RegexConstraintWithCharClassBrace(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		From: "/legacy/{slug:[a-z}]+}",
		To:   "/items/{slug}",
	}))
	app.GET("/items/{slug}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.Request().RouteParams()["slug"])
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/legacy/ab}c", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%q", w.Code, w.Body.String())
	}
	if w.Body.String() != "ab}c" {
		t.Errorf("body = %q, want ab}c", w.Body.String())
	}
}

func TestRewrite_RegexpField(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		Regexp: regexp.MustCompile(`^/blog/(?P<year>\d{4})/(?P<slug>[^/]+)$`),
		To:     "/posts/{year}/{slug}",
	}))
	app.GET("/posts/{year}/{slug}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, ctx.Request().RouteParams()["year"]+"/"+ctx.Request().RouteParams()["slug"])
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/blog/2026/hello-world", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "2026/hello-world" {
		t.Errorf("body = %q, want '2026/hello-world'", w.Body.String())
	}
}

func TestRewrite_FirstMatchWins(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(
		middleware.RewriteRule{From: "/a", To: "/first"},
		middleware.RewriteRule{From: "/a", To: "/second"},
	))
	app.GET("/first", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "first")
	})
	app.GET("/second", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "second")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/a", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "first" {
		t.Errorf("body = %q, want 'first'", w.Body.String())
	}
}

func TestRewrite_NoMatch(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		From: "/old",
		To:   "/new",
	}))
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "test")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "test" {
		t.Errorf("body = %q, want 'test' (no rewrite)", w.Body.String())
	}
}

func TestRewrite_HostFilter(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		Host: "api.example.com",
		From: "/v1/{path...}",
		To:   "/v2/{path}",
	}))
	app.GET("/v2/{path...}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "v2:"+ctx.Request().RouteParams()["path"])
	})
	app.GET("/v1/{path...}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "v1:"+ctx.Request().RouteParams()["path"])
	})

	// Matching host → rewrite
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/users", nil)
	r.Host = "api.example.com"
	app.ServeHTTP(w, r)
	if w.Body.String() != "v2:users" {
		t.Errorf("body = %q, want 'v2:users'", w.Body.String())
	}

	// Wrong host → no rewrite
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/v1/users", nil)
	r.Host = "other.example.com"
	app.ServeHTTP(w, r)
	if w.Body.String() != "v1:users" {
		t.Errorf("body = %q, want 'v1:users'", w.Body.String())
	}
}

func TestRewrite_QueryOverride(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		From: "/old",
		To:   "/new?forced=true",
	}))
	app.GET("/new", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "q="+ctx.Request().QueryParam("forced"))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old?original=yes", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "q=true" {
		t.Errorf("body = %q, want 'q=true'", w.Body.String())
	}
}

func TestRewrite_PreserveQuery(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(middleware.RewriteRule{
		From:          "/old",
		To:            "/new",
		PreserveQuery: true,
	}))
	app.GET("/new", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "q="+ctx.Request().QueryParam("keep"))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old?keep=me", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "q=me" {
		t.Errorf("body = %q, want 'q=me'", w.Body.String())
	}
}

func TestRewrite_EmptyRulesPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty rules")
		}
	}()
	middleware.Rewrite()
}

func TestRewrite_IntegrationWithRouting(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(middleware.Rewrite(
		middleware.RewriteRule{From: "/legacy/{id}", To: "/api/v2/items/{id}"},
	))
	app.GET("/api/v2/items/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "item:"+ctx.Request().RouteParams()["id"])
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/legacy/99", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "item:99" {
		t.Errorf("body = %q, want 'item:99'", w.Body.String())
	}
}
