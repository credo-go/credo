package credo_test

// The runnable examples of the normative sections of the lifecycle, HTTP
// features and middleware specs that the root package carries
// (docs/specs/lifecycle.md, docs/specs/http-features.md,
// docs/specs/middleware.md).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/credo-go/credo"
)

// exDB is an internal component the start walk starts and the drain stops.
type exDB struct{}

func (*exDB) Start(context.Context) error    { fmt.Println("start db"); return nil }
func (*exDB) Shutdown(context.Context) error { fmt.Println("stop db"); return nil }

// exRepo depends on exDB, so it starts after it and stops before it.
type exRepo struct{ db *exDB }

func (*exRepo) Start(context.Context) error    { fmt.Println("start repo"); return nil }
func (*exRepo) Shutdown(context.Context) error { fmt.Println("stop repo"); return nil }

// exConsumer takes work in from outside the process: an ingress component.
type exConsumer struct{}

func (*exConsumer) Start(context.Context) error    { fmt.Println("start consumer"); return nil }
func (*exConsumer) Shutdown(context.Context) error { fmt.Println("stop consumer"); return nil }

// hook returns a lifecycle hook that prints its name.
func hook(name string) func(context.Context) error {
	return func(context.Context) error { fmt.Println(name); return nil }
}

// The start walk starts the internal tier in dependency order, then its start
// hooks, then the ingress tier and its hooks. The drain stops the ingress
// tier — its stop hooks LIFO, then its components — beside the HTTP drain,
// then the internal tier the same way, components in reverse dependency
// order.
func ExampleApp_Shutdown() {
	app := newExampleApp()
	app.Provide(func() *exDB { return &exDB{} })
	app.Provide(func(db *exDB) *exRepo { return &exRepo{db: db} })
	app.Manage(&exConsumer{}, credo.Ingress())
	app.OnStart(hook("internal OnStart 1"))
	app.OnStart(hook("internal OnStart 2"))
	app.OnStart(hook("ingress OnStart"), credo.Ingress())
	app.OnStop(hook("internal OnStop 1"))
	app.OnStop(hook("internal OnStop 2"))
	app.OnStop(hook("ingress OnStop"), credo.Ingress())

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		panic(err)
	}
	fmt.Println("state:", app.State())
	if err := app.Shutdown(ctx); err != nil {
		panic(err)
	}
	fmt.Println("state:", app.State())
	// Output:
	// start db
	// start repo
	// internal OnStart 1
	// internal OnStart 2
	// start consumer
	// ingress OnStart
	// state: running
	// ingress OnStop
	// stop consumer
	// internal OnStop 2
	// internal OnStop 1
	// stop repo
	// stop db
	// state: stopped
}

// A stopped App answers every request with the callback-free 503 envelope,
// and cannot be run again.
func ExampleApp_ServeHTTP_stopped() {
	app := newExampleApp()
	app.GET("/", exText("hello"))
	exServe(app, http.MethodGet, "/")

	if err := app.Shutdown(context.Background()); err != nil {
		panic(err)
	}
	exServe(app, http.MethodGet, "/")
	fmt.Println(app.Start(context.Background()))
	// Output:
	// GET / → 200 hello
	// GET / → 503 {"success":false,"error":{"code":"service_unavailable","message":"Service Unavailable"}}
	// credo: Start: app cannot be run after shutdown; create a new App
}

// An App with something to start refuses ServeHTTP until its start phase
// has run.
func ExampleApp_Start() {
	app := newExampleApp()
	app.Provide(func() *exDB { return &exDB{} })
	app.GET("/", exText("hello"))

	func() {
		defer func() { fmt.Println("panics before Start:", recover() != nil) }()
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	if err := app.Start(context.Background()); err != nil {
		panic(err)
	}
	defer func() { _ = app.Shutdown(context.Background()) }()
	exServe(app, http.MethodGet, "/")
	// Output:
	// panics before Start: true
	// start db
	// GET / → 200 hello
	// stop db
}

// The user chain runs PreRouting → routed → group → route → handler; the
// router's own 404 runs PreRouting alone.
func ExampleApp_Middleware() {
	tier := func(name string) credo.Middleware {
		return func(next credo.Handler) credo.Handler {
			return func(c *credo.Context) error {
				fmt.Println(name)
				return next(c)
			}
		}
	}
	app := newExampleApp()
	app.PreRouting(tier("PreRouting"))
	app.Middleware(tier("routed"))
	api := app.Group("/api")
	api.Middleware(tier("group"))
	api.GET("/users", exText("users")).Middleware(tier("route"))

	exServe(app, http.MethodGet, "/api/users")
	exServe(app, http.MethodGet, "/missing")
	// Output:
	// PreRouting
	// routed
	// group
	// route
	// GET /api/users → 200 users
	// PreRouting
	// GET /missing → 404 {"success":false,"error":{"code":"not_found","message":"Not Found"}}
}

type traceKey struct{}

// A stdlib middleware sees the Context's head through the request it is
// handed, and the context it passes on is the head of the inner chain.
func ExampleWrapStdMiddleware() {
	app := newExampleApp()
	app.PreRouting(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			c.Set(Tenant, &TenantInfo{Name: "acme"})
			return next(c)
		}
	})
	app.PreRouting(credo.WrapStdMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := r.Context().Value(Tenant).(*TenantInfo) // a value set before the middleware
			ctx := context.WithValue(r.Context(), traceKey{}, "span-"+t.Name)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}))
	app.GET("/", func(c *credo.Context) error {
		return c.Response().Text(http.StatusOK, c.Context().Value(traceKey{}).(string))
	})

	exServe(app, http.MethodGet, "/")
	// Output:
	// GET / → 200 span-acme
}

// exObserver prints what an observer reads from the final response.
type exObserver struct{}

func (exObserver) Observe(ex *credo.Exchange) {
	t, _ := ex.Get(Tenant)
	fmt.Println(ex.Method(), ex.Route().Pattern, ex.Status(), ex.ErrorCode(), ex.RequestID(), t.Name)
}

// Observers see the final response — an error envelope included — with the
// classified error code and the request's values.
func ExampleApp_AddObserver() {
	app := newExampleApp()
	app.UseRequestID()
	app.AddObserver(exObserver{})
	app.GET("/orders/{id}", func(c *credo.Context) error {
		c.Set(Tenant, &TenantInfo{Name: "acme"})
		return credo.NewHTTPError(http.StatusConflict)
	})

	req := httptest.NewRequest(http.MethodGet, "/orders/7", nil)
	req.Header.Set("X-Request-ID", "req-1")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	fmt.Println(rec.Code, rec.Header().Get("X-Request-ID"))
	// Output:
	// GET /orders/{id} 409 conflict req-1 acme
	// 409 req-1
}
