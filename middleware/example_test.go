package middleware_test

// The runnable examples of the middleware spec's normative sections
// (docs/specs/middleware.md).

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func exampleApp() *credo.App {
	app, err := credo.New(credo.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		panic(err)
	}
	return app
}

func exampleServe(app *credo.App, req *http.Request) {
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	fmt.Println(req.Method, req.URL.RequestURI(), "→", rec.Code, rec.Body.String())
}

// Rewrite runs before routing, so the router matches the rewritten path; a
// captured value keeps its wire spelling.
func ExampleRewrite() {
	app := exampleApp()
	app.PreRouting(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
		{From: "/v1/{path...}", To: "/api/v1/{path}"},
	}}))
	app.GET("/api/v1/{path...}", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, c.Request().URL.EscapedPath()+" from "+c.OriginalPath())
	})

	exampleServe(app, httptest.NewRequest(http.MethodGet, "/v1/files/a%2Fb", nil))
	// Output:
	// GET /api/v1/files/a%2Fb → 200 /api/v1/files/a%2Fb from /v1/files/a%2Fb
}

var RequireAuth = credo.NewKey[bool]("auth")

// Middleware reads route meta to decide per route; a route's own value
// overrides its group's.
func Example_metaDrivenMiddleware() {
	validateToken := func(h string) bool { return h == "Bearer good" }
	Auth := func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			if required, _ := c.Route().GetMeta(RequireAuth); required {
				if !validateToken(c.Request().Header.Get("Authorization")) {
					return credo.NewHTTPError(http.StatusUnauthorized)
				}
			}
			return next(c)
		}
	}
	ok := func(c *credo.Context) error { return c.Response().Text(http.StatusOK, "ok") }

	app := exampleApp()
	api := app.Group("/api")
	api.SetMeta(RequireAuth, true).Middleware(Auth)

	api.GET("/users", ok)                              // authenticated
	api.GET("/health", ok).SetMeta(RequireAuth, false) // not authenticated

	exampleServe(app, httptest.NewRequest(http.MethodGet, "/api/users", nil))
	exampleServe(app, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	// Output:
	// GET /api/users → 401 {"success":false,"error":{"code":"unauthorized","message":"Unauthorized"}}
	// GET /api/health → 200 ok
}

// ContractGuard on the routed tier enforces the contracts a route declares
// in its meta; each rejection carries its value in details.
func ExampleContractGuard() {
	app := exampleApp()
	app.Middleware(middleware.ContractGuard()) // reads every route's meta; never runs for a 404 or a 405
	app.POST("/upload", func(c *credo.Context) error {
		return c.Response().NoContent(http.StatusNoContent)
	}).SetMeta(middleware.MetaAccept, []string{"application/json"}).
		SetMeta(middleware.MetaRequireHeaders, []string{"X-Tenant"})

	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("<a/>"))
	req.Header.Set("Content-Type", "application/xml")
	exampleServe(app, req)

	req = httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	exampleServe(app, req)

	req = httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", "acme")
	exampleServe(app, req)
	// Output:
	// POST /upload → 415 {"success":false,"error":{"code":"content_type_not_accepted","message":"Content-Type is not accepted.","details":{"content_type":"application/xml"}}}
	// POST /upload → 400 {"success":false,"error":{"code":"missing_required_header","message":"A required header is missing.","details":{"header":"X-Tenant"}}}
	// POST /upload → 204
}

// Each constructor takes an optional config: none selects the defaults, one
// customizes.
func ExampleCORS() {
	app := exampleApp()
	app.PreRouting(middleware.CORS(middleware.CORSConfig{
		AllowOrigins: []string{"https://example.com"},
		AllowMethods: []string{"GET", "POST"},
	}))
	app.GET("/data", func(c *credo.Context) error { return c.Response().Text(http.StatusOK, "data") })

	req := httptest.NewRequest(http.MethodOptions, "/data", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	fmt.Println(rec.Code, rec.Header().Get("Access-Control-Allow-Origin"), rec.Header().Get("Access-Control-Allow-Methods"))
	// Output:
	// 204 https://example.com GET,POST
}
