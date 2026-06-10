package credo_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

func TestApp_HelloWorld(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().JSON(200, map[string]string{"message": "Hello, Credo!"})
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Hello, Credo!") {
		t.Errorf("body = %q, want to contain 'Hello, Credo!'", body)
	}
}

func TestApp_URLParams(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().JSON(200, map[string]string{"id": ctx.Request().RouteParams()["id"]})
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/42", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"id":"42"`) {
		t.Errorf("body = %q, want to contain id:42", body)
	}
}

func TestApp_POST_BindBody(t *testing.T) {
	app := mustNew(t)

	type input struct {
		Name string `json:"name"`
	}

	app.POST("/users", func(ctx *credo.Context) error {
		var in input
		if err := ctx.Request().BindBody(&in); err != nil {
			return err
		}
		return ctx.Response().JSON(201, map[string]string{"name": in.Name})
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/users", strings.NewReader(`{"name":"Alice"}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	if w.Code != 201 {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Alice") {
		t.Errorf("body = %q, want to contain 'Alice'", body)
	}
}

func TestApp_BindBody_Validation(t *testing.T) {
	app := mustNew(t)

	type input struct {
		Name string `json:"name"`
	}

	// input does not implement Validatable, so no validation error.

	app.POST("/users", func(ctx *credo.Context) error {
		var in input
		if err := ctx.Request().BindBody(&in); err != nil {
			return err
		}
		return ctx.Response().JSON(200, in)
	})

	// Valid request
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/users", strings.NewReader(`{"name":"Bob"}`))
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	// Invalid JSON
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/users", strings.NewReader(`{invalid`))
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestApp_NamedRoutes(t *testing.T) {
	app := mustNew(t)
	app.GET("/products/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	}).Name("product.show")

	route := app.GetRoute("product.show")
	if route == nil {
		t.Fatal("expected to find route 'product.show'")
	}

	uri, err := route.BuildURI("42")
	if err != nil {
		t.Fatalf("BuildURI returned error: %v", err)
	}
	if uri != "/products/42" {
		t.Errorf("BuildURI = %q, want %q", uri, "/products/42")
	}
}

func TestApp_NamedRoutes_Rename(t *testing.T) {
	app := mustNew(t)
	route := app.GET("/x", func(ctx *credo.Context) error { return nil }).Name("a").Name("b")

	if app.GetRoute("a") != nil {
		t.Error("old name 'a' should be deregistered after rename")
	}
	if app.GetRoute("b") != route {
		t.Error("new name 'b' should resolve to the route")
	}
	if route.GetName() != "b" {
		t.Errorf("route.GetName() = %q, want %q", route.GetName(), "b")
	}
}

func TestApp_NamedRoutes_Duplicate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate route name")
		}
	}()

	app := mustNew(t)
	app.GET("/a", func(ctx *credo.Context) error { return nil }).Name("dup")
	app.GET("/b", func(ctx *credo.Context) error { return nil }).Name("dup")
}

func TestApp_NamedRoutes_RenameConflict_PreservesOldName(t *testing.T) {
	app := mustNew(t)
	first := app.GET("/a", func(ctx *credo.Context) error { return nil }).Name("first")
	second := app.GET("/b", func(ctx *credo.Context) error { return nil }).Name("second")

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate route name")
		}

		if app.GetRoute("first") != first {
			t.Error("old name should remain registered after failed rename")
		}
		if app.GetRoute("second") != second {
			t.Error("existing conflicting name should still point to original route")
		}
		if first.GetName() != "first" {
			t.Errorf("first.GetName() = %q, want %q", first.GetName(), "first")
		}
	}()

	first.Name("second")
}

func TestApp_NamedRoutes_ClearName(t *testing.T) {
	app := mustNew(t)
	route := app.GET("/a", func(ctx *credo.Context) error { return nil }).Name("myRoute")

	if app.GetRoute("myRoute") != route {
		t.Fatal("route should be registered under 'myRoute'")
	}

	// Clearing the name should deregister without registering "".
	route.Name("")

	if route.GetName() != "" {
		t.Errorf("route.GetName() = %q, want empty", route.GetName())
	}
	if app.GetRoute("myRoute") != nil {
		t.Error("old name 'myRoute' should be deregistered")
	}
	if app.GetRoute("") != nil {
		t.Error("empty string should NOT be registered as a route name")
	}
}

func TestApp_NamedRoutes_ClearName_NoPanicOnSecondClear(t *testing.T) {
	app := mustNew(t)
	r1 := app.GET("/a", func(ctx *credo.Context) error { return nil }).Name("r1")
	r2 := app.GET("/b", func(ctx *credo.Context) error { return nil }).Name("r2")

	// Both routes clear their name — should not panic.
	r1.Name("")
	r2.Name("")

	if app.GetRoute("") != nil {
		t.Error("empty string should NOT be registered")
	}
}

func TestApp_RouteMeta(t *testing.T) {
	app := mustNew(t)
	app.SetMeta("app-level", true)

	app.GET("/test", func(ctx *credo.Context) error {
		val, ok := ctx.Route().LookupMeta("auth")
		if !ok {
			return ctx.Response().Text(200, "no-auth")
		}
		if val.(bool) {
			return ctx.Response().Text(200, "authed")
		}
		return ctx.Response().Text(200, "no-auth")
	}).SetMeta("auth", true)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Body.String() != "authed" {
		t.Errorf("body = %q, want 'authed'", w.Body.String())
	}
}

func TestApp_RouteMetaInheritance(t *testing.T) {
	app := mustNew(t)
	app.SetMeta("global", "root")

	route := app.GET("/test", func(ctx *credo.Context) error {
		return nil
	})

	// Route should inherit app-level meta
	val, ok := route.LookupMeta("global")
	if !ok {
		t.Fatal("expected to find 'global' meta via inheritance")
	}
	if val != "root" {
		t.Errorf("meta 'global' = %v, want 'root'", val)
	}

	// Route-level meta overrides
	route.SetMeta("global", "overridden")
	val, ok = route.LookupMeta("global")
	if !ok || val != "overridden" {
		t.Errorf("meta 'global' = %v, want 'overridden'", val)
	}
}

func TestApp_GlobalMiddleware(t *testing.T) {
	app := mustNew(t)
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-Global", "yes")
			return next(ctx)
		}
	})

	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	// Global MW should run on matched routes
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Global"); got != "yes" {
		t.Errorf("X-Global = %q, want 'yes'", got)
	}

	// Global MW should also run on 404
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/notfound", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Global"); got != "yes" {
		t.Errorf("X-Global on 404 = %q, want 'yes'", got)
	}
}

func TestApp_GroupMiddleware_NotOn404(t *testing.T) {
	app := mustNew(t)

	api := app.Group("/api")
	api.Middleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-Group", "yes")
			return next(ctx)
		}
	})

	api.GET("/exists", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	// Matched route should have group MW
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/exists", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Group"); got != "yes" {
		t.Errorf("X-Group = %q, want 'yes'", got)
	}

	// 404 should NOT have group MW (group MW is baked into matched routes)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/notfound", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Group"); got != "" {
		t.Errorf("X-Group on 404 = %q, want empty", got)
	}
}

