package credo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
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
			app:    app,
			engine: internalworker.NewEngine(app.logger.With("module", "worker")),
		}
	})
	return app.workers.registry
}

// Continuous registers w under name as a continuous worker: its Run is
// called once when the worker starts and must stay active until its context
// is cancelled; a failed run — an error, a panic, or a nil return before
// shutdown — is restarted under cfg's [RestartPolicy]. A restart runs the
// same worker again, so a run must be re-enterable.
//
// w takes one of three forms:
//   - a worker: a value whose type implements [Worker], or a
//     func(context.Context) error, named function types of that signature
//     included. It has no edges the dependency graph can see and takes its
//     place in its tier by registration order.
//   - a run function: a function that takes exactly one context.Context, in
//     any position, and returns only an error, such as
//     func(ctx context.Context, db *sqldb.DB) error or the method expression
//     (*OutboxRelay).Run of a bound worker. Its other parameters are resolved
//     from the container once, after their components have started, and each
//     run calls the function with that run's context and those values.
//   - a constructor of a worker: func(deps...) W or func(deps...) (W, error),
//     where W is a worker in either form. The start walk builds it after its
//     dependencies have started, and the value it builds, the component
//     "worker-value:<name>", is not a binding: nothing resolves it, each
//     registration calls its constructor separately, and it is torn down, in
//     the internal tier, after the worker has stopped. A value it builds
//     that a binding or a value handed to [App.Manage] also holds — the
//     binding's own value, or a wrapper that embeds its handle — shares that
//     registration's readiness entry and teardown. A constructor that returns
//     a method value hands over the function alone, which has no teardown.
//
// The worker is a component of the App, in cfg's tier (internal by default),
// and stops before what its parameters depend on.
//
// Continuous takes at most one configuration and panics on misuse: a nil
// worker, a w of none of the three forms, an invalid or duplicate name, more
// than one configuration, a rejected value, or a call after Finalize.
// Finalize reports a missing parameter, and the start a constructor's error,
// panic or nil worker.
func (s *Workers) Continuous(name string, w any, cfg ...ContinuousConfig) {
	call := fmt.Sprintf("Workers.Continuous(%q)", name)
	form := workerFormOf(call, w)
	internalworker.ValidateName(call, name)
	c := oneWorkerConfig(call, cfg)
	tier := resolveWorkerTier(call, c.Tier, TierInternal)
	s.register(call, internalworker.NewContinuous(call, name, c.engine()), tier, form)
}

