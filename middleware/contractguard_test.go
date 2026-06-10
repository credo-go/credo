package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func contractOK(ctx *credo.Context) error {
	return ctx.Response().Text(http.StatusOK, "ok")
}

// contractGroup builds an app with a "/g" group guarded by ContractGuard.
// ContractGuard reads matched-route metadata, so it is registered at the group
// level (group/route middleware run after the route match; app-global
// middleware runs before it).
func contractGroup(t *testing.T, cfg ...middleware.ContractConfig) (*credo.App, *credo.Group) {
	t.Helper()
	app := mustNew(t)
	g := app.Group("/g")
	g.Middleware(middleware.ContractGuard(cfg...))
	return app, g
}

func TestContractGuard_Accept(t *testing.T) {
	tests := []struct {
		name        string
		accept      any
		contentType string
		want        int
	}{
		{"exact match", "application/json", "application/json", http.StatusOK},
		{"match ignores params", "application/json", "application/json; charset=utf-8", http.StatusOK},
		{"mismatch", "application/json", "text/plain", http.StatusUnsupportedMediaType},
		{"subtype wildcard", "image/*", "image/png", http.StatusOK},
		{"wildcard all", "*/*", "application/x-thing", http.StatusOK},
		{"empty content type passes", "application/json", "", http.StatusOK},
		{"slice match", []string{"application/json", "application/xml"}, "application/xml", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, g := contractGroup(t)
			g.POST("/x", contractOK).SetMeta(middleware.MetaAccept, tt.accept)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader("{}"))
			if tt.contentType != "" {
				r.Header.Set("Content-Type", tt.contentType)
			}
			app.ServeHTTP(w, r)

			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestContractGuard_MaxBody(t *testing.T) {
	t.Run("oversize content-length rejected eagerly", func(t *testing.T) {
		app, g := contractGroup(t)
		g.POST("/x", contractOK).SetMeta(middleware.MetaMaxBody, int64(10))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader(strings.Repeat("x", 50)))
		app.ServeHTTP(w, r)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", w.Code)
		}
	})

	t.Run("int value coerced", func(t *testing.T) {
		app, g := contractGroup(t)
		g.POST("/x", contractOK).SetMeta(middleware.MetaMaxBody, 10) // int, not int64

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader(strings.Repeat("x", 50)))
		app.ServeHTTP(w, r)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", w.Code)
		}
	})

	t.Run("within limit passes (body readable)", func(t *testing.T) {
		app, g := contractGroup(t)
		g.POST("/x", func(ctx *credo.Context) error {
			if _, err := io.ReadAll(ctx.Request().Body); err != nil {
				return err
			}
			return ctx.Response().Text(http.StatusOK, "ok")
		}).SetMeta(middleware.MetaMaxBody, int64(64))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader("small body"))
		app.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})

	t.Run("negative disables the per-route cap", func(t *testing.T) {
		app, g := contractGroup(t)
		g.POST("/x", contractOK).SetMeta(middleware.MetaMaxBody, int64(-1))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader(strings.Repeat("x", 5000)))
		app.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_RequireHeaders(t *testing.T) {
	app, g := contractGroup(t)
	g.GET("/x", contractOK).SetMeta(middleware.MetaRequireHeaders, []string{"X-Tenant-Id"})

	t.Run("missing header rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("present header passes", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/g/x", nil)
		r.Header.Set("X-Tenant-Id", "acme")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_RequireQuery(t *testing.T) {
	app, g := contractGroup(t)
	g.GET("/list", contractOK).SetMeta(middleware.MetaRequireQuery, "page")

	t.Run("missing query rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/list", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("present query passes", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/list?page=1", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_APIVersion(t *testing.T) {
	app, g := contractGroup(t)
	g.GET("/data", contractOK).SetMeta(middleware.MetaAPIVersion, []string{"1", "2"})
	g.GET("/v{version}/data", contractOK).SetMeta(middleware.MetaAPIVersion, []string{"1"})

	tests := []struct {
		name   string
		path   string
		header string
		want   int
	}{
		{"header allowed", "/g/data", "2", http.StatusOK},
		{"header not allowed", "/g/data", "3", http.StatusBadRequest},
		{"header missing", "/g/data", "", http.StatusBadRequest},
		{"path param allowed", "/g/v1/data", "", http.StatusOK},
		{"path param not allowed", "/g/v9/data", "", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.header != "" {
				r.Header.Set("X-API-Version", tt.header)
			}
			app.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestContractGuard_Scope(t *testing.T) {
	t.Run("no checker denies", func(t *testing.T) {
		app, g := contractGroup(t)
		g.GET("/x", contractOK).SetMeta(middleware.MetaScope, "admin")

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("checker allows", func(t *testing.T) {
		app, g := contractGroup(t, middleware.ContractConfig{
			ScopeChecker: func(_ *credo.Context, s string) bool { return s == "admin" },
		})
		g.GET("/x", contractOK).SetMeta(middleware.MetaScope, "admin")

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})

	t.Run("checker denies", func(t *testing.T) {
		app, g := contractGroup(t, middleware.ContractConfig{
			ScopeChecker: func(_ *credo.Context, _ string) bool { return false },
		})
		g.GET("/x", contractOK).SetMeta(middleware.MetaScope, []string{"a", "b"})

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})
}

func TestContractGuard_CustomCheck(t *testing.T) {
	app, g := contractGroup(t, middleware.ContractConfig{
		CustomChecks: []func(*credo.Context) error{
			func(ctx *credo.Context) error {
				if ctx.Request().Header.Get("X-Block") != "" {
					return credo.NewHTTPError(http.StatusTeapot, "blocked by custom check")
				}
				return nil
			},
		},
	})
	g.GET("/x", contractOK)

	t.Run("custom check rejects", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/g/x", nil)
		r.Header.Set("X-Block", "1")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Fatalf("status = %d, want 418", w.Code)
		}
	})

	t.Run("custom check passes", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_GroupMetaInheritance(t *testing.T) {
	app, g := contractGroup(t)
	g.SetMeta(middleware.MetaAccept, "application/json") // group-wide contract
	g.POST("/users", contractOK)

	t.Run("inherited contract enforced", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/users", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "text/plain")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", w.Code)
		}
	})

	t.Run("inherited contract satisfied", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/users", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_NoMetaPasses(t *testing.T) {
	app, g := contractGroup(t)
	g.GET("/x", contractOK)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestContractGuard_Skipper(t *testing.T) {
	app, g := contractGroup(t, middleware.ContractConfig{
		Skipper: func(_ *credo.Context) bool { return true },
	})
	g.POST("/x", contractOK).SetMeta(middleware.MetaAccept, "application/json")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "text/plain") // would be 415 if not skipped
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}
