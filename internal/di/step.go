package di

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

// StartStep is work the start walk runs on a binding's built value before
// the value's Start: a store registration pings the store. A step that fails
// fails the start, and the value it was given is still torn down by the
// rollback, since the step opened nothing the value had not.
type StartStep struct {
	// Call is the registering call, as a Finalize finding names it, for
	// example "store.Register[*app.DB]".
	Call string
	// Run receives the start context and the binding's built value.
	Run func(ctx context.Context, value any) error
}

// pendingStep is a step waiting for Seal to find the unit of its type.
type pendingStep struct {
	t    reflect.Type
	step StartStep
}

// AddStartStep adds a step for the binding of t: a direct binding, or an
// interface Alias names. The binding may be made later in the registration
// window; Finalize reports a step whose type has no binding then. Every
// rejection is a [*MisuseError].
func (c *Container) AddStartStep(t reflect.Type, step StartStep) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.closedLocked("", t); err != nil {
		if m, ok := errors.AsType[*MisuseError](err); ok {
			m.Call = step.Call
		}
		return err
	}
	if step.Run == nil {
		return &MisuseError{Call: step.Call, Reason: "the start step must not be nil"}
	}
	c.steps = append(c.steps, pendingStep{t: t, step: step})
	return nil
}

// stepUnitLocked returns the unit a step for t runs on: the binding of t, or
// the binding an alias of t names. c.mu must be held.
func (c *Container) stepUnitLocked(t reflect.Type) *Unit {
	if u := c.unitOf[t]; u != nil {
		return u
	}
	if target, ok := c.aliases[t]; ok {
		return c.unitOf[target]
	}
	return nil
}

// stepFindingsLocked reports each step whose type has no binding. c.mu must
// be held.
func (c *Container) stepFindingsLocked() []error {
	var errs []error
	for _, p := range c.steps {
		if c.stepUnitLocked(p.t) == nil {
			errs = append(errs, fmt.Errorf("di: %s: %s has no binding; bind it with Provide or ProvideValue "+
				"before Finalize", p.step.Call, p.t))
		}
	}
	return errs
}

// attachStepsLocked hands each step to the unit it runs on. Seal calls it
// once, after validation. c.mu must be held.
func (c *Container) attachStepsLocked() {
	for _, p := range c.steps {
		if u := c.stepUnitLocked(p.t); u != nil {
			u.steps = append(u.steps, p.step)
		}
	}
}

// Steps returns the start steps the walk runs on the unit's value, in
// registration order. It is fixed once Finalize has run.
func (u *Unit) Steps() []StartStep { return u.steps }