// Scheduled registers w under name as a scheduled worker that runs once per
// activation of the cron expression expr. Runs never overlap: activations
// that pass during a run are skipped and logged. A failed run counts toward
// cfg.MaxConsecutiveFailures.
//
// w is a worker, a run function or a constructor of a worker, as for
// [Workers.Continuous]. The worker is a component of the App, in cfg's tier
// (ingress by default: it stops with the listener, so no run starts during
// the drain); the value a constructor builds is internal, torn down after
// the HTTP drain. Every replica runs every schedule.
//
// Scheduled panics on misuse — a nil worker, a w of none of the three forms,
// an invalid or duplicate name, more than one configuration, a rejected
// value, an expression that does not parse ([ParseSchedule] returns that
// error instead), a call after Finalize.
func (s *Workers) Scheduled(name, expr string, w any, cfg ...ScheduledConfig) {
	call := fmt.Sprintf("Workers.Scheduled(%q)", name)
	form := workerFormOf(call, w)
	internalworker.ValidateName(call, name)
	c := oneWorkerConfig(call, cfg)
	tier := resolveWorkerTier(call, c.Tier, TierIngress)
	s.register(call, internalworker.NewScheduled(call, name, expr, c.engine()), tier, form)
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

var (
	workerIfaceType = reflect.TypeFor[Worker]()
	workerFuncType  = reflect.TypeFor[func(context.Context) error]()
	workerCtxType   = reflect.TypeFor[context.Context]()
	workerErrType   = reflect.TypeFor[error]()
)

// workerForm is what a registration's w is: exactly one of a worker, a run
// function or a constructor.
type workerForm struct {
	worker Worker
	// run is a run function and runCtx the position of its context.
	run    reflect.Value
	runCtx int
	// ctor is a constructor of a worker.
	ctor reflect.Value
}

// workerFormOf decides the form of w, panicking, naming call, when it is
// nil or none of the three.
func workerFormOf(call string, w any) workerForm {
	if isNilValue(w) {
		panic(fmt.Sprintf("credo: %s: the worker is nil; pass a value with a Run method", call))
	}
	if wk, ok := w.(Worker); ok {
		return workerForm{worker: wk}
	}
	v := reflect.ValueOf(w)
	t := v.Type()
	if t.Kind() != reflect.Func {
		panic(noFormMessage(call, t))
	}
	ctxAt, contexts := -1, 0
	for i := range t.NumIn() {
		if t.In(i) == workerCtxType {
			ctxAt = i
			contexts++
		}
	}
	const runShape = "a run function takes exactly one context.Context, in any position, and returns only an error"
	switch {
	case contexts > 1:
		panic(fmt.Sprintf("credo: %s: %s takes %d context.Context parameters; %s", call, t, contexts, runShape))
	case contexts == 1 && (t.NumOut() != 1 || t.Out(0) != workerErrType):
		panic(fmt.Sprintf("credo: %s: %s takes a context.Context and returns %s; %s", call, t, results(t),
			runShape))
	case contexts == 1 && t.NumIn() == 1:
		return workerForm{worker: funcWorker(v.Convert(workerFuncType).Interface().(func(context.Context) error))}
	case contexts == 1:
		return workerForm{run: v, runCtx: ctxAt}
	}
	switch {
	case t.NumOut() == 2 && t.Out(1).Implements(workerErrType):
	case t.NumOut() == 1:
	default:
		panic(noFormMessage(call, t))
	}
	if !isWorkerType(t.Out(0)) {
		panic(noFormMessage(call, t))
	}
	return workerForm{ctor: v}
}

// noFormMessage is the panic of a w of none of the forms, which names them.
func noFormMessage(call string, t reflect.Type) string {
	return fmt.Sprintf("credo: %s: %s is none of the forms a worker registers as; pass a worker (a credo.Worker, "+
		"or a func(context.Context) error), a run function (one that takes exactly one context.Context and "+
		"returns only an error, whose other parameters the container supplies), or a constructor of a worker "+
		"(func(deps...) W or func(deps...) (W, error), where W is a worker)", call, t)
}

// results renders a function type's results for a message.
func results(t reflect.Type) string {
	switch t.NumOut() {
	case 0:
		return "nothing"
	case 1:
		return t.Out(0).String()
	}
	names := make([]string, t.NumOut())
	for i := range names {
		names[i] = t.Out(i).String()
	}
	return "(" + strings.Join(names, ", ") + ")"
}

// isWorkerType reports whether a value of type t is a worker: it implements
// Worker, or it is a func(context.Context) error, named or not.
func isWorkerType(t reflect.Type) bool {
	return t.Implements(workerIfaceType) || (t.Kind() == reflect.Func && t.ConvertibleTo(workerFuncType))
}

// asWorker returns the Worker a value of a worker type is.
func asWorker(v any) Worker {
	if w, ok := v.(Worker); ok {
		return w
	}
	return funcWorker(reflect.ValueOf(v).Convert(workerFuncType).Interface().(func(context.Context) error))
}

// isNilValue reports whether v is nil or holds a nil value of a nilable
// kind.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	}
	return false
}

// funcWorker is a func(context.Context) error registered as a worker.
type funcWorker func(context.Context) error

func (f funcWorker) Run(ctx context.Context) error { return f(ctx) }

// runFunction is a run function's worker: each run calls the function with
// the run's context at its position and the parameters the loop's
// constructor resolved once.
type runFunction struct {
	fn    reflect.Value
	ctxAt int
	args  []reflect.Value
}

func (r *runFunction) Run(ctx context.Context) error {
	in := make([]reflect.Value, 0, len(r.args)+1)
	in = append(in, r.args[:r.ctxAt]...)
	in = append(in, reflect.ValueOf(&ctx).Elem())
	in = append(in, r.args[r.ctxAt:]...)
	var out []reflect.Value
	if r.fn.Type().IsVariadic() {
		out = r.fn.CallSlice(in)
	} else {
		out = r.fn.Call(in)
	}
	err, _ := out[0].Interface().(error)
	return err
}

// loopValue returns the loop's component and its type: one whose type shows
// Ready when the registration sets a readiness condition, since the App
// plans that from the type.
func loopValue(c *internalworker.Component) (any, reflect.Type) {
	if c.Definition().Readies() {
		rc := &internalworker.ReadyComponent{Component: c}
		return rc, reflect.TypeOf(rc)
	}
	return c, reflect.TypeOf(c)
}

