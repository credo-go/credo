package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/credo-go/credo"
	internalhealth "github.com/credo-go/credo/internal/health"
)

// DefaultPingTimeout is the default timeout for the initial health check
// performed by [Register].
const DefaultPingTimeout = 5 * time.Second

// RegisterOption configures a [Register] call.
type RegisterOption func(*registerOptions)

type registerOptions struct {
	name        string
	pingTimeout time.Duration
	lifecycle   Lifecycle
}

type registerPlan struct {
	name        string
	pingTimeout time.Duration
	lifecycle   Lifecycle
}

// WithName sets the display name for health reporting.
// If not set, the name is derived from the type parameter R via reflection.
func WithName(name string) RegisterOption {
	return func(o *registerOptions) {
		o.name = name
	}
}

// WithPingTimeout overrides the default ping timeout (5s) for the
// initial health check performed by [Register].
func WithPingTimeout(d time.Duration) RegisterOption {
	return func(o *registerOptions) {
		o.pingTimeout = d
	}
}

// WithLifecycle provides the Lifecycle handle when the value R does not
// implement Lifecycle itself (e.g., a wrapper that keeps the connection
// in a named field — wrappers that embed the connection inherit its
// methods and need no explicit handle).
func WithLifecycle(lc Lifecycle) RegisterOption {
	return func(o *registerOptions) {
		o.lifecycle = lc
	}
}

// Register registers value as type R in the DI container, pings the
// connection, and tracks it in the [Registry] for lifecycle and health
// management.
//
// If value implements [Lifecycle], it is used directly for ping/shutdown/health.
// Otherwise, provide the Lifecycle handle via [WithLifecycle].
//
// Steps:
//  1. Resolve Lifecycle — use value if it implements Lifecycle, otherwise WithLifecycle
//  2. Ping — verify connection is alive (fail-fast at startup)
//  3. Ensure Registry — resolve or create Registry in DI
//  4. Track — add Lifecycle handle to Registry for ping/health aggregation
//  5. DI register — register value as type R via credo.ProvideValue
//
// Shutdown ownership: closing is the DI container's job alone. A value
// that implements [credo.Shutdowner] (every Lifecycle does) is closed by
// the container during app shutdown, in reverse registration order. The
// Registry never closes connections. When value does not implement
// Shutdowner — possible only with [WithLifecycle] — Register logs a
// warning and closing stays with the caller: close the underlying
// connection yourself (e.g. via [credo.App.OnShutdown]) or register a
// value type that implements Shutdowner (embedding the connection does).
//
// On failure, framework-owned registration state is rolled back. The
// caller retains ownership of the provided lifecycle value.
func Register[R any](app *credo.App, value R, opts ...RegisterOption) error {
	if app == nil {
		return fmt.Errorf("store: app must not be nil")
	}
	if isNilValue(value) {
		return fmt.Errorf("store: value must not be nil")
	}

	plan, err := buildRegisterPlan[R](value, opts...)
	if err != nil {
		return err
	}

	if err := pingLifecycle(plan); err != nil {
		return err
	}

	rollbackTrack, err := trackLifecycle(app, plan)
	if err != nil {
		return err
	}

	if err := publishValue(app, plan.name, value, rollbackTrack); err != nil {
		return err
	}

	// The container only closes singletons that implement Shutdowner; a
	// value without it (WithLifecycle registration) has no framework-owned
	// closing path — surface that instead of leaking silently.
	if _, ok := any(value).(credo.Shutdowner); !ok {
		app.Logger().Warn(
			"store: connection will not be closed by the framework (registered value does not implement Shutdowner)",
			"store", plan.name,
		)
	}
	return nil
}