func TestApp_RouteMiddleware(t *testing.T) {
	app := mustNew(t)

	cacheMW := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-Cache", "hit")
			return next(ctx)
		}
	}

	app.GET("/cached", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	}).Middleware(cacheMW)

	app.GET("/uncached", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	// Cached route should have the middleware
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/cached", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Cache"); got != "hit" {
		t.Errorf("X-Cache = %q, want 'hit'", got)
	}

	// Uncached route should NOT have the middleware
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/uncached", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-Cache"); got != "" {
		t.Errorf("X-Cache = %q, want empty", got)
	}
}

func TestApp_Group(t *testing.T) {
	app := mustNew(t)

	api := app.Group("/api")
	api.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "users")
	})
	api.GET("/posts", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "posts")
	})

	tests := []struct {
		path string
		code int
		body string
	}{
		{"/api/users", 200, "users"},
		{"/api/posts", 200, "posts"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.path, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
			if tt.body != "" && w.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.body)
			}
		})
	}
}

func TestApp_GroupMiddleware(t *testing.T) {
	app := mustNew(t)

	api := app.Group("/api")
	api.Middleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-API", "true")
			return next(ctx)
		}
	})
	api.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "users")
	})

	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "root")
	})

	// API route should have group MW
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/users", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-API"); got != "true" {
		t.Errorf("X-API = %q, want 'true'", got)
	}

	// Root should NOT have group MW
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if got := w.Header().Get("X-API"); got != "" {
		t.Errorf("X-API on root = %q, want empty", got)
	}
}

func TestGroupMiddleware_AfterRoutes_AppliesAtCompile(t *testing.T) {
	app := mustNew(t)

	api := app.Group("/api")
	api.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "users")
	})

	sub := api.Group("/v1")
	sub.GET("/orders", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "orders")
	})

	// Added AFTER both the route and the sub-group: per-route chains are
	// assembled from the group parent chain at compile time, so the
	// middleware applies to previously registered routes and to routes of
	// previously created sub-groups alike.
	api.Middleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-API", "true")
			return next(ctx)
		}
	})

	for _, path := range []string{"/api/users", "/api/v1/orders"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		app.ServeHTTP(w, r)

		if w.Code != 200 {
			t.Fatalf("%s: status = %d, want 200", path, w.Code)
		}
		if got := w.Header().Get("X-API"); got != "true" {
			t.Errorf("%s: X-API = %q, want \"true\" (group middleware must apply regardless of registration order)", path, got)
		}
	}
}

func TestMiddlewareTiers_ExecutionOrder(t *testing.T) {
	app := mustNew(t)

	var order []string
	tag := func(name string) credo.Middleware {
		return func(next credo.Handler) credo.Handler {
			return func(ctx *credo.Context) error {
				order = append(order, name)
				return next(ctx)
			}
		}
	}

	app.GlobalMiddleware(tag("global1"), tag("global2"))

	api := app.Group("/api")
	v1 := api.Group("/v1")
	v1.GET("/x", func(ctx *credo.Context) error {
		order = append(order, "handler")
		return ctx.Response().NoContent(204)
	}).Middleware(tag("route"))

	// Group middleware registered AFTER the route: collected at compile.
	// Expected order is Global → Group (parent before child, append order
	// within a group) → Route → handler.
	api.Middleware(tag("group-parent1"), tag("group-parent2"))
	v1.Middleware(tag("group-child"))

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/x", nil))

	want := []string{"global1", "global2", "group-parent1", "group-parent2", "group-child", "route", "handler"}
	if !slices.Equal(order, want) {
		t.Errorf("execution order = %v, want %v", order, want)
	}
}

func TestApp_HEADAutoHandling(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "body-content")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("HEAD", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// HEAD should not have body
	if w.Body.Len() > 0 {
		t.Errorf("HEAD body = %q, want empty", w.Body.String())
	}
}

func TestApp_DefaultErrorHandling(t *testing.T) {
	app := mustNew(t)
	app.GET("/error", func(ctx *credo.Context) error {
		return credo.NewHTTPError(422, "validation failed")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/error", nil)
	app.ServeHTTP(w, r)

	if w.Code != 422 {
		t.Errorf("status = %d, want 422", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "validation failed") {
		t.Errorf("body = %q, want to contain 'validation failed'", body)
	}
}

func TestApp_CustomErrorRenderer(t *testing.T) {
	app := mustNew(t)
	app.SetErrorRenderer(func(ctx *credo.Context, info credo.ErrorInfo) {
		ctx.Response().Text(info.Problem.Status, "custom: "+info.Problem.Title)
	})

	app.GET("/fail", func(ctx *credo.Context) error {
		return credo.NewHTTPError(500, "oops")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/fail", nil)
	app.ServeHTTP(w, r)

	body := w.Body.String()
	if !strings.Contains(body, "custom:") {
		t.Errorf("body = %q, want to contain 'custom:'", body)
	}
}

func TestApp_ResponseHelpers(t *testing.T) {
	app := mustNew(t)

	app.GET("/string", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "hello")
	})
	app.GET("/html", func(ctx *credo.Context) error {
		return ctx.Response().HTML(200, "<h1>hello</h1>")
	})
	app.GET("/nocontent", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(204)
	})
	app.GET("/redirect", func(ctx *credo.Context) error {
		return ctx.Response().Redirect(301, "/target")
	})
	app.GET("/blob", func(ctx *credo.Context) error {
		return ctx.Response().Blob(200, "application/octet-stream", []byte{0x01, 0x02})
	})

	tests := []struct {
		path        string
		code        int
		contentType string
		body        string
	}{
		{"/string", 200, "text/plain; charset=utf-8", "hello"},
		{"/html", 200, "text/html; charset=utf-8", "<h1>hello</h1>"},
		{"/nocontent", 204, "", ""},
		{"/redirect", 301, "", ""},
		{"/blob", 200, "application/octet-stream", "\x01\x02"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.path, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
			if tt.contentType != "" {
				ct := w.Header().Get("Content-Type")
				if ct != tt.contentType {
					t.Errorf("Content-Type = %q, want %q", ct, tt.contentType)
				}
			}
			if tt.body != "" && w.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.body)
			}
		})
	}
}

func TestApp_ContextStore(t *testing.T) {
	app := mustNew(t)

	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			// Global MW can now access credo.Context directly
			return next(ctx)
		}
	})

	app.GET("/store", func(ctx *credo.Context) error {
		ctx.Set("key", "value")
		val := ctx.Get("key")
		return ctx.Response().Text(200, val.(string))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/store", nil)
	app.ServeHTTP(w, r)

	if w.Body.String() != "value" {
		t.Errorf("body = %q, want 'value'", w.Body.String())
	}
}

func TestApp_NestedGroup(t *testing.T) {
	app := mustNew(t)

	api := app.Group("/api")
	v1 := api.Group("/v1")
	v1.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "v1-users")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/users", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "v1-users" {
		t.Errorf("body = %q, want 'v1-users'", w.Body.String())
	}
}