// runConstructor returns the constructor the container receives for a run
// function's loop: its parameters are the function's other than the
// context, so the graph orders the loop after them, and it attaches the
// function with the values it received to the loop's component.
func runConstructor(c *internalworker.Component, fn reflect.Value, ctxAt int) any {
	ft := fn.Type()
	params := make([]reflect.Type, 0, ft.NumIn()-1)
	for i := range ft.NumIn() {
		if i != ctxAt {
			params = append(params, ft.In(i))
		}
	}
	loop, loopType := loopValue(c)
	ctorType := reflect.FuncOf(params, []reflect.Type{loopType, workerErrType}, false)
	return reflect.MakeFunc(ctorType, func(args []reflect.Value) []reflect.Value {
		c.SetWorker(&runFunction{fn: fn, ctxAt: ctxAt, args: slices.Clone(args)})
		return []reflect.Value{reflect.ValueOf(loop), reflect.Zero(workerErrType)}
	}).Interface()
}

// valueConstructor returns the constructor the container receives for a
// worker's built value: the application's constructor, with its parameters
// and its result type W, which the App plans Start and Ready from, returning
// (W, error), so that a nil worker fails the build.
func valueConstructor(call string, fn reflect.Value) any {
	ft := fn.Type()
	params := make([]reflect.Type, ft.NumIn())
	for i := range params {
		params[i] = ft.In(i)
	}
	w := ft.Out(0)
	ctorType := reflect.FuncOf(params, []reflect.Type{w, workerErrType}, false)
	return reflect.MakeFunc(ctorType, func(args []reflect.Value) []reflect.Value {
		var out []reflect.Value
		if ft.IsVariadic() {
			out = fn.CallSlice(args)
		} else {
			out = fn.Call(args)
		}
		fail := func(err error) []reflect.Value {
			ev := reflect.New(workerErrType).Elem()
			ev.Set(reflect.ValueOf(err))
			return []reflect.Value{reflect.Zero(w), ev}
		}
		if len(out) == 2 {
			if err, _ := out[1].Interface().(error); err != nil {
				return fail(err)
			}
		}
		if isNilValue(out[0].Interface()) {
			return fail(fmt.Errorf("%s: the constructor returned a nil %s; return a worker, or an error", call, w))
		}
		return []reflect.Value{out[0], reflect.Zero(workerErrType)}
	}).Interface()
}

// register is the single path behind the registration methods, once the
// form, the name, the configuration and the schedule are valid: it refuses
// a duplicate name and adds the worker's units to the App — the loop
// "worker:<name>", and for a constructor the built value
// "worker-value:<name>" the loop depends on — whose refusal it reports
// under call.
func (s *Workers) register(call string, def *internalworker.Definition, tier Tier, form workerForm) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := def.Name()
	for _, existing := range s.workers {
		if existing.c.Definition().Name() == name {
			panic(fmt.Sprintf("credo: %s: a worker named %q is already registered; give each worker its own name",
				call, name))
		}
	}
	refused := func(err error) {
		if m, ok := errors.AsType[*di.MisuseError](err); ok {
			panic("credo: " + call + ": " + m.Reason)
		}
	}
	container := s.app.container
	// The worker's Finalize remedy, its Tier field, names how to declare an
	// internal worker on an ingress component ingress.
	loopOpts := di.Options{
		Ingress:       tier == TierIngress,
		Name:          internalworker.ComponentName(name),
		IngressRemedy: "Tier: credo.TierIngress in its worker configuration",
		Call:          call,
	}

	c := internalworker.NewComponent(s.engine, def, form.worker)
	switch {
	case form.worker != nil:
		loop, _ := loopValue(c)
		_, err := container.Manage(loop, loopOpts)
		refused(err)
	case form.run.IsValid():
		_, err := container.Manage(runConstructor(c, form.run, form.runCtx), loopOpts)
		refused(err)
	default:
		// The loop's name is checked before the value is added, so that a
		// refusal adds neither.
		refused(container.ManageNameFree(loopOpts.Name))
		value, err := container.Manage(valueConstructor(call, form.ctor), di.Options{
			Name:   "worker-value:" + name,
			Worker: call,
		})
		refused(err)
		build := func() error {
			v, buildErr := container.BuildUnit(value)
			if buildErr != nil {
				return buildErr
			}
			c.SetWorker(asWorker(v))
			return nil
		}
		var ctor any
		if def.Readies() {
			rc := &internalworker.ReadyComponent{Component: c}
			ctor = func() (*internalworker.ReadyComponent, error) { return rc, build() }
		} else {
			ctor = func() (*internalworker.Component, error) { return c, build() }
		}
		loop, err := container.Manage(ctor, loopOpts)
		refused(err)
		refused(container.AddEdge(loop, value))
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