func buildRegisterPlan[R any](value R, opts ...RegisterOption) (registerPlan, error) {
	o := registerOptions{
		pingTimeout: DefaultPingTimeout,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.pingTimeout <= 0 {
		return registerPlan{}, fmt.Errorf("store: ping timeout must be > 0, got %s", o.pingTimeout)
	}

	name := o.name
	if name == "" {
		name = registerName[R]()
	}

	lc := resolveLifecycle(value, o.lifecycle)
	if lc == nil {
		return registerPlan{}, fmt.Errorf("store: %q does not implement Lifecycle and no WithLifecycle option provided", name)
	}

	return registerPlan{
		name:        name,
		pingTimeout: o.pingTimeout,
		lifecycle:   lc,
	}, nil
}

func registerName[R any]() string {
	rType := reflect.TypeFor[R]()
	if name := rType.Name(); name != "" {
		return name
	}
	return rType.String()
}

func resolveLifecycle[R any](value R, explicit Lifecycle) Lifecycle {
	if explicit != nil {
		return explicit
	}
	if lc, ok := any(value).(Lifecycle); ok {
		return lc
	}
	return nil
}

func pingLifecycle(plan registerPlan) error {
	pingCtx, cancel := context.WithTimeout(context.Background(), plan.pingTimeout)
	defer cancel()

	if err := plan.lifecycle.Ping(pingCtx); err != nil {
		return fmt.Errorf("store: ping %q: %w", plan.name, err)
	}
	return nil
}

func trackLifecycle(app *credo.App, plan registerPlan) (func() error, error) {
	reg, err := ensureRegistry(app)
	if err != nil {
		return nil, err
	}

	if err := reg.Add(plan.name, plan.lifecycle); err != nil {
		return nil, fmt.Errorf("store: track %q: %w", plan.name, err)
	}

	rollback := func() error {
		if reg.remove(plan.name) {
			return nil
		}
		return fmt.Errorf("store: rollback track %q: not found", plan.name)
	}

	return rollback, nil
}

func publishValue[R any](app *credo.App, name string, value R, rollbackTrack func() error) error {
	if err := credo.ProvideValue[R](app, value); err != nil {
		mainErr := fmt.Errorf("store: register %q: %w", name, err)
		if rollbackTrack != nil {
			if rollbackErr := rollbackTrack(); rollbackErr != nil {
				mainErr = errors.Join(mainErr, rollbackErr)
			}
		}
		return mainErr
	}
	return nil
}

func wireStoreHealth(app *credo.App, reg *Registry) error {
	fn := internalhealth.StoreFunc(func(ctx context.Context) []internalhealth.StoreResult {
		all := reg.HealthAll(ctx)
		names := make([]string, 0, len(all))
		for name := range all {
			names = append(names, name)
		}
		slices.Sort(names)

		results := make([]internalhealth.StoreResult, 0, len(names))
		for _, name := range names {
			h := all[name]
			results = append(results, internalhealth.StoreResult{
				Name:    name,
				Status:  strings.ToLower(string(h.Status)),
				Latency: h.Latency,
			})
		}
		return results
	})
	// The readiness handler resolves this lazily on each check; providing it
	// here (instead of pushing into the app) keeps the wiring module-internal.
	if err := credo.ProvideValue[internalhealth.StoreFunc](app, fn); err != nil {
		return fmt.Errorf("store: wire health reporting: %w", err)
	}
	return nil
}

// ensureRegistry resolves or creates the store [Registry] in the DI
// container. On first call, a new Registry is created, registered as a
// singleton, and wired into the app's health reporting exactly once.
// The Registry has no Shutdown method, so the container's shutdown pass
// skips it — closing tracked connections is not its job.
func ensureRegistry(app *credo.App) (*Registry, error) {
	reg, err := credo.Resolve[*Registry](app)
	if err == nil {
		return reg, nil
	}

	// First store connection — create and register the registry.
	reg = &Registry{}
	if err := credo.ProvideValue[*Registry](app, reg); err != nil {
		// Race: another goroutine may have registered between our Resolve and ProvideValue.
		resolved, resolveErr := credo.Resolve[*Registry](app)
		if resolveErr == nil {
			return resolved, nil
		}
		return nil, fmt.Errorf("store: register registry: %w", errors.Join(err, resolveErr))
	}
	if err := wireStoreHealth(app, reg); err != nil {
		return nil, err
	}
	return reg, nil
}

// isNilValue reports whether value is a nil pointer, interface, or other nilable type.
func isNilValue[R any](value R) bool {
	v := reflect.ValueOf(&value).Elem()
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
