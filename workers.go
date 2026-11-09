package credo

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/credo-go/credo/internal/di"
	internalworker "github.com/credo-go/credo/internal/worker"
)

// Workers is the App's worker registry, which [App.Workers] returns. It is a
// registry, not a lifecycle: each registration adds the worker to the App as
// a component named "worker:<name>", which the App starts and stops in its
// tier like every other component. Workers has no Start, no Shutdown and no
// DI binding.
type Workers struct {
	app    *App
	engine *internalworker.Engine

	mu      sync.Mutex
	workers []registeredWorker // registration order
	// provided records the type of every provided worker and the name it was
	// registered under: one type under two names would hand both loops one
	// instance.
	provided map[reflect.Type]string
}

// registeredWorker is one registration: its component and the tier the
// registry resolved, which the engine does not know.
type registeredWorker struct {
	c    *internalworker.Component
	tier Tier
}

// Workers returns the App's one worker registry, created on first use; every
// call returns the same value. It registers nothing, so an App that calls it
// and registers no worker has no start work. It is not bound in DI: a module
// that registers workers takes the *App it already receives.
//
// The registry logs through the App's logger with module=worker, and each
// worker's records carry component=worker:<name>.
func (app *App) Workers() *Workers {
	app.workers.once.Do(func() {
		app.workers.registry = &Workers{
			app:      app,
			engine:   internalworker.NewEngine(app.logger.With("module", "worker")),
			provided: make(map[reflect.Type]string),
		}
	})
	return app.workers.registry
}

// Continuous registers w under name as a continuous worker: Run is called
// once when the worker starts and must stay active until its context is
// cancelled; a failed run — an error, a panic, or a nil return before
// shutdown — is restarted under cfg's [RestartPolicy]. A restart calls Run
// again on the same value, so Run must be re-enterable.
//
// The worker is a component of the App, in cfg's tier (internal by default).
// Registered by value it has no edges the dependency graph can see: it takes
// its place in its tier by registration order. A worker that uses
// infrastructure is registered with [Workers.ContinuousProvided].
//
// Continuous takes at most one configuration and panics on misuse: a nil
// worker, an invalid or duplicate name, more than one configuration, a
// rejected value, or a call after Finalize.
func (s *Workers) Continuous(name string, w Worker, cfg ...ContinuousConfig) {
	call := fmt.Sprintf("Workers.Continuous(%q)", name)
	if internalworker.IsNilWorker(w) {
		panic(fmt.Sprintf("credo: %s: the worker is nil; pass a value with a Run method", call))
	}
	internalworker.ValidateName(call, name)
	c := oneWorkerConfig(call, cfg)
	tier := resolveWorkerTier(call, c.Tier, TierInternal)
	s.register(call, internalworker.NewContinuous(call, name, c.engine()), tier, w, nil)
}

// Scheduled registers w under name as a scheduled worker that runs once per
// activation of the cron expression expr. Runs never overlap: activations
// that pass during a run are skipped and logged. A failed run counts toward
// cfg.MaxConsecutiveFailures.
//
// The worker is a component of the App, in cfg's tier (ingress by default:
// it stops with the listener, so no run starts during the drain). Registered
// by value it has no edges; a worker that uses infrastructure is registered
// with [Workers.ScheduledProvided]. Every replica runs every schedule.
//
// Scheduled panics on misuse — a nil worker, an invalid or duplicate name,
// more than one configuration, a rejected value, an expression that does not
// parse ([ParseSchedule] returns that error instead), a call after Finalize.
func (s *Workers) Scheduled(name, expr string, w Worker, cfg ...ScheduledConfig) {
	call := fmt.Sprintf("Workers.Scheduled(%q)", name)
	if internalworker.IsNilWorker(w) {
		panic(fmt.Sprintf("credo: %s: the worker is nil; pass a value with a Run method", call))
	}
	internalworker.ValidateName(call, name)
	c := oneWorkerConfig(call, cfg)
	tier := resolveWorkerTier(call, c.Tier, TierIngress)
	s.register(call, internalworker.NewScheduled(call, name, expr, c.engine()), tier, w, nil)
}

// ContinuousProvided registers, under name, the continuous worker the DI
// container provides as T:
//
//	app.Provide(NewOrderConsumer)
//	app.Workers().ContinuousProvided[*OrderConsumer]("order-consumer")
//
// The worker's component is a constructor over T: the start walk builds it
// after T's component dependencies have started, and the worker stops before
// them. The Provide call that binds T may come before or after this call,
// both before Finalize; T may be an interface bound with [App.Alias]. A T
// without a binding fails Finalize; a constructor error, a panic or a nil T
// fails the start. One T registered under two names panics, since the
// container hands every registration the same instance.
//
// The configuration follows [Workers.Continuous].
func (s *Workers) ContinuousProvided[T Worker](name string, cfg ...ContinuousConfig) {
	t := reflect.TypeFor[T]()
	call := fmt.Sprintf("Workers.ContinuousProvided[%s](%q)", t, name)
	internalworker.ValidateName(call, name)
	c := oneWorkerConfig(call, cfg)
	tier := resolveWorkerTier(call, c.Tier, TierInternal)
	s.register(call, internalworker.NewContinuous(call, name, c.engine()), tier, nil,
		&providedWorker{t: t, constructor: providedConstructor[T]})
}

