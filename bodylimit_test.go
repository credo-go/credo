package credo_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

type maxBodyPayload struct {
	Name string `json:"name"`
}

func TestMaxBodyBytes(t *testing.T) {
	newApp := func(t *testing.T, limit int64) *credo.App {
		t.Helper()
		app, err := credo.New(credo.WithMaxBodyBytes(limit))
		if err != nil {
			t.Fatal(err)
		}
		app.POST("/u", func(ctx *credo.Context) error {
			var p maxBodyPayload
			if err := ctx.Request().BindBody(&p); err != nil {
				return err
			}
			return ctx.Response().JSON(http.StatusOK, p)
		})
		return app
	}

	post := func(app *credo.App, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/u", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(w, r)
		return w
	}

	large := `{"name":"` + strings.Repeat("a", 256) + `"}`

	t.Run("under the limit succeeds", func(t *testing.T) {
		w := post(newApp(t, 1024), `{"name":"ok"}`)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
	})

	t.Run("over the limit returns 413", func(t *testing.T) {
		w := post(newApp(t, 16), large)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413 (body: %s)", w.Code, w.Body.String())
		}
	})

	t.Run("negative disables the limit", func(t *testing.T) {
		// The same body that 413s under a 16-byte cap must pass when disabled.
		if w := post(newApp(t, 16), large); w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("precondition: capped app should 413, got %d", w.Code)
		}
		if w := post(newApp(t, -1), large); w.Code != http.StatusOK {
			t.Errorf("disabled limit: status = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
	})
}
