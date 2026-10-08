package credo

import (
	"context"
	"errors"
	"fmt"

	"github.com/credo-go/credo/internal/di"
	internalhealth "github.com/credo-go/credo/internal/health"
	"github.com/credo-go/credo/internal/kernel"
)

func init() {
	kernel.RegisterStore = func(app any, s kernel.Store) {
		app.(*App).registerStore(s)
	}
}

// storeSlot is one store registration and, once the start phase has pinged
// the store, its readiness probe.
type storeSlot struct {
	kernel.Store
	// probe is written by the start walk and read once it has completed.
	probe *internalhealth.Probe
}

// registerStore adds a store registration: a start step on the binding of
// s.Type that pings the built value, and a readiness entry built from it.
// Registration does no I/O; a type without a binding fails Finalize.
func (app *App) registerStore(s kernel.Store) {
	call := "store.Register[" + s.Type.String() + "]"
	app.checkFrozen(call)
	for _, slot := range app.stores {
		switch {
		case slot.Type == s.Type:
			panic(fmt.Sprintf("credo: %s: %s is already registered as a store", call, s.Type))
		case slot.Name == s.Name:
			panic(fmt.Sprintf("credo: %s: a store named %q is already registered; give it its own name "+
				"with store.WithName", call, s.Name))
		}
	}
	slot := &storeSlot{Store: s}
	err := app.container.AddStartStep(s.Type, di.StartStep{
		Call: call,
		Run: func(ctx context.Context, value any) error {
			if err := s.Ping(ctx, value); err != nil {
				return err
			}
			slot.probe = s.Probe(value)
			return nil
		},
	})
	if err != nil {
		if m, ok := errors.AsType[*di.MisuseError](err); ok {
			panic("credo: " + m.Call + ": " + m.Reason)
		}
		panic(err)
	}
	app.stores = append(app.stores, slot)
}

// storeChecks returns the readiness checks of the stores the start phase
// pinged; nil before the App has started.
func (app *App) storeChecks() []internalhealth.StoreCheck {
	if !app.lifecycle.started.Load() || len(app.stores) == 0 {
		return nil
	}
	checks := make([]internalhealth.StoreCheck, 0, len(app.stores))
	for _, slot := range app.stores {
		if slot.probe != nil {
			checks = append(checks, internalhealth.StoreCheck{Name: slot.Name, Probe: slot.probe})
		}
	}
	return checks
}