func TestApp_MultipleMethods(t *testing.T) {
	app := mustNew(t)
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "GET")
	})
	app.POST("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(201, "POST")
	})
	app.PUT("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "PUT:"+ctx.Request().RouteParams()["id"])
	})
	app.DELETE("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(204)
	})

	tests := []struct {
		method string
		path   string
		code   int
		body   string
	}{
		{"GET", "/users", 200, "GET"},
		{"POST", "/users", 201, "POST"},
		{"PUT", "/users/42", 200, "PUT:42"},
		{"DELETE", "/users/42", 204, ""},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, tt.path, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
			if tt.body != "" && w.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.body)
			}
		})
	}
}

func TestHTTPError(t *testing.T) {
	e := credo.NewHTTPError(404)
	if e.Code != 404 {
		t.Errorf("Code = %d, want 404", e.Code)
	}
	if e.MessageKey != credo.MsgKeyNotFound {
		t.Errorf("MessageKey = %q, want %q", e.MessageKey, credo.MsgKeyNotFound)
	}

	e = credo.NewHTTPError(400, "bad input")
	if e.MessageKey != "bad input" {
		t.Errorf("MessageKey = %q, want 'bad input'", e.MessageKey)
	}

	inner := credo.NewHTTPError(500)
	wrapped := e.WithInternal(inner)
	if wrapped.Internal != inner {
		t.Error("expected Internal to be set")
	}
	if wrapped.Code != 400 {
		t.Error("WithInternal should preserve Code")
	}
}

func TestApp_Group_GET_AutoHEAD(t *testing.T) {
	app := mustNew(t)

	api := app.Group("/api")
	api.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "users-body")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("HEAD", "/api/users", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.Len() > 0 {
		t.Errorf("HEAD body = %q, want empty", w.Body.String())
	}
}

// --- Routing Tests (previously in mux_test.go) ---

func TestRouting_Basic(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "root")
	})
	app.GET("/hello", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "hello")
	})

	tests := []struct {
		method string
		path   string
		code   int
		body   string
	}{
		{"GET", "/", 200, "root"},
		{"GET", "/hello", 200, "hello"},
		{"GET", "/notfound", 404, ""},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, tt.path, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
			if tt.body != "" {
				body := w.Body.String()
				if body != tt.body {
					t.Errorf("body = %q, want %q", body, tt.body)
				}
			}
		})
	}
}

func TestRouting_URLParams(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/{id}", func(ctx *credo.Context) error {
		id := ctx.Request().RouteParams()["id"]
		return ctx.Response().Text(200, "id:"+id)
	})
	app.GET("/users/{id}/posts/{postID}", func(ctx *credo.Context) error {
		id := ctx.Request().RouteParams()["id"]
		postID := ctx.Request().RouteParams()["postID"]
		return ctx.Response().Text(200, "id:"+id+" postID:"+postID)
	})

	tests := []struct {
		path string
		body string
	}{
		{"/users/42", "id:42"},
		{"/users/abc", "id:abc"},
		{"/users/42/posts/7", "id:42 postID:7"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.path, nil)
			app.ServeHTTP(w, r)

			if w.Code != 200 {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			body := w.Body.String()
			if body != tt.body {
				t.Errorf("body = %q, want %q", body, tt.body)
			}
		})
	}
}

func TestRouting_RegexParams(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/{id:[0-9]+}", func(ctx *credo.Context) error {
		id := ctx.Request().RouteParams()["id"]
		return ctx.Response().Text(200, "id:"+id)
	})

	tests := []struct {
		path string
		code int
		body string
	}{
		{"/users/42", 200, "id:42"},
		{"/users/abc", 404, ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.path, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
			if tt.body != "" {
				body := w.Body.String()
				if body != tt.body {
					t.Errorf("body = %q, want %q", body, tt.body)
				}
			}
		})
	}
}

func TestRouting_CatchAll(t *testing.T) {
	app := mustNew(t)
	app.GET("/files/{path...}", func(ctx *credo.Context) error {
		path := ctx.Request().RouteParams()["path"]
		return ctx.Response().Text(200, "path:"+path)
	})

	tests := []struct {
		path string
		body string
	}{
		{"/files/a", "path:a"},
		{"/files/a/b/c", "path:a/b/c"},
		{"/files/a/b/c.txt", "path:a/b/c.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.path, nil)
			app.ServeHTTP(w, r)

			if w.Code != 200 {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			body := w.Body.String()
			if body != tt.body {
				t.Errorf("body = %q, want %q", body, tt.body)
			}
		})
	}
}

func TestRouting_MethodNotAllowed(t *testing.T) {
	app := mustNew(t)
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/users", nil)
	app.ServeHTTP(w, r)

	if w.Code != 405 {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestRouting_MultipleMethods(t *testing.T) {
	app := mustNew(t)
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "GET")
	})
	app.POST("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "POST")
	})

	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, "/users", nil)
			app.ServeHTTP(w, r)

			if w.Code != 200 {
				t.Errorf("status = %d, want 200", w.Code)
			}
			body := w.Body.String()
			if body != method {
				t.Errorf("body = %q, want %q", body, method)
			}
		})
	}
}

func TestRouting_Walk(t *testing.T) {
	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error { return nil })
	app.POST("/users", func(ctx *credo.Context) error { return nil })

	var visited []string
	err := credo.Walk(app.Mux(), func(method, pattern string) error {
		visited = append(visited, method+" "+pattern)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk error: %v", err)
	}

	// GET / registers both GET and HEAD, plus POST /users = 3 routes
	if len(visited) < 2 {
		t.Fatalf("visited %d routes, want at least 2", len(visited))
	}
}

func TestRouting_Mount(t *testing.T) {
	// Create a sub-router (stdlib mux)
	sub := http.NewServeMux()
	sub.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("dashboard"))
	})

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "root")
	})
	app.Mount("/admin", sub)

	tests := []struct {
		method string
		path   string
		code   int
		body   string
	}{
		{"GET", "/", 200, "root"},
		{"GET", "/admin/dashboard", 200, "dashboard"},
		{"POST", "/admin/dashboard", 200, "dashboard"},
		// CONNECT and TRACE are deliberately excluded from Mount.
		{"TRACE", "/admin/dashboard", 405, ""},
		{"CONNECT", "/admin/dashboard", 405, ""},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, tt.path, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
			if tt.body != "" {
				body, _ := io.ReadAll(w.Body)
				if string(body) != tt.body {
					t.Errorf("body = %q, want %q", string(body), tt.body)
				}
			}
		})
	}
}

