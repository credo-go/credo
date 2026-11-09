package credo_test

// The runnable examples of the context spec's normative sections
// (docs/specs/context.md).

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

// exPost sends a POST with a JSON body through app and prints the status and
// the body.
func exPost(app *credo.App, target, body string) {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	fmt.Println(rec.Code, rec.Body.String())
}

type TenantInfo struct{ Name string }

var Tenant = credo.NewKey[*TenantInfo]("tenant")

// A typed key carries a request value: Set takes only a value of the key's
// type, Get returns it typed, and stdlib code reads it with Value and the key.
func ExampleContext_Set() {
	app := newExampleApp()
	app.PreRouting(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			c.Set(Tenant, &TenantInfo{Name: "acme"}) // a value of another type does not compile
			return next(c)
		}
	})
	app.GET("/whoami", func(c *credo.Context) error {
		t, ok := c.Get(Tenant) // t is a *TenantInfo
		std := c.Request().Std().Context().Value(Tenant).(*TenantInfo)
		return c.Response().Text(http.StatusOK, fmt.Sprint(t.Name, " ", ok, " ", std.Name))
	})

	exServe(app, http.MethodGet, "/whoami")
	// Output:
	// GET /whoami → 200 acme true acme
}

// Deadline opens a scope: its deadline is the head of the inner chain and
// ends with it, while a value set inside the scope outlives it.
func Deadline(d time.Duration) credo.Middleware {
	return func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			ctx, cancel := context.WithTimeout(c.Context(), d)
			defer cancel()
			return c.Within(ctx, next)
		}
	}
}

func ExampleContext_Within() {
	app := newExampleApp()
	app.PreRouting(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			err := next(c)
			_, hasDeadline := c.Context().Deadline()
			t, _ := c.Get(Tenant)
			fmt.Println("after the scope: deadline", hasDeadline, "tenant", t.Name)
			return err
		}
	})
	app.Middleware(Deadline(time.Second))
	app.GET("/", func(c *credo.Context) error {
		_, hasDeadline := c.Context().Deadline()
		fmt.Println("inside the scope: deadline", hasDeadline)
		c.Set(Tenant, &TenantInfo{Name: "acme"})
		return c.Response().NoContent(http.StatusNoContent)
	})

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	// Output:
	// inside the scope: deadline true
	// after the scope: deadline false tenant acme
}

type CreateUserInput struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (c *CreateUserInput) Validate() error {
	return validation.ValidateStruct(c,
		validation.Field(&c.Name, validation.Required[string](), validation.Length(2, 100)),
		validation.Field(&c.Email, validation.Required[string](), validation.Email()),
	)
}

// BindBody decodes and validates in one call: a decode error and a
// validation error alike return the default error envelope.
func ExampleRequest_BindBody() {
	app := newExampleApp()
	app.POST("/users", func(c *credo.Context) error {
		var input CreateUserInput
		if err := c.Request().BindBody(&input); err != nil {
			return err // decode OR validation error → default error envelope
		}
		// input is GUARANTEED valid here
		return c.Response().JSON(http.StatusCreated, input)
	})

	exPost(app, "/users", `{"name":"Ada","email":"ada@example.com"}`)
	exPost(app, "/users", `{"name":"A","email":"ada@example.com"}`)
	exPost(app, "/users", `{"name":"Ada","email":"ada@example.com"}{"name":"Eve"}`)
	// Output:
	// 201 {"name":"Ada","email":"ada@example.com"}
	// 422 {"success":false,"error":{"code":"validation_failed","message":"Validation Failed","violations":[{"field":"name","code":"length","message":"must be between 2 and 100 characters","params":{"max":100,"min":2}}]}}
	// 400 {"success":false,"error":{"code":"bind_failed","message":"Malformed Request","violations":[{"field":"","code":"trailing_data","message":"request body must contain a single JSON value"}]}}
}

