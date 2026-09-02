package faultstatus

import (
	"errors"
	"fmt"
	"testing"
)

type statusErr struct{ code int }

func (e statusErr) Error() string   { return "status" }
func (e statusErr) HTTPStatus() int { return e.code }

func TestProviderOf(t *testing.T) {
	direct := statusErr{code: 418}
	if p, ok := ProviderOf(direct); !ok || p.HTTPStatus() != 418 {
		t.Fatalf("ProviderOf(direct) = (%v, %v), want (418, true)", p, ok)
	}
	wrapped := fmt.Errorf("outer: %w", statusErr{code: 409})
	if p, ok := ProviderOf(wrapped); !ok || p.HTTPStatus() != 409 {
		t.Fatalf("ProviderOf(wrapped) = (%v, %v), want (409, true)", p, ok)
	}
	if _, ok := ProviderOf(errors.New("plain")); ok {
		t.Fatal("ProviderOf(plain) reported a provider")
	}
	if _, ok := ProviderOf(nil); ok {
		t.Fatal("ProviderOf(nil) reported a provider")
	}
}
