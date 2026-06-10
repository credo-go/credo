package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/credo-go/credo/store"
)

func TestSentinelErrors_ErrorsIs(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"ErrNotFound", store.ErrNotFound},
		{"ErrDuplicate", store.ErrDuplicate},
		{"ErrConflict", store.ErrConflict},
		{"ErrTimeout", store.ErrTimeout},
		{"ErrReadOnly", store.ErrReadOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.err) {
				t.Errorf("errors.Is(%v, %v) = false, want true", tt.err, tt.err)
			}
		})
	}
}

func TestSentinelErrors_HTTPStatus(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"ErrNotFound", store.ErrNotFound, 404},
		{"ErrDuplicate", store.ErrDuplicate, 409},
		{"ErrConflict", store.ErrConflict, 409},
		{"ErrTimeout", store.ErrTimeout, 504},
		{"ErrReadOnly", store.ErrReadOnly, 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var se interface{ HTTPStatus() int }
			if !errors.As(tt.err, &se) {
				t.Fatalf("errors.As did not match HTTPStatus interface for %v", tt.err)
			}
			if got := se.HTTPStatus(); got != tt.status {
				t.Errorf("HTTPStatus() = %d, want %d", got, tt.status)
			}
		})
	}
}

func TestSentinelErrors_WrappedPreservesChain(t *testing.T) {
	wrapped := fmt.Errorf("repo: user get: %w", store.ErrNotFound)

	if !errors.Is(wrapped, store.ErrNotFound) {
		t.Error("errors.Is on wrapped error should match ErrNotFound")
	}

	var se interface{ HTTPStatus() int }
	if !errors.As(wrapped, &se) {
		t.Fatal("errors.As should unwrap to find HTTPStatus on wrapped error")
	}
	if got := se.HTTPStatus(); got != 404 {
		t.Errorf("HTTPStatus() = %d, want 404", got)
	}
}

func TestSentinelErrors_ErrorMessage(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{store.ErrNotFound, "store: record not found"},
		{store.ErrDuplicate, "store: duplicate record"},
		{store.ErrConflict, "store: conflict"},
		{store.ErrTimeout, "store: timeout"},
		{store.ErrReadOnly, "store: read-only"},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}

func TestSentinelErrors_NotEqual(t *testing.T) {
	if errors.Is(store.ErrNotFound, store.ErrDuplicate) {
		t.Error("ErrNotFound should not match ErrDuplicate")
	}
	if errors.Is(store.ErrDuplicate, store.ErrConflict) {
		t.Error("ErrDuplicate should not match ErrConflict")
	}
}

func TestWrap_PreservesCauseAndStatus(t *testing.T) {
	original := errors.New("duplicate key value violates unique constraint users_email_key")
	wrapped := store.Wrap(store.ErrDuplicate, original)

	if !errors.Is(wrapped, store.ErrDuplicate) {
		t.Fatal("wrapped error should match ErrDuplicate")
	}
	if !errors.Is(wrapped, original) {
		t.Fatal("wrapped error should preserve original cause")
	}
	if wrapped.Error() != original.Error() {
		t.Fatalf("wrapped error message = %q, want %q", wrapped.Error(), original.Error())
	}

	var se interface{ HTTPStatus() int }
	if !errors.As(wrapped, &se) {
		t.Fatal("wrapped error should expose HTTPStatus")
	}
	if got := se.HTTPStatus(); got != 409 {
		t.Fatalf("HTTPStatus() = %d, want 409", got)
	}
}