func TestRequest_RouteParam(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/{id}/posts/{postID}", func(ctx *credo.Context) error {
		req := ctx.Request()
		// RouteParam mirrors the RouteParams map for single values.
		if req.RouteParam("id") != req.RouteParams()["id"] {
			return ctx.Response().Text(200, "FAIL:mismatch")
		}
		// Missing names return "".
		if req.RouteParam("nope") != "" {
			return ctx.Response().Text(200, "FAIL:missing")
		}
		return ctx.Response().Text(200, req.RouteParam("id")+"/"+req.RouteParam("postID"))
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/users/42/posts/7", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "42/7" {
		t.Errorf("body = %q, want %q", got, "42/7")
	}
}

func TestRequest_PathValue(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/{id}", func(ctx *credo.Context) error {
		// Route params resolve through the stdlib-shaped accessor.
		if got := ctx.Request().PathValue("id"); got != "42" {
			return ctx.Response().Text(200, "FAIL:route:"+got)
		}
		// Unknown names fall back to the embedded request (empty here).
		if got := ctx.Request().PathValue("nope"); got != "" {
			return ctx.Response().Text(200, "FAIL:missing:"+got)
		}
		// Values set via the embedded stdlib API stay readable when no
		// route param shadows them.
		ctx.Request().Request.SetPathValue("extra", "stdlib")
		if got := ctx.Request().PathValue("extra"); got != "stdlib" {
			return ctx.Response().Text(200, "FAIL:fallback:"+got)
		}
		return ctx.Response().Text(200, "OK")
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/users/42", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "OK" {
		t.Errorf("body = %q, want OK", got)
	}
}

func TestRouting_Mount_CredoSubApp_ParamIsolation(t *testing.T) {
	// Child app: has its own route with a param.
	child := mustNew(t)
	child.GET("/items/{id}", func(ctx *credo.Context) error {
		params := ctx.Request().RouteParams()
		// _mount is an internal routing param — it must NOT leak to child handlers.
		if _, ok := params["_mount"]; ok {
			return ctx.Response().Text(200, "FAIL:_mount_leaked")
		}
		return ctx.Response().Text(200, "id="+params["id"])
	})
	child.GET("/", func(ctx *credo.Context) error {
		params := ctx.Request().RouteParams()
		if _, ok := params["_mount"]; ok {
			return ctx.Response().Text(200, "FAIL:_mount_leaked")
		}
		return ctx.Response().Text(200, "child-root")
	})

	parent := mustNew(t)
	parent.Mount("/api", child)

	tests := []struct {
		path string
		code int
		body string
	}{
		{"/api/items/42", 200, "id=42"},
		{"/api", 200, "child-root"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.path, nil)
			parent.ServeHTTP(w, r)

			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
			body := w.Body.String()
			if body != tt.body {
				t.Errorf("body = %q, want %q", body, tt.body)
			}
		})
	}
}

func TestRouting_Mount_HostScopeInteraction(t *testing.T) {
	app := mustNew(t)

	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-Global", "1")
			return next(ctx)
		}
	})

	host := app.Host("api.acme.test")
	host.GET("/ping", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "host-ping")
	})
	// Host-group middleware added after the route — compile-time collection
	// must apply it on the host mux too.
	host.Middleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-Host-MW", "1")
			return next(ctx)
		}
	})

	sub := http.NewServeMux()
	sub.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("mounted"))
	})
	app.Mount("/admin", sub)

	// Default-scope request reaches the mounted handler; mounted handlers
	// receive built-in and global middleware but never group middleware.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "http://other.test/admin", nil))
	if w.Code != 200 || w.Body.String() != "mounted" {
		t.Fatalf("default scope: status=%d body=%q, want 200 %q", w.Code, w.Body.String(), "mounted")
	}
	if w.Header().Get("X-Global") != "1" {
		t.Error("mounted handler must run inside the global middleware chain")
	}
	if w.Header().Get("X-Host-MW") != "" {
		t.Error("host-group middleware must not leak into mounted handlers")
	}

	// A request whose Host matches a registered host pattern dispatches
	// ONLY against that host's mux; Mount registers on the default mux,
	// so the mounted handler is not reachable from a matched host.
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "http://api.acme.test/admin", nil))
	if w.Code != 404 {
		t.Errorf("host scope: status=%d, want 404 (mounted handlers live on the default mux only)", w.Code)
	}

	// Host-mux routes work and carry the host-group middleware.
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "http://api.acme.test/ping", nil))
	if w.Code != 200 || w.Body.String() != "host-ping" {
		t.Fatalf("host route: status=%d body=%q, want 200 %q", w.Code, w.Body.String(), "host-ping")
	}
	if w.Header().Get("X-Host-MW") != "1" {
		t.Error("host-group middleware (added after the route) must apply to host routes")
	}
}

func TestRouting_StaticPriorityOverParam(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/new", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "static")
	})
	app.GET("/users/{id}", func(ctx *credo.Context) error {
		id := ctx.Request().RouteParams()["id"]
		return ctx.Response().Text(200, "param:"+id)
	})

	tests := []struct {
		path string
		body string
	}{
		{"/users/new", "static"},
		{"/users/42", "param:42"},
		{"/users/abc", "param:abc"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.path, nil)
			app.ServeHTTP(w, r)

			body := w.Body.String()
			if body != tt.body {
				t.Errorf("body = %q, want %q", body, tt.body)
			}
		})
	}
}

func TestRouting_DuplicateRoutePanics(t *testing.T) {
	app := mustNew(t)
	app.GET("/dup", func(ctx *credo.Context) error { return nil })

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for duplicate route registration")
		}
	}()

	// GET already registered GET + HEAD for /dup. This second GET should panic.
	app.POST("/dup", func(ctx *credo.Context) error { return nil }) // fine
	// Force a duplicate via same method
	app.GET("/dup", func(ctx *credo.Context) error { return nil }) // panic
}

func TestRouting_ConflictingParamNamesPanicIncludesRoutes(t *testing.T) {
	app := mustNew(t)
	h := func(ctx *credo.Context) error { return nil }
	app.GET("/v1/crm/customers/{id}", h)

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic for conflicting route parameter names")
		}

		got, ok := v.(string)
		if !ok {
			t.Fatalf("panic = %#v, want string", v)
		}

		for _, want := range []string{
			`conflicting path parameter "customer_id" with existing "id"`,
			`existing route "/v1/crm/customers/{id}"`,
			`new route "/v1/crm/customers/{customer_id}/timeline"`,
			"dynamic segments at the same path level must use the same parameter name",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("panic = %q, want substring %q", got, want)
			}
		}
	}()

	app.GET("/v1/crm/customers/{customer_id}/timeline", h)
}

func TestRouting_MethodNotAllowed_AllowHeader(t *testing.T) {
	app := mustNew(t)
	app.GET("/users", func(ctx *credo.Context) error { return nil })
	app.POST("/users", func(ctx *credo.Context) error { return nil })
	app.DELETE("/users", func(ctx *credo.Context) error { return nil })

	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/users", nil)
	app.ServeHTTP(w, r)

	if w.Code != 405 {
		t.Fatalf("status = %d, want 405", w.Code)
	}

	allow := w.Header().Get("Allow")
	if allow == "" {
		t.Fatal("Allow header missing on 405 response")
	}

	for _, method := range []string{"DELETE", "GET", "POST"} {
		if !strings.Contains(allow, method) {
			t.Errorf("Allow header %q missing method %s", allow, method)
		}
	}
}