// ScheduledProvided registers, under name, the scheduled worker the DI
// container provides as T, running once per activation of expr. T is resolved
// as in [Workers.ContinuousProvided]; the configuration and the schedule
// follow [Workers.Scheduled].
func (s *Workers) ScheduledProvided[T Worker](name, expr string, cfg ...ScheduledConfig) {
	t := reflect.TypeFor[T]()
	call := fmt.Sprintf("Workers.ScheduledProvided[%s](%q)", t, name)
	internalworker.ValidateName(call, name)
	c := oneWorkerConfig(call, cfg)
	tier := resolveWorkerTier(call, c.Tier, TierIngress)
	s.register(call, internalworker.NewScheduled(call, name, expr, c.engine()), tier, nil,
		&providedWorker{t: t, constructor: providedConstructor[T]})
}

// Snapshot returns one WorkerInfo per worker of the App, in registration
// order: its resolved configuration and its live state.
func (s *Workers) Snapshot() []WorkerInfo {
	s.mu.Lock()
	workers := s.workers[:len(s.workers):len(s.workers)]
	s.mu.Unlock()
	infos := make([]WorkerInfo, 0, len(workers))
	for _, w := range workers {
		infos = append(infos, workerInfo(w.c.Snapshot(), w.tier))
	}
	return infos
}

// Lookup returns the worker registered under exactly name, or false. A name
// it does not know is the caller's error to report, never a pass.
func (s *Workers) Lookup(name string) (WorkerInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workers {
		if w.c.Definition().Name() == name {
			return workerInfo(w.c.Snapshot(), w.tier), true
		}
	}
	return WorkerInfo{}, false
}

// providedWorker is the DI side of a provided registration: the type the
// worker is resolved as, and the constructor of its component.
type providedWorker struct {
	t           reflect.Type
	constructor func(c *internalworker.Component) any
}

// providedConstructor returns the constructor the container receives for a
// provided worker: it takes T, so the graph orders the worker after T's
// dependencies, and returns the worker's component. Its result type decides
// whether the component answers Ready, since the App plans that from the
// type.
func providedConstructor[T Worker](c *internalworker.Component) any {
	attach := func(w T) error {
		if internalworker.IsNilWorker(w) {
			return fmt.Errorf("worker: %q: %s resolved to nil", c.Definition().Name(), reflect.TypeFor[T]())
		}
		c.SetWorker(w)
		return nil
	}
	if c.Definition().Readies() {
		rc := &internalworker.ReadyComponent{Component: c}
		return func(w T) (*internalworker.ReadyComponent, error) {
			if err := attach(w); err != nil {
				return nil, err
			}
			return rc, nil
		}
	}
	return func(w T) (*internalworker.Component, error) {
		if err := attach(w); err != nil {
			return nil, err
		}
		return c, nil
	}
}

// register is the single path behind the four registration methods, once
// the worker, the name, the configuration and the schedule are valid: it
// refuses a duplicate name or provided type and adds the worker's component
// to the App, whose refusal it reports under call.
func (s *Workers) register(call string, def *internalworker.Definition, tier Tier, w Worker,
	provided *providedWorker,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := def.Name()
	for _, existing := range s.workers {
		if existing.c.Definition().Name() == name {
			panic(fmt.Sprintf("credo: %s: a worker named %q is already registered; give each worker its own name",
				call, name))
		}
	}
	if provided != nil {
		if prior, taken := s.provided[provided.t]; taken {
			panic(fmt.Sprintf("credo: %s: %s is already registered as worker %q; the container hands "+
				"every registration the same instance, so register it once", call, provided.t, prior))
		}
	}

	c := internalworker.NewComponent(s.engine, def, w)
	var v any = c
	if def.Readies() {
		v = &internalworker.ReadyComponent{Component: c}
	}
	if provided != nil {
		v = provided.constructor(c)
	}
	// The worker's Finalize remedy, its Tier field, names how to declare an
	// internal worker on an ingress component ingress.
	err := s.app.container.Manage(v, di.Options{
		Ingress:       tier == TierIngress,
		Name:          internalworker.ComponentName(name),
		IngressRemedy: "Tier: credo.TierIngress in its worker configuration",
	})
	if m, ok := errors.AsType[*di.MisuseError](err); ok {
		panic("credo: " + call + ": " + m.Reason)
	}
	if provided != nil {
		s.provided[provided.t] = name
	}
	s.workers = append(s.workers, registeredWorker{c: c, tier: tier})
}

// oneWorkerConfig returns the single configuration of a registration, or the
// zero value; it panics, naming call, when more than one is given.
func oneWorkerConfig[C any](call string, cfgs []C) C {
	var zero C
	switch len(cfgs) {
	case 0:
		return zero
	case 1:
		return cfgs[0]
	}
	panic(fmt.Sprintf("credo: %s: %d configurations given; pass at most one %T", call, len(cfgs), zero))
}

// resolveWorkerTier returns tier, or def when tier is zero; it panics on a
// value that is neither.
func resolveWorkerTier(call string, tier, def Tier) Tier {
	switch tier {
	case 0:
		return def
	case TierIngress, TierInternal:
		return tier
	}
	panic(fmt.Sprintf("credo: %s: %s is not a tier; use credo.TierIngress, credo.TierInternal, "+
		"or zero for the default", call, tier))
}
