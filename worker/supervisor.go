package worker

import (
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
	"weak"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/internal/kernel"
)

// Supervisor registers an App's workers and reports their state. It is a
// registry, not a lifecycle: each registration adds the worker to the App as
// a component named "worker:<name>", which the App starts and stops in its
// tier like every other component. The supervisor has no Start, no Shutdown
// and no DI binding.
type Supervisor struct {
	app    *credo.App
	logger *slog.Logger

	// jitter picks a restart delay in [lo, hi]; uniformJitter unless a test
	// pins it to a bound.
	jitter func(lo, hi time.Duration) time.Duration

	mu         sync.Mutex
	components []*component // registration order
}

// Use returns a new supervisor for app's workers. It registers nothing: it
// binds nothing into the container, installs no hook and adds no component.
// The supervisor logs through app.Logger with module=worker. Use may be called
// more than once; each supervisor is an independent registry, and the App
// refuses a worker name that another supervisor already registered.
//
// A module that registers workers takes the supervisor as a parameter; an
// application that wants it injected binds it itself with app.ProvideValue.
func Use(app *credo.App) *Supervisor {
	if app == nil {
		panic("worker: Use: app must not be nil; pass the *credo.App the workers belong to")
	}
	return &Supervisor{app: app, logger: app.Logger().With("module", "worker"), jitter: uniformJitter}
}

// Continuous registers w under name as a continuous worker: Run is called
// once when the worker starts and must stay active until its context is
// cancelled; a failed run — an error, a panic, or a nil return before
// shutdown — is restarted under cfg's [Restart] policy. A restart calls Run
// again on the same value, so Run must be re-enterable.
//
// The worker is a component of the App, in cfg's tier (internal by default).
// Registered by value it has no edges the dependency graph can see: it takes
// its place in its tier by registration order. A worker that uses
// infrastructure is registered with [Supervisor.ContinuousProvided].
//
// Continuous takes at most one configuration and panics on misuse: a nil
// worker, an invalid or duplicate name, more than one configuration, a
// rejected value, or a call after Finalize.
func (s *Supervisor) Continuous(name string, w Worker, cfg ...ContinuousConfig) {
	call := fmt.Sprintf("Continuous(%q)", name)
	if isNilWorker(w) {
		panic(fmt.Sprintf("worker: %s: the worker is nil; pass a value with a Run method", call))
	}
	validateName(call, name)
	def := &definition{
		name:       name,
		kind:       KindContinuous,
		continuous: resolveContinuous(call, oneConfig(call, cfg)),
	}
	s.register(call, def, w, nil)
}

// Scheduled registers w under name as a scheduled worker that runs once per
// activation of the cron expression expr. Runs never overlap: activations
// that pass during a run are skipped and logged. A failed run counts toward
// cfg.MaxConsecutiveFailures.
//
// The worker is a component of the App, in cfg's tier (ingress by default:
// it stops with the listener, so no run starts during the drain). Registered
// by value it has no edges; a worker that uses infrastructure is registered
// with [Supervisor.ScheduledProvided]. Every replica runs every schedule.
//
// Scheduled panics on misuse — a nil worker, an invalid or duplicate name,
// more than one configuration, a rejected value, an expression that does not
// parse ([ParseSchedule] returns that error instead), a call after Finalize.
func (s *Supervisor) Scheduled(name, expr string, w Worker, cfg ...ScheduledConfig) {
	call := fmt.Sprintf("Scheduled(%q)", name)
	if isNilWorker(w) {
		panic(fmt.Sprintf("worker: %s: the worker is nil; pass a value with a Run method", call))
	}
	validateName(call, name)
	def := &definition{
		name:      name,
		kind:      KindScheduled,
		scheduled: resolveScheduled(call, oneConfig(call, cfg)),
		schedule:  mustSchedule(call, expr),
	}
	s.register(call, def, w, nil)
}