func TestRouting_UnknownMethod_Returns404(t *testing.T) {
	app := mustNew(t)
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("PURGE", "/users", nil)
	app.ServeHTTP(w, r)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404 for unknown method", w.Code)
	}
}

func TestRouting_Walk_ErrorPropagation(t *testing.T) {
	app := mustNew(t)
	app.GET("/a", func(ctx *credo.Context) error { return nil })
	app.POST("/b", func(ctx *credo.Context) error { return nil })

	errStop := io.EOF // any error
	var count int
	err := credo.Walk(app.Mux(), func(method, pattern string) error {
		count++
		return errStop // stop immediately
	})

	if err != errStop {
		t.Errorf("Walk error = %v, want %v", err, errStop)
	}
	if count != 1 {
		t.Errorf("Walk visited %d routes before stopping, want 1", count)
	}
}

func TestRouting_WalkRoutes_IncludesHostRoutes(t *testing.T) {
	app := mustNew(t)
	app.GET("/health", func(ctx *credo.Context) error { return nil })
	app.Host("api.example.com").GET("/users", func(ctx *credo.Context) error { return nil })

	type seenRoute struct {
		method  string
		pattern string
		host    string
	}

	var seen []seenRoute
	err := credo.WalkRoutes(app.Mux(), func(ri credo.RouteInfo) error {
		seen = append(seen, seenRoute{method: ri.Method, pattern: ri.Pattern, host: ri.Host})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkRoutes error: %v", err)
	}

	contains := func(method, pattern, host string) bool {
		for _, route := range seen {
			if route.method == method && route.pattern == pattern && route.host == host {
				return true
			}
		}
		return false
	}

	if !contains("GET", "/health", "") {
		t.Fatalf("WalkRoutes missing default route GET /health")
	}
	if !contains("GET", "/users", "api.example.com") {
		t.Fatalf("WalkRoutes missing host route GET /users on api.example.com")
	}
}

func TestRouting_URLParam(t *testing.T) {
	app := mustNew(t)
	app.GET("/items/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/items/99", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// --- Trailing slash redirect tests ---

func TestTrailingSlashRedirect(t *testing.T) {
	app := mustNew(t)
	ok := func(ctx *credo.Context) error { return ctx.Response().Text(200, "ok") }
	app.GET("/users", ok)
	app.POST("/items/", ok)
	app.PUT("/items/", ok)
	app.DELETE("/items/", ok)
	app.PATCH("/items/", ok)

	tests := []struct {
		name     string
		method   string
		path     string
		wantCode int
		wantLoc  string
	}{
		{"GET strip slash", "GET", "/users/", 301, "/users"},
		{"HEAD strip slash", "HEAD", "/users/", 301, "/users"},
		{"POST add slash", "POST", "/items", 308, "/items/"},
		{"PUT add slash", "PUT", "/items", 308, "/items/"},
		{"DELETE add slash", "DELETE", "/items", 308, "/items/"},
		{"PATCH add slash", "PATCH", "/items", 308, "/items/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, tt.path, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if got := w.Header().Get("Location"); got != tt.wantLoc {
				t.Errorf("Location = %q, want %q", got, tt.wantLoc)
			}
		})
	}
}

func TestTrailingSlashRedirect_Params(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/42/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 301 {
		t.Errorf("status = %d, want 301", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/users/42" {
		t.Errorf("Location = %q, want %q", loc, "/users/42")
	}
}

func TestTrailingSlashRedirect_QueryString(t *testing.T) {
	app := mustNew(t)
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/?q=test&page=2", nil)
	app.ServeHTTP(w, r)

	if w.Code != 301 {
		t.Errorf("status = %d, want 301", w.Code)
	}
	want := "/users?q=test&page=2"
	if loc := w.Header().Get("Location"); loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
}

func TestTrailingSlashRedirect_RootPath(t *testing.T) {
	app := mustNew(t)
	app.GET("/about", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404 (root path should not redirect)", w.Code)
	}
}

func TestTrailingSlashRedirect_Disabled(t *testing.T) {
	app := mustNew(t, credo.WithRedirectTrailingSlash(false))
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404 (redirect disabled)", w.Code)
	}
}

func TestTrailingSlashRedirect_DisabledViaConfig(t *testing.T) {
	rc := newServerConfigRC(map[string]any{
		"redirect_trailing_slash": new(false),
	})
	app, err := credo.New(credo.WithRawConfig(rc))
	if err != nil {
		t.Fatal(err)
	}
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404 (redirect disabled via config)", w.Code)
	}
}

func TestTrailingSlashRedirect_405NotRedirected(t *testing.T) {
	app := mustNew(t)
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/users", nil)
	app.ServeHTTP(w, r)

	if w.Code != 405 {
		t.Errorf("status = %d, want 405 (method not allowed takes precedence)", w.Code)
	}
}

func TestTrailingSlashRedirect_NoAlternateMatch(t *testing.T) {
	app := mustNew(t)
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/notfound/", nil)
	app.ServeHTTP(w, r)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404 (no alternate match)", w.Code)
	}
}

func TestTrailingSlashRedirect_DirectMatch(t *testing.T) {
	app := mustNew(t)
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "without slash")
	})
	app.GET("/users/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "with slash")
	})

	// Both should match directly — no redirect.
	for _, path := range []string{"/users", "/users/"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		app.ServeHTTP(w, r)

		if w.Code != 200 {
			t.Errorf("GET %s: status = %d, want 200 (direct match)", path, w.Code)
		}
	}
}

// --- P1-1: HEAD override tests ---

func TestApp_HEAD_ExplicitOverridesAutoGenerated(t *testing.T) {
	app := mustNew(t)

	// GET auto-generates HEAD
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "get-body")
	})
	// Explicit HEAD should override auto-generated
	app.HEAD("/test", func(ctx *credo.Context) error {
		ctx.Response().Header().Set("X-Custom-Head", "yes")
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("HEAD", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Custom-Head"); got != "yes" {
		t.Errorf("X-Custom-Head = %q, want 'yes'", got)
	}
}

func TestApp_HEAD_ExplicitFirst_GETDoesNotOverwrite(t *testing.T) {
	app := mustNew(t)

	// Register explicit HEAD first
	app.HEAD("/test", func(ctx *credo.Context) error {
		ctx.Response().Header().Set("X-Custom-Head", "explicit")
		return ctx.Response().NoContent(200)
	})
	// GET tries to auto-register HEAD — should silently skip
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "get-body")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("HEAD", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Custom-Head"); got != "explicit" {
		t.Errorf("X-Custom-Head = %q, want 'explicit' (auto-generated should not overwrite)", got)
	}
}

