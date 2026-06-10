package middleware

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/credo-go/credo"
)

func TestDefaultTimeoutErrorHandler(t *testing.T) {
	t.Run("deadline exceeded", func(t *testing.T) {
		err := defaultTimeoutErrorHandler(nil, context.DeadlineExceeded)
		he, ok := errors.AsType[*credo.HTTPError](err)
		if !ok {
			t.Fatalf("error type = %T, want *credo.HTTPError", err)
		}
		if he.Code != http.StatusServiceUnavailable {
			t.Fatalf("status code = %d, want %d", he.Code, http.StatusServiceUnavailable)
		}
	})

	t.Run("passthrough", func(t *testing.T) {
		source := errors.New("boom")
		if got := defaultTimeoutErrorHandler(nil, source); !errors.Is(got, source) {
			t.Fatalf("expected passthrough error, got %v", got)
		}
	})
}
