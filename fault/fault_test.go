package fault_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/credo-go/credo/fault"
)

type testFault struct {
	kind fault.Kind
}

func (e *testFault) Error() string         { return "test fault" }
func (e *testFault) FaultKind() fault.Kind { return e.kind }

func TestKindOf_TraversesWrappedAndJoinedErrors(t *testing.T) {
	target := &testFault{kind: fault.KindDeadlock}
	err := errors.Join(errors.New("outer"), fmt.Errorf("wrapped: %w", target))

	got, ok := fault.KindOf(err)
	if !ok {
		t.Fatal("KindOf did not find Provider in joined error")
	}
	if got != fault.KindDeadlock {
		t.Fatalf("KindOf = %q, want %q", got, fault.KindDeadlock)
	}
}

func TestKindOf_UnknownError(t *testing.T) {
	if got, ok := fault.KindOf(errors.New("plain")); ok || got != fault.KindUnknown {
		t.Fatalf("KindOf(plain) = (%q, %v), want (%q, false)", got, ok, fault.KindUnknown)
	}
}

func TestKindOf_SkipsTypedNilAndUnknownProviders(t *testing.T) {
	var typedNil *testFault
	err := errors.Join(
		typedNil,
		&testFault{kind: fault.KindUnknown},
		&testFault{kind: fault.KindSerialization},
	)

	got, ok := fault.KindOf(err)
	if !ok || got != fault.KindSerialization {
		t.Fatalf("KindOf = (%q, %v), want (%q, true)", got, ok, fault.KindSerialization)
	}
}

func TestProviderOf_ReturnsUnknownButSkipsTypedNil(t *testing.T) {
	var typedNil *testFault
	unknown := &testFault{kind: fault.KindUnknown}
	err := errors.Join(typedNil, unknown, &testFault{kind: fault.KindDeadlock})

	provider, ok := fault.ProviderOf(err)
	if !ok || provider != unknown {
		t.Fatalf("ProviderOf = (%v, %v), want first non-nil unknown provider", provider, ok)
	}
}