func TestGroup_HEAD_ExplicitOverridesAutoGenerated(t *testing.T) {
	app := mustNew(t)
	api := app.Group("/api")

	api.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "get-body")
	})
	api.HEAD("/test", func(ctx *credo.Context) error {
		ctx.Response().Header().Set("X-Group-Head", "explicit")
		return ctx.Response().NoContent(200)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("HEAD", "/api/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Group-Head"); got != "explicit" {
		t.Errorf("X-Group-Head = %q, want 'explicit'", got)
	}
}

// --- P2-2: BuildURI regex quantifier test ---

func TestBuildURI_RegexQuantifier(t *testing.T) {
	app := mustNew(t)
	route := app.GET("/users/{id:[0-9]{2,4}}/posts/{pid}", func(ctx *credo.Context) error {
		return nil
	})

	uri, err := route.BuildURI("42", "7")
	if err != nil {
		t.Fatalf("BuildURI returned error: %v", err)
	}
	if uri != "/users/42/posts/7" {
		t.Errorf("BuildURI = %q, want %q", uri, "/users/42/posts/7")
	}
}

// --- P3-3: Coverage improvement tests ---

func TestApp_PATCH(t *testing.T) {
	app := mustNew(t)
	app.PATCH("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "patched:"+ctx.Request().RouteParams()["id"])
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("PATCH", "/users/42", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "patched:42" {
		t.Errorf("body = %q, want 'patched:42'", w.Body.String())
	}
}

func TestApp_OPTIONS(t *testing.T) {
	app := mustNew(t)
	app.OPTIONS("/users", func(ctx *credo.Context) error {
		ctx.Response().Header().Set("Allow", "GET, POST")
		return ctx.Response().NoContent(204)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("OPTIONS", "/users", nil)
	app.ServeHTTP(w, r)

	if w.Code != 204 {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow = %q, want 'GET, POST'", got)
	}
}

func TestApp_WrapStdMiddleware(t *testing.T) {
	app := mustNew(t)

	stdMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Std-MW", "applied")
			next.ServeHTTP(w, r)
		})
	}

	app.GlobalMiddleware(credo.WrapStdMiddleware(stdMW))
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Std-MW"); got != "applied" {
		t.Errorf("X-Std-MW = %q, want 'applied'", got)
	}
}

// writerSpy records whether Write was called on the wrapped writer.
type writerSpy struct {
	http.ResponseWriter
	writeCalled bool
}

func (ws *writerSpy) Write(b []byte) (int, error) {
	ws.writeCalled = true
	return ws.ResponseWriter.Write(b)
}

func (ws *writerSpy) WriteHeader(code int) {
	ws.ResponseWriter.WriteHeader(code)
}

func TestApp_WrapStdMiddleware_WrappedWriter(t *testing.T) {
	app := mustNew(t)

	var spy *writerSpy
	stdMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			spy = &writerSpy{ResponseWriter: w}
			next.ServeHTTP(spy, r)
		})
	}

	app.GlobalMiddleware(credo.WrapStdMiddleware(stdMW))
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "hello")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if spy == nil {
		t.Fatal("spy writer was not set")
	}
	if !spy.writeCalled {
		t.Error("expected writes to go through wrapped writer, but Write was not called on spy")
	}
}

func TestApp_WrapStdMiddleware_RestoresWriterForErrorPipeline(t *testing.T) {
	app := mustNew(t)

	// The wrapper is only valid while the stdlib middleware's ServeHTTP runs
	// (think gzip: Close flushes trailers on return). The error response is
	// rendered after the chain unwinds, so it must NOT go through the wrapper.
	var spy *writerSpy
	stdMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			spy = &writerSpy{ResponseWriter: w}
			next.ServeHTTP(spy, r)
		})
	}

	app.GlobalMiddleware(credo.WrapStdMiddleware(stdMW))
	app.GET("/fail", func(ctx *credo.Context) error {
		return credo.NewHTTPError(418, "teapot")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/fail", nil)
	app.ServeHTTP(w, r)

	if w.Code != 418 {
		t.Fatalf("status = %d, want 418", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("expected error response body to reach the original writer")
	}
	if spy == nil {
		t.Fatal("spy writer was not set")
	}
	if spy.writeCalled {
		t.Error("error response was written through the stdlib wrapper; want original writer after restore")
	}
}

func TestRoute_BuildURL(t *testing.T) {
	h := func(ctx *credo.Context) error { return nil }

	t.Run("default route equals BuildURI", func(t *testing.T) {
		app := mustNew(t)
		route := app.GET("/users/{id}", h)
		u, err := route.BuildURL("42")
		if err != nil {
			t.Fatalf("BuildURL returned error: %v", err)
		}
		if u != "/users/42" {
			t.Errorf("BuildURL = %q, want %q", u, "/users/42")
		}
	})

	t.Run("default route no params", func(t *testing.T) {
		app := mustNew(t)
		route := app.GET("/health", h)
		u, err := route.BuildURL()
		if err != nil {
			t.Fatalf("BuildURL returned error: %v", err)
		}
		if u != "/health" {
			t.Errorf("BuildURL = %q, want %q", u, "/health")
		}
	})

	t.Run("host-scoped static host", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("api.example.com").GET("/users/{id}", h)
		u, err := route.BuildURL("42")
		if err != nil {
			t.Fatalf("BuildURL returned error: %v", err)
		}
		if u != "api.example.com/users/42" {
			t.Errorf("BuildURL = %q, want %q", u, "api.example.com/users/42")
		}
	})

	t.Run("host-scoped with host param", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("{tenant}.myapp.com").GET("/users/{id}", h)
		u, err := route.BuildURL("acme", "42")
		if err != nil {
			t.Fatalf("BuildURL returned error: %v", err)
		}
		if u != "acme.myapp.com/users/42" {
			t.Errorf("BuildURL = %q, want %q", u, "acme.myapp.com/users/42")
		}
	})

	t.Run("host-scoped with regex host param", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("{org:[a-z]+}.api.com").GET("/items", h)
		u, err := route.BuildURL("acme")
		if err != nil {
			t.Fatalf("BuildURL returned error: %v", err)
		}
		if u != "acme.api.com/items" {
			t.Errorf("BuildURL = %q, want %q", u, "acme.api.com/items")
		}
	})

	t.Run("host-scoped multiple host params", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("{region}.{tenant}.myapp.com").GET("/data/{id}", h)
		u, err := route.BuildURL("us-east", "acme", "99")
		if err != nil {
			t.Fatalf("BuildURL returned error: %v", err)
		}
		if u != "us-east.acme.myapp.com/data/99" {
			t.Errorf("BuildURL = %q, want %q", u, "us-east.acme.myapp.com/data/99")
		}
	})

	t.Run("host-scoped no path params", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("{tenant}.myapp.com").GET("/health", h)
		u, err := route.BuildURL("acme")
		if err != nil {
			t.Fatalf("BuildURL returned error: %v", err)
		}
		if u != "acme.myapp.com/health" {
			t.Errorf("BuildURL = %q, want %q", u, "acme.myapp.com/health")
		}
	})
}