// A decode failure is a typed *credo.BindError, rendered as 400 bind_failed
// with one violation whose code is the reason.
func ExampleBindError() {
	type Person struct {
		Age int `json:"age"`
	}
	app := newExampleApp()
	app.POST("/people", func(c *credo.Context) error {
		var p Person
		return c.Request().BindBody(&p)
	})

	exPost(app, "/people", `{"name":"x","age":"12"}`)
	// Output:
	// 400 {"success":false,"error":{"code":"bind_failed","message":"Malformed Request","violations":[{"field":"age","code":"type_mismatch","message":"must be of type integer","params":{"expected":"integer","offset":18}}]}}
}

type ListUsersQuery struct {
	Page    int    `query:"page"`
	PerPage int    `query:"per_page"`
	Sort    string `query:"sort"`
}

func (q *ListUsersQuery) Validate() error {
	return validation.ValidateStruct(q,
		validation.Field(&q.Page, validation.Min(1)),
		validation.Field(&q.PerPage, validation.Between(1, 100)),
		validation.Field(&q.Sort, validation.In("name", "created_at")),
	)
}

// BindQuery reads the query string into a struct and validates it.
func ExampleRequest_BindQuery() {
	app := newExampleApp()
	app.GET("/users", func(c *credo.Context) error {
		var q ListUsersQuery
		if err := c.Request().BindQuery(&q); err != nil {
			return err
		}
		return c.Response().JSON(http.StatusOK, q)
	})

	exServe(app, http.MethodGet, "/users?page=2&per_page=20&sort=name")
	exServe(app, http.MethodGet, "/users?page=2&per_page=20&sort=age")
	// Output:
	// GET /users?page=2&per_page=20&sort=name → 200 {"Page":2,"PerPage":20,"Sort":"name"}
	// GET /users?page=2&per_page=20&sort=age → 422 {"success":false,"error":{"code":"validation_failed","message":"Validation Failed","violations":[{"field":"sort","code":"in","message":"must be one of the allowed values"}]}}
}

// Response.JSON encodes under the application's profile: nil slices and maps
// as [] and {}, map keys sorted, < > & literal, no trailing newline;
// WithJSONOptions overrides an axis for the whole application.
func ExampleWithJSONOptions() {
	payload := map[string]any{"tags": []string(nil), "b": "<b>", "a": map[string]int(nil)}
	handler := func(c *credo.Context) error { return c.Response().JSON(http.StatusOK, payload) }

	app := newExampleApp()
	app.GET("/", handler)
	exServe(app, http.MethodGet, "/")

	legacy, err := credo.New(
		credo.WithLogger(slog.New(slog.DiscardHandler)),
		credo.WithJSONOptions(jsonv2.FormatNilSliceAsNull(true), jsontext.EscapeForHTML(true)),
	)
	if err != nil {
		panic(err)
	}
	legacy.GET("/", handler)
	exServe(legacy, http.MethodGet, "/")
	// Output:
	// GET / → 200 {"a":{},"b":"<b>","tags":[]}
	// GET / → 200 {"a":{},"b":"\u003cb\u003e","tags":null}
}

// A body-forbidding status is status-only: the helper writes neither the body
// nor its Content-Type.
func ExampleResponse_JSON() {
	app := newExampleApp()
	app.GET("/", func(c *credo.Context) error {
		return c.Response().JSON(http.StatusNoContent, map[string]string{"ignored": "body"})
	})

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Printf("%d %q %q\n", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	// Output:
	// 204 "" ""
}

// Render routes a success body through the installed SuccessRenderer; the
// raw helpers never do.
func ExampleContext_Render() {
	app := newExampleApp()
	app.UseSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		return map[string]any{"success": true, "data": info.Data}
	})
	app.GET("/users", func(c *credo.Context) error {
		return c.Render(http.StatusOK, []string{"ada"})
	})
	app.GET("/webhook", func(c *credo.Context) error {
		return c.Response().JSON(http.StatusOK, map[string]bool{"ok": true})
	}).SetMeta(credo.MetaRawResponse, true)

	exServe(app, http.MethodGet, "/users")
	exServe(app, http.MethodGet, "/webhook")
	// Output:
	// GET /users → 200 {"data":["ada"],"success":true}
	// GET /webhook → 200 {"ok":true}
}
