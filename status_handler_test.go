package credo_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
)

func TestStatusHandler_404And405AreConsulted(t *testing.T) {
	app := mustNew(t)
	app.GET("/items", func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusOK, "items")
	})
	app.StatusHandler(http.StatusNotFound, func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusNotFound, "custom 404")
	})
	app.StatusHandler(http.StatusMethodNotAllowed, func(ctx *credo.Context) error {
		return ctx.Response().Text(http.StatusMethodNotAllowed, "custom 405")
	})

	cases := []struct {
		method, path string
		status       int
		body         string
	}{
		{http.MethodGet, "/missing", http.StatusNotFound, "custom 404"},
		{http.MethodDelete, "/items", http.StatusMethodNotAllowed, "custom 405"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || w.Body.String() != tc.body {
			t.Errorf("%s %s = %d %q, want %d %q", tc.method, tc.path, w.Code, w.Body.String(), tc.status, tc.body)
		}
	}
}

func TestStatusHandler_OtherCodesPanic(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusInternalServerError, http.StatusOK, 0} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			app := mustNew(t)
			want := fmt.Sprintf("credo: App.StatusHandler(%d): only 404 (http.StatusNotFound) and "+
				"405 (http.StatusMethodNotAllowed) are consulted", code)
			expectPanicContaining(t, want, func() {
				app.StatusHandler(code, func(*credo.Context) error { return nil })
			})
		})
	}
}