func TestRoute_BuildURLParameterErrors(t *testing.T) {
	h := func(ctx *credo.Context) error { return nil }

	t.Run("BuildURI missing param", func(t *testing.T) {
		app := mustNew(t)
		route := app.GET("/users/{id}", h)
		if _, err := route.BuildURI(); err == nil {
			t.Fatal("BuildURI should reject missing params")
		}
	})

	t.Run("BuildURI extra param", func(t *testing.T) {
		app := mustNew(t)
		route := app.GET("/health", h)
		if _, err := route.BuildURI("extra"); err == nil {
			t.Fatal("BuildURI should reject extra params")
		}
	})

	t.Run("BuildURL missing host param", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("{tenant}.myapp.com").GET("/users/{id}", h)
		if _, err := route.BuildURL("42"); err == nil {
			t.Fatal("BuildURL should reject missing host/path params")
		}
	})

	t.Run("BuildURL extra param", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("api.example.com").GET("/health", h)
		if _, err := route.BuildURL("extra"); err == nil {
			t.Fatal("BuildURL should reject extra params")
		}
	})

	t.Run("BuildURL wildcard host", func(t *testing.T) {
		app := mustNew(t)
		route := app.Host("*.myapp.com").GET("/health", h)
		if _, err := route.BuildURL(); err == nil {
			t.Fatal("BuildURL should reject wildcard hosts")
		}
	})
}

func TestGroup_AllMethods(t *testing.T) {
	app := mustNew(t)
	g := app.Group("/api")

	g.POST("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(201, "POST")
	})
	g.PUT("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "PUT")
	})
	g.DELETE("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(204)
	})
	g.PATCH("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "PATCH")
	})
	g.OPTIONS("/users", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(204)
	})

	tests := []struct {
		method string
		path   string
		code   int
	}{
		{"POST", "/api/users", 201},
		{"PUT", "/api/users/1", 200},
		{"DELETE", "/api/users/1", 204},
		{"PATCH", "/api/users/1", 200},
		{"OPTIONS", "/api/users", 204},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, tt.path, nil)
			app.ServeHTTP(w, r)
			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
		})
	}
}

func TestGroup_SetMeta_RemoveMeta(t *testing.T) {
	app := mustNew(t)
	g := app.Group("/api")
	g.SetMeta("auth", true)

	route := g.GET("/test", func(ctx *credo.Context) error {
		return nil
	})

	val, ok := route.LookupMeta("auth")
	if !ok || val != true {
		t.Errorf("expected 'auth' meta = true, got %v", val)
	}

	g.RemoveMeta("auth")

	_, ok = route.LookupMeta("auth")
	if ok {
		t.Error("expected 'auth' meta to be removed")
	}
}

// --- Host routing integration tests ---

func TestHost_ExactHost(t *testing.T) {
	app := mustNew(t)
	api := app.Host("api.example.com")
	api.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "api-users")
	})

	// Default mux fallback
	app.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "default-users")
	})

	// Correct host → host mux
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users", nil)
	r.Host = "api.example.com"
	app.ServeHTTP(w, r)
	if w.Body.String() != "api-users" {
		t.Errorf("body = %q, want 'api-users'", w.Body.String())
	}

	// Wrong host → default mux
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/users", nil)
	r.Host = "other.example.com"
	app.ServeHTTP(w, r)
	if w.Body.String() != "default-users" {
		t.Errorf("body = %q, want 'default-users'", w.Body.String())
	}
}

func TestHost_ParamExtraction(t *testing.T) {
	app := mustNew(t)
	tenant := app.Host("{tenant}.myapp.com")
	tenant.GET("/dashboard", func(ctx *credo.Context) error {
		name := ctx.Request().RouteParams()["tenant"]
		return ctx.Response().Text(200, "tenant:"+name)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/dashboard", nil)
	r.Host = "acme.myapp.com"
	app.ServeHTTP(w, r)
	if w.Body.String() != "tenant:acme" {
		t.Errorf("body = %q, want 'tenant:acme'", w.Body.String())
	}
}

func TestHost_RegexConstraint(t *testing.T) {
	app := mustNew(t)
	host := app.Host("{org:[a-z]+}.platform.io")
	host.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "org:"+ctx.Request().RouteParams()["org"])
	})
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "fallback")
	})

	// Valid regex → host mux
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "alpha.platform.io"
	app.ServeHTTP(w, r)
	if w.Body.String() != "org:alpha" {
		t.Errorf("body = %q, want 'org:alpha'", w.Body.String())
	}

	// Invalid regex → fallback
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/", nil)
	r.Host = "123.platform.io"
	app.ServeHTTP(w, r)
	if w.Body.String() != "fallback" {
		t.Errorf("body = %q, want 'fallback'", w.Body.String())
	}
}

func TestHost_Wildcard(t *testing.T) {
	app := mustNew(t)
	app.Host("*.acme.io").GET("/", func(ctx *credo.Context) error {
		if len(ctx.Request().RouteParams()) != 0 {
			return ctx.Response().Text(200, "wildcard-with-params")
		}
		return ctx.Response().Text(200, "wildcard")
	})
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "fallback")
	})

	tests := []struct {
		name string
		host string
		want string
	}{
		{"single label subdomain", "api.acme.io", "wildcard"},
		{"apex fallback", "acme.io", "fallback"},
		{"nested fallback", "a.b.acme.io", "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/", nil)
			r.Host = tt.host
			app.ServeHTTP(w, r)
			if w.Body.String() != tt.want {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.want)
			}
		})
	}
}

func TestHost_Priority(t *testing.T) {
	app := mustNew(t)

	// Register in reverse specificity order
	app.Host("{sub}.example.com").GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "param")
	})
	app.Host("{sub:[a-z]+}.example.com").GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "regex")
	})
	app.Host("api.example.com").GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "static")
	})

	// Static host should win
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "api.example.com"
	app.ServeHTTP(w, r)
	if w.Body.String() != "static" {
		t.Errorf("body = %q, want 'static'", w.Body.String())
	}

	// Regex should beat param
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/", nil)
	r.Host = "www.example.com"
	app.ServeHTTP(w, r)
	if w.Body.String() != "regex" {
		t.Errorf("body = %q, want 'regex'", w.Body.String())
	}
}

func TestHost_WildcardPriority(t *testing.T) {
	app := mustNew(t)

	app.Host("*.example.com").GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "wildcard")
	})
	app.Host("{sub:[a-z]+}.example.com").GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "regex")
	})
	app.Host("api.example.com").GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "static")
	})

	tests := []struct {
		name string
		host string
		want string
	}{
		{"static beats wildcard", "api.example.com", "static"},
		{"regex beats wildcard", "www.example.com", "regex"},
		{"wildcard fallback", "123.example.com", "wildcard"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/", nil)
			r.Host = tt.host
			app.ServeHTTP(w, r)
			if w.Body.String() != tt.want {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.want)
			}
		})
	}
}

