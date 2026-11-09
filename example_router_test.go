package credo_test

// The runnable examples of the router spec's normative sections
// (docs/specs/router.md). Each one fails the build's tests when the contract
// it shows drifts.

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/credo-go/credo"
)

// newExampleApp returns an App that loads no configuration from disk and logs
// nowhere, so an example's output is its own.
func newExampleApp() *credo.App {
	app, err := credo.New(credo.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		panic(err)
	}
	return app
}

// exServe sends one request through app and prints the status, the Allow or
// Location header when set, and the body.
func exServe(app *credo.App, method, target string) {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	line := fmt.Sprintf("%s %s → %d", method, target, rec.Code)
	if allow := rec.Header().Get("Allow"); allow != "" {
		line += " Allow: " + allow
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		line += " Location: " + loc
	}
	if body, _ := io.ReadAll(rec.Body); len(body) > 0 {
		line += " " + string(body)
	}
	fmt.Println(line)
}

// exText answers with s.
func exText(s string) credo.Handler {
	return func(c *credo.Context) error { return c.Response().Text(http.StatusOK, s) }
}

var RequiresAuth = credo.NewKey[bool]("auth")

// A meta value set on a group reaches its routes, a route's own value
// overrides it, and middleware reads it with the key's type.
func ExampleNewKey() {
	app := newExampleApp()

	// Middleware reads meta declaratively; the value is a bool, no assertion.
	authMiddleware := func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			if auth, _ := c.Route().GetMeta(RequiresAuth); auth {
				return credo.NewHTTPError(http.StatusUnauthorized)
			}
			return next(c)
		}
	}

	// Group-level — inherited by all child routes.
	api := app.Group("/api").SetMeta(RequiresAuth, true)
	api.Middleware(authMiddleware)
	api.GET("/users", exText("users"))

	// Route-level — overrides the group.
	api.GET("/public", exText("public")).SetMeta(RequiresAuth, false)

	exServe(app, http.MethodGet, "/api/users")
	exServe(app, http.MethodGet, "/api/public")
	// Output:
	// GET /api/users → 401 {"success":false,"error":{"code":"unauthorized","message":"Unauthorized"}}
	// GET /api/public → 200 public
}

// A named route builds its URL from decoded parameter values, escaping each
// path value for one segment; a host-scoped route fills its host first.
func ExampleRoute_BuildURI() {
	app := newExampleApp()
	app.GET("/products/{id}", exText("product")).Name("product.show")
	app.Host("{tenant}.myapp.com").GET("/products/{id}", exText("product")).Name("tenant.product.show")

	route := app.GetRoute("product.show")
	uri, _ := route.BuildURI("42")
	fmt.Println(uri)
	uri, _ = route.BuildURI("a/b")
	fmt.Println(uri)
	url, _ := route.BuildURL("42") // a default route: the same as BuildURI
	fmt.Println(url)
	fmt.Printf("%q\n", route.GetHost())

	url, _ = app.GetRoute("tenant.product.show").BuildURL("acme", "42")
	fmt.Println(url)

	_, err := route.BuildURI("42", "extra")
	fmt.Println(err != nil)
	// Output:
	// /products/42
	// /products/a%2Fb
	// /products/42
	// ""
	// acme.myapp.com/products/42
	// true
}

// A custom 404 answers when no route matches; it is not an error handler.
func ExampleApp_StatusHandler() {
	app := newExampleApp()
	app.StatusHandler(http.StatusNotFound, func(c *credo.Context) error {
		return c.Response().JSON(http.StatusNotFound, map[string]string{"error": "not found"})
	})

	exServe(app, http.MethodGet, "/missing")
	// Output:
	// GET /missing → 404 {"error":"not found"}
}

