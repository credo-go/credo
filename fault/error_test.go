package fault_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/credo-go/credo/fault"
)

func TestNew_KindIsTheDefaultCode(t *testing.T) {
	e := fault.New(fault.KindAlreadyExists)
	if e.Kind != fault.KindAlreadyExists || e.Code != "already_exists" || e.Details != nil || e.Internal != nil {
		t.Fatalf("New(KindAlreadyExists) = %+v", e)
	}
	e = fault.New(fault.KindConflict, "tenant_domain_taken")
	if e.Code != "tenant_domain_taken" || e.FaultKind() != fault.KindConflict {
		t.Fatalf("New(KindConflict, code) = %+v", e)
	}
}

func TestNew_Panics(t *testing.T) {
	cases := []struct {
		name string
		call func()
		want string
	}{
		{"unknown kind", func() { fault.New(fault.KindUnknown) }, `fault: New: kind "" is not a declared kind`},
		{"undeclared kind", func() { fault.New(fault.Kind("future_kind")) }, `fault: New: kind "future_kind" is not a declared kind`},
		{"invalid code", func() { fault.New(fault.KindConflict, "Domain Taken") }, `fault: New: "Domain Taken" is not a valid machine code (want ^[a-z0-9]+(_[a-z0-9]+)*$)`},
		{"empty code", func() { fault.New(fault.KindConflict, "") }, `fault: New: "" is not a valid machine code (want ^[a-z0-9]+(_[a-z0-9]+)*$)`},
		{"second code", func() { fault.New(fault.KindConflict, "a", "b") }, "fault: New: at most one code argument is allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if got := recover(); got != tc.want {
					t.Fatalf("panic = %v, want %q", got, tc.want)
				}
			}()
			tc.call()
		})
	}
}

func TestError_WithReturnsCopies(t *testing.T) {
	sentinel := fault.New(fault.KindAlreadyExists, "tenant_domain_taken")
	cause := errors.New("duplicate key")
	derived := sentinel.WithDetails(map[string]any{"domain": "a.example"}).WithInternal(cause)

	if sentinel.Details != nil || sentinel.Internal != nil {
		t.Fatalf("sentinel mutated: %+v", sentinel)
	}
	if derived.Details == nil || !errors.Is(derived, cause) {
		t.Fatalf("derived = %+v", derived)
	}
	if !errors.Is(derived, sentinel) || !errors.Is(fmt.Errorf("wrap: %w", derived), sentinel) {
		t.Fatal("a derived copy does not match its sentinel")
	}
	if errors.Is(derived, fault.New(fault.KindConflict, "tenant_domain_taken")) {
		t.Fatal("matched a fault of another kind")
	}
	if errors.Is(derived, fault.New(fault.KindAlreadyExists)) {
		t.Fatal("matched a fault of another code")
	}
	if got := derived.Error(); got != "fault: kind=already_exists, code=tenant_domain_taken, internal=duplicate key" {
		t.Fatalf("Error() = %q", got)
	}
	if kind, ok := fault.KindOf(derived); !ok || kind != fault.KindAlreadyExists {
		t.Fatalf("KindOf = %q, %t", kind, ok)
	}
}