func TestHost_Middleware(t *testing.T) {
	app := mustNew(t)
	host := app.Host("api.example.com")
	host.Middleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-Host-MW", "yes")
			return next(ctx)
		}
	})
	host.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})
	app.GET("/test", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "default")
	})

	// Host MW should run for matching host
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.Host = "api.example.com"
	app.ServeHTTP(w, r)
	if got := w.Header().Get("X-Host-MW"); got != "yes" {
		t.Errorf("X-Host-MW = %q, want 'yes'", got)
	}

	// Host MW should NOT run for default mux
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/test", nil)
	r.Host = "other.example.com"
	app.ServeHTTP(w, r)
	if got := w.Header().Get("X-Host-MW"); got != "" {
		t.Errorf("X-Host-MW = %q, want empty for default mux", got)
	}
}

func TestHost_NestedGroup(t *testing.T) {
	app := mustNew(t)
	host := app.Host("api.example.com")
	v1 := host.Group("/v1")
	v1.GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "v1-users")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/users", nil)
	r.Host = "api.example.com"
	app.ServeHTTP(w, r)
	if w.Body.String() != "v1-users" {
		t.Errorf("body = %q, want 'v1-users'", w.Body.String())
	}
}

func TestHost_DuplicatePanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate host pattern")
		}
	}()

	app := mustNew(t)
	app.Host("api.example.com")
	app.Host("api.example.com") // duplicate → panic
}

func TestHost_IdenticalSemanticsPanic(t *testing.T) {
	tests := []struct {
		name   string
		first  string
		second string
	}{
		{"param names differ", "{a}.acme.io", "{b}.acme.io"},
		{"wildcard and param", "*.acme.io", "{tenant}.acme.io"},
		{"regex param names differ", "{a:[a-z]+}.acme.io", "{b:[a-z]+}.acme.io"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for host patterns %q and %q", tt.first, tt.second)
				}
			}()

			app := mustNew(t)
			app.Host(tt.first)
			app.Host(tt.second)
		})
	}
}

func TestHost_PathParamNameCollisionPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for host/path parameter name collision")
		}
	}()

	app := mustNew(t)
	app.Host("{tenant}.example.com").GET("/users/{tenant}", func(ctx *credo.Context) error {
		return nil
	})
}

func TestHost_FrozenPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for host registration after compile")
		}
	}()

	app := mustNew(t)
	app.GET("/", func(ctx *credo.Context) error { return nil })

	// Force compile
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	app.ServeHTTP(w, r)

	// Now try to add host → should panic
	app.Host("api.example.com")
}

func TestHost_CaseInsensitive(t *testing.T) {
	app := mustNew(t)
	app.Host("api.example.com").GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "API.Example.COM"
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestHost_EmptyHostFallback(t *testing.T) {
	app := mustNew(t)
	app.Host("api.example.com").GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "host")
	})
	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "default")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = ""
	app.ServeHTTP(w, r)
	if w.Body.String() != "default" {
		t.Errorf("body = %q, want 'default'", w.Body.String())
	}
}

func TestHost_NamedRoutes(t *testing.T) {
	app := mustNew(t)
	app.Host("api.example.com").GET("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	}).Name("api.user")

	route := app.GetRoute("api.user")
	if route == nil {
		t.Fatal("expected named route 'api.user'")
	}
	if route.GetHost() != "api.example.com" {
		t.Errorf("GetHost() = %q, want 'api.example.com'", route.GetHost())
	}
}

func TestHost_TrailingSlashRedirect(t *testing.T) {
	app := mustNew(t)
	app.Host("api.example.com").GET("/users", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/", nil)
	r.Host = "api.example.com"
	app.ServeHTTP(w, r)
	if w.Code != 301 {
		t.Errorf("status = %d, want 301", w.Code)
	}
}

// --- ctx.Rewrite integration tests ---

func TestRewrite_Basic(t *testing.T) {
	app := mustNew(t)
	app.GET("/old", func(ctx *credo.Context) error {
		return ctx.Rewrite("/new")
	})
	app.GET("/new", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "new-handler")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "new-handler" {
		t.Errorf("body = %q, want 'new-handler'", w.Body.String())
	}
}

func TestRewrite_WithQuery(t *testing.T) {
	app := mustNew(t)
	app.GET("/old", func(ctx *credo.Context) error {
		return ctx.Rewrite("/new?key=val")
	})
	app.GET("/new", func(ctx *credo.Context) error {
		q := ctx.Request().QueryParam("key")
		return ctx.Response().Text(200, "key="+q)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "key=val" {
		t.Errorf("body = %q, want 'key=val'", w.Body.String())
	}
}

func TestRewrite_LoopLimit(t *testing.T) {
	app := mustNew(t)
	app.GET("/loop", func(ctx *credo.Context) error {
		return ctx.Rewrite("/loop")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/loop", nil)
	app.ServeHTTP(w, r)
	// Should get 500 from rewrite loop
	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestRewrite_PreservesOriginalPath(t *testing.T) {
	app := mustNew(t)
	var origPath string
	app.GET("/old", func(ctx *credo.Context) error {
		return ctx.Rewrite("/new")
	})
	app.GET("/new", func(ctx *credo.Context) error {
		origPath = ctx.OriginalPath()
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old", nil)
	app.ServeHTTP(w, r)
	if origPath != "/old" {
		t.Errorf("OriginalPath() = %q, want '/old'", origPath)
	}
}

func TestRewrite_NotFound(t *testing.T) {
	app := mustNew(t)
	app.GET("/old", func(ctx *credo.Context) error {
		return ctx.Rewrite("/nonexistent")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old", nil)
	app.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRewrite_ParamsCleared(t *testing.T) {
	app := mustNew(t)
	app.GET("/users/{id}", func(ctx *credo.Context) error {
		return ctx.Rewrite("/items")
	})
	app.GET("/items", func(ctx *credo.Context) error {
		// "id" param from first route should NOT leak
		id := ctx.Request().RouteParams()["id"]
		return ctx.Response().Text(200, "id="+id)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/users/42", nil)
	app.ServeHTTP(w, r)
	if w.Body.String() != "id=" {
		t.Errorf("body = %q, want 'id=' (param should be cleared)", w.Body.String())
	}
}

func TestRewrite_MiddlewarePerRound(t *testing.T) {
	app := mustNew(t)
	var mwCount int
	g := app.Group("")
	g.Middleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			mwCount++
			return next(ctx)
		}
	})
	g.GET("/a", func(ctx *credo.Context) error {
		return ctx.Rewrite("/b")
	})
	g.GET("/b", func(ctx *credo.Context) error {
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/a", nil)
	app.ServeHTTP(w, r)
	// MW runs for /a AND /b dispatch
	if mwCount != 2 {
		t.Errorf("middleware ran %d times, want 2", mwCount)
	}
}
