package credo_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// Types for the module tests.
type (
	modUsers   struct{ billing *modBilling }
	modBilling struct{}
	modLate    struct{}
)

// funcModule is a Module whose phases are closures.
type funcModule struct {
	services func(*credo.App)
	handlers func(*credo.App)
}

func (m funcModule) Services(app *credo.App) {
	if m.services != nil {
		m.services(app)
	}
}

func (m funcModule) Handlers(app *credo.App) {
	if m.handlers != nil {
		m.handlers(app)
	}
}

func TestUseModules_ServicesOfEveryModuleBeforeAnyHandlers(t *testing.T) {
	app := mustNew(t)
	var order []string
	users := funcModule{
		services: func(app *credo.App) {
			order = append(order, "users.Services")
			app.Provide(func(b *modBilling) *modUsers { return &modUsers{billing: b} })
		},
		handlers: func(app *credo.App) {
			order = append(order, "users.Handlers")
			// Resolves a service of the module registered after this one.
			u := app.MustResolve[*modUsers]()
			app.GET("/users", func(c *credo.Context) error {
				if u.billing == nil {
					return c.Response().NoContent(http.StatusInternalServerError)
				}
				return c.Response().NoContent(http.StatusNoContent)
			})
		},
	}
	billing := funcModule{
		services: func(app *credo.App) {
			order = append(order, "billing.Services")
			app.Provide(func() *modBilling { return &modBilling{} })
		},
		handlers: func(*credo.App) { order = append(order, "billing.Handlers") },
	}

	app.UseModules(users, billing)

	want := []string{"users.Services", "billing.Services", "users.Handlers", "billing.Handlers"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("GET /users = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestUseModules_RegistrationFromHandlersAfterResolvePanics(t *testing.T) {
	app := mustNew(t)
	var site string
	mod := funcModule{
		services: func(app *credo.App) {
			app.Provide(func() *modBilling { return &modBilling{} })
		},
		handlers: func(app *credo.App) {
			site = lineAfter()
			_ = app.MustResolve[*modBilling]()
			app.Provide(func() *modLate { return &modLate{} })
		},
	}
	msg := mustPanicMessage(t, func() { app.UseModules(mod) })
	for _, want := range []string{
		"credo: App.Provide[*credo_test.modLate]: called after the container was finalized by ",
		"MustResolve[*credo_test.modBilling] at " + site,
		"; register before that call",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q\nwant it to contain %q", msg, want)
		}
	}
}

func TestUseModules_SecondCallPanics(t *testing.T) {
	app := mustNew(t)
	app.UseModules(funcModule{})
	msg := mustPanicMessage(t, func() { app.UseModules(funcModule{}) })
	want := "credo: App.UseModules called a second time; pass every module to one " +
		"UseModules call, which registers all their services before any of their " +
		"handlers resolve"
	if msg != want {
		t.Errorf("message = %q\nwant      %q", msg, want)
	}
}

func TestUseModules_NestedCallIsASecondCall(t *testing.T) {
	app := mustNew(t)
	outer := funcModule{
		services: func(app *credo.App) { app.UseModules(funcModule{}) },
	}
	msg := mustPanicMessage(t, func() { app.UseModules(outer) })
	if !strings.Contains(msg, "App.UseModules called a second time; pass every module to one UseModules call") {
		t.Errorf("message = %q, want the second-call panic", msg)
	}
}

func TestUseModules_AfterPreparationPanics(t *testing.T) {
	app := mustNew(t)
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	msg := mustPanicMessage(t, func() { app.UseModules(funcModule{}) })
	if want := "credo: App.UseModules called after app was compiled or shut down"; msg != want {
		t.Errorf("message = %q\nwant      %q", msg, want)
	}
}