// ContinuousProvided registers, under name, the continuous worker the DI
// container provides as T:
//
//	app.Provide[*OrderConsumer](NewOrderConsumer)
//	workers.ContinuousProvided[*OrderConsumer]("order-consumer")
//
// The worker's component is a constructor over T: the start walk builds it
// after T's component dependencies have started, and the worker stops before
// them. Provide[T] may come before or after this call, both before Finalize;
// T may be an interface bound with app.Alias. A T without a binding fails
// Finalize; a constructor error, a panic or a nil T fails the start. One T
// registered under two names panics, in any supervisor of the App, since the
// container hands every registration the same instance.
//
// The configuration follows [Supervisor.Continuous].
func (s *Supervisor) ContinuousProvided[T Worker](name string, cfg ...ContinuousConfig) {
	t := reflect.TypeFor[T]()
	call := fmt.Sprintf("ContinuousProvided[%s](%q)", t, name)
	validateName(call, name)
	def := &definition{
		name:       name,
		kind:       KindContinuous,
		continuous: resolveContinuous(call, oneConfig(call, cfg)),
	}
	s.register(call, def, nil, &providedWorker{t: t, constructor: providedConstructor[T]})
}

// ScheduledProvided registers, under name, the scheduled worker the DI
// container provides as T, running once per activation of expr. T is resolved
// as in [Supervisor.ContinuousProvided]; the configuration and the schedule
// follow [Supervisor.Scheduled].
func (s *Supervisor) ScheduledProvided[T Worker](name, expr string, cfg ...ScheduledConfig) {
	t := reflect.TypeFor[T]()
	call := fmt.Sprintf("ScheduledProvided[%s](%q)", t, name)
	validateName(call, name)
	def := &definition{
		name:      name,
		kind:      KindScheduled,
		scheduled: resolveScheduled(call, oneConfig(call, cfg)),
		schedule:  mustSchedule(call, expr),
	}
	s.register(call, def, nil, &providedWorker{t: t, constructor: providedConstructor[T]})
}

// Snapshot returns one Info per registered worker, in registration order:
// its resolved configuration and its live state.
func (s *Supervisor) Snapshot() []Info {
	s.mu.Lock()
	components := s.components[:len(s.components):len(s.components)]
	s.mu.Unlock()
	infos := make([]Info, 0, len(components))
	for _, c := range components {
		infos = append(infos, c.r.snapshot())
	}
	return infos
}

// Lookup returns the worker registered under exactly name, or false. A name
// it does not know is the caller's error to report, never a pass.
func (s *Supervisor) Lookup(name string) (Info, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.components {
		if c.def.name == name {
			return c.r.snapshot(), true
		}
	}
	return Info{}, false
}

// providedWorker is the DI side of a provided registration: the type the
// worker is resolved as, and the constructor of its component.
type providedWorker struct {
	t           reflect.Type
	constructor func(c *component) any
}

// providedConstructor returns the constructor Manage receives for a provided
// worker: it takes T, so the graph orders the worker after T's dependencies,
// and returns the worker's component. Its result type decides whether the
// component answers Ready, since the App plans that from the type.
func providedConstructor[T Worker](c *component) any {
	attach := func(w T) error {
		if isNilWorker(w) {
			return fmt.Errorf("worker: %q: %s resolved to nil", c.def.name, reflect.TypeFor[T]())
		}
		c.r.worker = w
		return nil
	}
	if c.def.readies() {
		rc := &readyComponent{c}
		return func(w T) (*readyComponent, error) {
			if err := attach(w); err != nil {
				return nil, err
			}
			return rc, nil
		}
	}
	return func(w T) (*component, error) {
		if err := attach(w); err != nil {
			return nil, err
		}
		return c, nil
	}
}

// register is the single path behind the four registration methods, once
// the worker, the name, the configuration and the schedule are valid: it
// refuses a duplicate name or provided type and adds the worker's component
// to the App.
func (s *Supervisor) register(call string, def *definition, w Worker, provided *providedWorker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.components {
		if existing.def.name == def.name {
			panic(fmt.Sprintf("worker: %s: duplicate worker name %q; give each worker its own name", call, def.name))
		}
	}

	c := &component{s: s, def: def, r: newRunner(def, w)}
	var v any = c
	if def.readies() {
		v = &readyComponent{c}
	}
	if provided != nil {
		if prior, taken := reserveProvided(s.app, provided.t, def.name); taken {
			panic(fmt.Sprintf("worker: %s: %s is already registered as worker %q; the container hands "+
				"every registration the same instance, so register it once", call, provided.t, prior))
		}
		v = provided.constructor(c)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				if provided != nil {
					releaseProvided(s.app, provided.t)
				}
				panic(manageMisuse(call, def.name, r))
			}
		}()
		// Through the kernel rather than App.Manage, so that Finalize's
		// finding for an internal worker on an ingress component names the
		// worker's own remedy, its Tier field.
		kernel.Manage(s.app, kernel.Component{
			Value:         v,
			Name:          componentName(def.name),
			Ingress:       tier(def) == credo.TierIngress,
			IngressRemedy: "Tier: credo.TierIngress in its worker configuration",
		})
	}()
	s.components = append(s.components, c)
}

