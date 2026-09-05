// Adapted from github.com/samber/do (MIT License).

package di

import (
	"context"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"time"
)

// Container is a type-safe dependency injection container using Go generics.
// Services are registered with Provide[T] and resolved with Resolve[T].
// All services use the Singleton lifecycle.
//
// The container moves through three phases. During registration, bindings are
// written and prebuilt values may be adopted; nothing is constructed. Seal
// freezes the registrations and validates the graph; Resolve is admitted only
// from then on. Shutdown enters the closing phase: Resolve is rejected with
// [ErrClosed], admitted builds are accounted for, and instances are shut down
// consumers-before-dependencies.
type Container struct {
	mu             sync.RWMutex
	registrations  map[reflect.Type]provider
	protected      map[reflect.Type]struct{} // bindings Replace must not overwrite
	singletons     map[reflect.Type]*singletonEntry
	aliases        map[reflect.Type]reflect.Type // interface → concrete type (Alias)
	manyBindings   map[reflect.Type][]reflect.Type
	manyBindingSet map[reflect.Type]map[reflect.Type]struct{}
	order          []reflect.Type // registration order (teardown tie-break)
	// frameworkProviders produces constructor parameters the framework injects
	// without a registration (credo.Infra, Model 1). Written at setup only.
	frameworkProviders map[reflect.Type]FrameworkProvider

	// logger receives teardown diagnostics. Never nil; defaults to discard.
	logger *slog.Logger

	// frozen closes the registration window: set by Seal, by the validation-free
	// Freeze used for bootstrap teardown, and by Shutdown.
	frozen bool
	// sealed records that Seal ran (successfully or not). Resolve is admitted
	// only after it.
	sealed   bool
	sealOnce sync.Once
	sealErr  error

	// closing is set when Shutdown begins. From then on Resolve is rejected
	// with ErrClosed, no new construction is admitted, and results of builds
	// that were already running are withheld from callers.
	closing bool
	// shutdownCtx is the Shutdown context, recorded so a build completing after
	// the context ended can be routed to the late-cleanup path.
	shutdownCtx context.Context
	// teardownDone records that the shutdown pass returned its report.
	teardownDone bool
	// buildDone is a capacity-one wake-up for the shutdown pass: every build
	// completion performs a non-blocking send.
	buildDone chan struct{}
}

// entryState is the construction state of one singleton.
type entryState uint8

const (
	// entryUnbuilt: registered, constructor never started.
	entryUnbuilt entryState = iota
	// entryBuilding: a constructor is running; done is non-nil and open.
	entryBuilding
	// entryBuilt: value holds the instance (prebuilt or constructed).
	entryBuilt
	// entryFailed: the constructor returned an error or panicked; err holds
	// the terminal failure and no retry is attempted.
	entryFailed
)

// singletonEntry is the per-type construction record. Every field is written
// under Container.mu; constructors run outside the lock while the entry is in
// entryBuilding, and waiters block on done.
type singletonEntry struct {
	state entryState
	// done is closed when a build completes (built or failed). It is created
	// when the build is admitted and stays nil for prebuilt values.
	done  chan struct{}
	value any
	err   error

	buildStart    time.Time
	buildDuration time.Duration

	// late marks an instance whose construction completed after the shutdown
	// context ended. It is owned by the separate late-cleanup path and never
	// by the shutdown pass.
	late bool
}

// New creates a new Container.
func New() *Container {
	return &Container{
		registrations:  make(map[reflect.Type]provider),
		protected:      make(map[reflect.Type]struct{}),
		singletons:     make(map[reflect.Type]*singletonEntry),
		aliases:        make(map[reflect.Type]reflect.Type),
		manyBindings:   make(map[reflect.Type][]reflect.Type),
		manyBindingSet: make(map[reflect.Type]map[reflect.Type]struct{}),

		frameworkProviders: make(map[reflect.Type]FrameworkProvider),
		logger:             slog.New(slog.DiscardHandler),
		buildDone:          make(chan struct{}, 1),
	}
}

// SetLogger installs the logger that receives teardown diagnostics (Debug
// timings on success, Warn/Error for late completions and best-effort cleanup).
// A nil logger restores the discarding default. Must be called before Shutdown.
func (c *Container) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	c.mu.Lock()
	c.logger = l
	c.mu.Unlock()
}

// Freeze closes the registration window without validating the graph or
// consuming Seal. It exists for bootstrap teardown: an App shut down before it
// ever served must stop accepting bindings, yet must not be validated (a
// half-registered graph is not an error at that point) and must not enter the
// closing phase before the drain stages that precede DI teardown have run.
func (c *Container) Freeze() {
	c.mu.Lock()
	c.frozen = true
	c.mu.Unlock()
}

// findRegistration searches for a registration by type, following aliases.
// It returns the registration, the canonical (concrete) type under which the
// singleton is cached, and whether the lookup succeeded.
func (c *Container) findRegistration(t reflect.Type) (provider, reflect.Type, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.findRegistrationLocked(t)
}

func (c *Container) findRegistrationLocked(t reflect.Type) (provider, reflect.Type, bool) {
	// Direct registration lookup.
	if reg, ok := c.registrations[t]; ok {
		return reg, t, true
	}

	// Follow alias chain: interface → concrete type.
	if concrete, ok := c.aliases[t]; ok {
		if reg, ok := c.registrations[concrete]; ok {
			return reg, concrete, true
		}
	}

	return nil, t, false
}

// hasDirectRegistration reports whether a type has its own registration entry.
func (c *Container) hasDirectRegistration(t reflect.Type) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, ok := c.registrations[t]
	return ok
}

// collectionBindings returns a copy of the concrete types bound to an
// interface via BindMany, preserving binding order.
func (c *Container) collectionBindings(t reflect.Type) []reflect.Type {
	c.mu.RLock()
	defer c.mu.RUnlock()

	bindings := c.manyBindings[t]
	if len(bindings) == 0 {
		return nil
	}

	return slices.Clone(bindings)
}

func isInterfaceSlice(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Interface
}

// frozenError is the shared registration-window rejection.
func frozenError(op string, t reflect.Type) error {
	return &frozenErr{op: op, t: t}
}

type frozenErr struct {
	op string
	t  reflect.Type
}

func (e *frozenErr) Error() string {
	return "di: " + e.op + "[" + e.t.String() + "]: container is frozen (finalized or shut down)"
}