// A host pattern selects its own routes before the path lookup, and its
// parameters share the route's namespace.
func ExampleApp_Host() {
	app := newExampleApp()
	app.GET("/", exText("landing"))

	api := app.Host("api.example.com")
	api.GET("/users", exText("users"))

	tenant := app.Host("{tenant}.example.com")
	tenant.GET("/dashboard", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, c.Request().RouteParam("tenant"))
	})

	exServe(app, http.MethodGet, "http://example.org/")
	exServe(app, http.MethodGet, "http://API.example.com:8080/users")
	exServe(app, http.MethodGet, "http://acme.example.com/dashboard")
	// Output:
	// GET http://example.org/ → 200 landing
	// GET http://API.example.com:8080/users → 200 users
	// GET http://acme.example.com/dashboard → 200 acme
}

// A mount hands its handler the path below the prefix in its wire spelling,
// and a parametric prefix's values as stdlib path values.
func ExampleApp_Mount() {
	app := newExampleApp()

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.EscapedPath())
	})
	app.Mount("/admin", adminMux)

	app.Mount("/t/{tenant}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.PathValue("tenant")+" "+r.URL.EscapedPath())
	}))

	exServe(app, http.MethodGet, "/admin/a%2Fb")
	exServe(app, http.MethodGet, "/admin")
	exServe(app, http.MethodGet, "/t/ac%2Fme/x")
	// Output:
	// GET /admin/a%2Fb → 200 /a%2Fb
	// GET /admin → 200 /
	// GET /t/ac%2Fme/x → 200 ac/me /x
}

// The first candidate that matches the path and serves the method answers; a
// 405 lists the methods of every candidate that matches the path.
func Example_matchingOrder() {
	app := newExampleApp()
	app.GET("/users/new", exText("newUserForm"))
	app.POST("/users/{id}", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, "updateUser "+c.Request().RouteParam("id"))
	})

	exServe(app, http.MethodGet, "/users/new")
	exServe(app, http.MethodPost, "/users/new")
	exServe(app, http.MethodDelete, "/users/new")
	// Output:
	// GET /users/new → 200 newUserForm
	// POST /users/new → 200 updateUser new
	// DELETE /users/new → 405 Allow: GET, HEAD, POST {"success":false,"error":{"code":"method_not_allowed","message":"Method Not Allowed"}}
}

// A path that matches only with its trailing slash toggled is redirected:
// 301 for GET and HEAD, 308 otherwise, the query preserved.
func Example_trailingSlashRedirect() {
	app := newExampleApp()
	app.GET("/users", exText("users"))
	app.POST("/users", exText("created"))

	exServe(app, http.MethodGet, "/users/?page=2")
	exServe(app, http.MethodPost, "/users/")
	// Output:
	// GET /users/?page=2 → 301 Location: /users?page=2
	// POST /users/ → 308 Location: /users
}

// Route names belong to the endpoint: routes that share a dynamic segment
// name it each their own way, and a parameter value is decoded once.
func Example_parameterNames() {
	app := newExampleApp()
	app.GET("/v1/crm/customers/{id}", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, "id="+c.Request().RouteParam("id"))
	})
	app.GET("/v1/crm/customers/{customer_id}/timeline", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, "customer_id="+c.Request().RouteParam("customer_id"))
	})

	exServe(app, http.MethodGet, "/v1/crm/customers/a%2Fb")
	exServe(app, http.MethodGet, "/v1/crm/customers/%252F/timeline")
	// Output:
	// GET /v1/crm/customers/a%2Fb → 200 id=a/b
	// GET /v1/crm/customers/%252F/timeline → 200 customer_id=%2F
}

// app.Routes returns every route and every mount in a deterministic order.
func ExampleApp_Routes() {
	app := newExampleApp()
	app.GET("/users/{id}", exText("user")).Name("user.show")
	app.Mount("/admin", http.NotFoundHandler())

	for _, ri := range app.Routes() {
		if ri.Kind == credo.RouteKindMount {
			fmt.Println("mount", ri.Pattern, ri.Methods)
			continue
		}
		fmt.Println(ri.Method, ri.Pattern, ri.Name, ri.AutoHead)
	}
	// Output:
	// mount /admin [DELETE GET HEAD OPTIONS PATCH POST PUT QUERY]
	// GET /users/{id} user.show false
	// HEAD /users/{id}  true
}