// manageMisuse turns the App's refusal of a worker's component into the
// worker's own message, naming the call and the remedy.
func manageMisuse(call, name string, r any) string {
	msg := fmt.Sprint(r)
	switch {
	case strings.Contains(msg, "called after Finalize"):
		return fmt.Sprintf("worker: %s after app.Finalize; register workers before Finalize", call)
	case strings.Contains(msg, "called after shutdown began"):
		return fmt.Sprintf("worker: %s after the App shut down; register workers before Finalize", call)
	case strings.Contains(msg, "is already managed"):
		return fmt.Sprintf("worker: %s: the App already has a component named %q, registered by another "+
			"supervisor or with credo.Named; give each worker its own name", call, componentName(name))
	}
	return fmt.Sprintf("worker: %s: %s", call, msg)
}

// componentName is the name of a worker's component, under which readiness
// and the lifecycle's reports name it.
func componentName(name string) string { return "worker:" + name }

// tier returns the resolved tier of a definition.
func tier(def *definition) credo.Tier {
	if def.kind == KindScheduled {
		return def.scheduled.Tier
	}
	return def.continuous.Tier
}

// validateName applies the worker name rules. Names are never normalized, so
// the registered name is exactly the one every log line and snapshot reports.
// No prefix is reserved: health's reserved "credo." prefix applies to the
// full component name, which always starts with "worker:".
func validateName(call, name string) {
	switch {
	case name == "":
		panic(fmt.Sprintf("worker: %s: the name is empty; name the worker", call))
	case strings.TrimSpace(name) != name:
		panic(fmt.Sprintf("worker: %s: the name has leading or trailing whitespace; names are never "+
			"trimmed, so remove it", call))
	case strings.ContainsFunc(name, unicode.IsControl):
		panic(fmt.Sprintf("worker: %s: the name contains control characters; remove them", call))
	}
}

// mustSchedule parses expr, panicking with the parse error.
func mustSchedule(call, expr string) *Schedule {
	schedule, err := ParseSchedule(expr)
	if err != nil {
		panic(fmt.Sprintf("worker: %s: %s; validate a schedule from configuration with "+
			"worker.ParseSchedule before registering it", call, strings.TrimPrefix(err.Error(), "worker: ")))
	}
	return schedule
}

// providedTypes records, per App, the type of every provided worker and the
// name it was registered under: one T under two names would hand both loops
// one instance, in one supervisor or across supervisors of the App. The App
// is held weakly and its entry removed when it is collected.
var providedTypes struct {
	mu    sync.Mutex
	byApp map[weak.Pointer[credo.App]]map[reflect.Type]string
}

// reserveProvided records t under name for app, or returns the name t is
// already registered under.
func reserveProvided(app *credo.App, t reflect.Type, name string) (prior string, taken bool) {
	providedTypes.mu.Lock()
	defer providedTypes.mu.Unlock()
	key := weak.Make(app)
	types, ok := providedTypes.byApp[key]
	if !ok {
		if providedTypes.byApp == nil {
			providedTypes.byApp = make(map[weak.Pointer[credo.App]]map[reflect.Type]string)
		}
		types = make(map[reflect.Type]string)
		providedTypes.byApp[key] = types
		runtime.AddCleanup(app, forgetApp, key)
	}
	if prior, taken = types[t]; taken {
		return prior, true
	}
	types[t] = name
	return "", false
}

// releaseProvided undoes the reservation of a registration the App refused.
func releaseProvided(app *credo.App, t reflect.Type) {
	providedTypes.mu.Lock()
	defer providedTypes.mu.Unlock()
	delete(providedTypes.byApp[weak.Make(app)], t)
}

func forgetApp(key weak.Pointer[credo.App]) {
	providedTypes.mu.Lock()
	defer providedTypes.mu.Unlock()
	delete(providedTypes.byApp, key)
}
