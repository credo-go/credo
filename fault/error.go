package fault

import (
	"fmt"

	"github.com/credo-go/credo/internal/machinecode"
)

// Error is the error a domain layer returns without naming a transport
// status: a [Kind], a client-visible machine code, optional client-safe
// details and an internal cause.
//
//	var ErrDomainTaken = fault.New(fault.KindAlreadyExists, "tenant_domain_taken")
//
//	return nil, ErrDomainTaken.WithDetails(map[string]any{"domain": in.Domain}).WithInternal(err)
//
// A transport maps the kind to its status, as for every [Provider]; Credo's
// HTTP pipeline renders Code as the code, Details as the details and the
// message it resolves from the code, and logs Internal, which it never
// renders. An Error wins over a validation or bind error it wraps, so a cause
// given as Internal never reaches the client.
//
// A value built without [New] whose Kind is not declared or whose Code is not
// a valid machine code fails closed: the HTTP pipeline renders a generic 500.
type Error struct {
	// Kind is the semantic category; a transport maps it to its status.
	Kind Kind

	// Code is the client-visible machine code, ^[a-z0-9]+(_[a-z0-9]+)*$.
	// [New] sets the kind as the code when none is given.
	Code string

	// Details carries optional structured, client-safe detail. Never place
	// secrets or internal state here.
	Details any

	// Internal is the underlying cause. It is logged and never rendered.
	Internal error
}

// New returns an Error of kind with code as its machine code, or the kind
// itself as the code when none is given, as NewHTTPError derives a code from
// its status.
//
// Misuse panics, as a developer invariant violation: a kind that is not one
// of the declared kinds, [KindUnknown] included, more than one code, or a code
// that fails the machine-code grammar ^[a-z0-9]+(_[a-z0-9]+)*$. During a
// request, recovery turns the panic into a generic 500.
func New(kind Kind, code ...string) *Error {
	if !kind.declared() {
		panic(fmt.Sprintf("fault: New: kind %q is not a declared kind", string(kind)))
	}
	switch len(code) {
	case 0:
		return &Error{Kind: kind, Code: string(kind)}
	case 1:
		if !machinecode.Valid(code[0]) {
			panic(fmt.Sprintf("fault: New: %q is not a valid machine code (want ^[a-z0-9]+(_[a-z0-9]+)*$)", code[0]))
		}
		return &Error{Kind: kind, Code: code[0]}
	default:
		panic("fault: New: at most one code argument is allowed")
	}
}

// Error implements the error interface. The diagnostic order is kind, code,
// internal; empty segments are omitted.
func (e *Error) Error() string {
	s := "fault: kind=" + string(e.Kind)
	if e.Code != "" {
		s += ", code=" + e.Code
	}
	if e.Internal != nil {
		s += fmt.Sprintf(", internal=%v", e.Internal)
	}
	return s
}

// FaultKind implements [Provider].
func (e *Error) FaultKind() Kind { return e.Kind }

// Unwrap returns the internal cause.
func (e *Error) Unwrap() error { return e.Internal }

// Is reports whether target is an Error of the same kind and code, so a copy
// derived with [Error.WithDetails] or [Error.WithInternal] still matches its
// sentinel.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error) //nolint:errorlint // Is compares this node only.
	return ok && t != nil && e.Kind == t.Kind && e.Code == t.Code
}

// WithDetails returns a copy of e with client-safe details; e is unchanged.
func (e *Error) WithDetails(v any) *Error {
	c := *e
	c.Details = v
	return &c
}

// WithInternal returns a copy of e with the internal cause set; e is
// unchanged.
func (e *Error) WithInternal(err error) *Error {
	c := *e
	c.Internal = err
	return &c
}
