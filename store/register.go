package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/credo-go/credo"
	internalhealth "github.com/credo-go/credo/internal/health"
)

// DefaultPingTimeout is the default context deadline for the initial health
// check performed by [Register]. Lifecycle.Ping implementations must honor the
// context; Register does not detach a non-cooperative Ping call.
const DefaultPingTimeout = 5 * time.Second

// RegisterOption configures a [Register] call.
type RegisterOption func(*registerOptions)

type registerOptions struct {
	name         string
	nameSet      bool
	pingTimeout  time.Duration
	lifecycle    Lifecycle
	lifecycleSet bool
	callerOwned  bool
}

type registerPlan struct {
	name              string
	pingTimeout       time.Duration
	lifecycle         Lifecycle
	lifecycleIdentity lifecycleIdentity
	warningCodes      []string
}

const maxRegistrationWarningCodeLength = 64

// registrationWarningProvider is an optional, deliberately private seam used
// by store implementations to surface low-cardinality, secret-free startup
// diagnostics through Register's application logger.
type registrationWarningProvider interface {
	StoreRegistrationWarningCodes() []string
}

// WithName sets the stable identifier used in health reporting. It rejects an
// empty or padded name, control characters, and the reserved "credo." prefix.
// If omitted, Register uses the pointer-unwrapped, package-qualified name of R.
func WithName(name string) RegisterOption {
	return func(o *registerOptions) {
		o.name = name
		o.nameSet = true
	}
}

// WithPingTimeout overrides the default Ping context deadline (5s) for the
// initial health check performed by [Register].
func WithPingTimeout(d time.Duration) RegisterOption {
	return func(o *registerOptions) {
		o.pingTimeout = d
	}
}

// WithLifecycle provides the handle used for Ping and Health when value does
// not implement Lifecycle itself. It must be paired with
// [WithCallerOwnedLifecycle]: the caller retains responsibility for invoking
// Shutdown on that separate handle. Prefer making value implement Lifecycle so
// the framework can own shutdown.
func WithLifecycle(lc Lifecycle) RegisterOption {
	return func(o *registerOptions) {
		o.lifecycle = lc
		o.lifecycleSet = true
	}
}

// WithCallerOwnedLifecycle explicitly keeps lifecycle shutdown ownership with
// the caller when [WithLifecycle] is used for a value that cannot itself
// implement Lifecycle. The caller must arrange shutdown, for example through
// [credo.App.OnShutdown]. The option is invalid without [WithLifecycle].
func WithCallerOwnedLifecycle() RegisterOption {
	return func(o *registerOptions) {
		o.callerOwned = true
	}
}

// Register registers value as type R in the DI container, pings the
// connection, and tracks it in the [Registry] for lifecycle and health
// management.
//
// If value implements [Lifecycle], it is used directly for Ping, Health, and
// Shutdown. Otherwise, a separate health handle is accepted only through
// [WithLifecycle] plus the explicit [WithCallerOwnedLifecycle] opt-out.
//
// Steps:
//  1. Validate name, lifecycle ownership, and predictable DI conflicts
//  2. Resolve or create Registry and wire the internal readiness seam
//  3. Privately reserve the unique store name, DI type, and lifecycle identity
//  4. Ping the connection with a finite timeout
//  5. Publish the DI value and Registry health entry together
//  6. Emit validated, secret-free registration warning codes after publication
//
// Shutdown ownership is unambiguous. A direct Lifecycle value is
// framework-owned after successful registration. The DI container visits it in
// reverse registration order and makes at most one shutdown attempt if the
// live shutdown deadline reaches its entry. A separate WithLifecycle handle
// remains caller-owned and requires WithCallerOwnedLifecycle; the caller must
// arrange its shutdown. The Registry never closes connections.
//
// On every error, including Ping or final DI publication failure, Register
// exposes no health entry and does not acquire ownership. This does not undo an
// independent raw DI publication performed by the caller or another goroutine.
// Do not place the same lifecycle in DI through Provide, ProvideFactory,
// ProvideValue, ProvideProtectedValue, or Replace and also through Register;
// register it once and use [credo.App.Alias] for additional interface views.
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

	// Reject predictable local DI failures before creating infrastructure or
	// performing network I/O. This is a point-in-time preflight; ProvideValue
	// remains authoritative against external concurrent mutations.
	if preflightErr := app.CanProvideValue[R](); preflightErr != nil {
		return fmt.Errorf("store: register %q: %w", plan.name, preflightErr)
	}

	reg, err := ensureRegistry(app)
	if err != nil {
		return err
	}

	reservation, err := reg.reserveIdentified(
		plan.name,
		reflect.TypeFor[R](),
		plan.lifecycle,
		plan.lifecycleIdentity,
	)
	if err != nil {
		return fmt.Errorf("store: reserve %q: %w", plan.name, err)
	}
	defer reservation.release()

	// Re-check after Registry/seam setup so finalization or a concurrent value
	// registration that won during setup is still caught before Ping.
	if err := app.CanProvideValue[R](); err != nil {
		return fmt.Errorf("store: register %q: %w", plan.name, err)
	}

	if pingErr := pingLifecycle(plan); pingErr != nil {
		return pingErr
	}

	if err := reservation.commit(func() error {
		return app.ProvideProtectedValue[R](value)
	}); err != nil {
		return fmt.Errorf("store: register %q: %w", plan.name, err)
	}
	for _, code := range plan.warningCodes {
		app.Logger().Warn(
			"credo: store configuration warning",
			"component", "store",
			"store", plan.name,
			"code", code,
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
	if !o.nameSet {
		name = registerName[R]()
		if name == "" {
			return registerPlan{}, fmt.Errorf(
				"store: type %s has no stable default registration name; provide WithName", reflect.TypeFor[R](),
			)
		}
	}
	if err := internalhealth.ValidateName(name); err != nil {
		return registerPlan{}, fmt.Errorf("store: invalid registration name: %w", err)
	}

	valueLifecycle, implementsLifecycle := any(value).(Lifecycle)
	if implementsLifecycle && isNilDynamicValue(valueLifecycle) {
		implementsLifecycle = false
	}
	_, implementsShutdowner := any(value).(credo.Shutdowner)

	var lc Lifecycle
	switch {
	case implementsLifecycle:
		if o.lifecycleSet {
			return registerPlan{}, fmt.Errorf(
				"store: %q implements Lifecycle; WithLifecycle would split or duplicate ownership", name,
			)
		}
		if o.callerOwned {
			return registerPlan{}, fmt.Errorf(
				"store: %q implements Lifecycle and is framework-owned; caller-owned opt-out is not supported", name,
			)
		}
		lc = valueLifecycle
	case o.lifecycleSet:
		if isNilDynamicValue(o.lifecycle) {
			return registerPlan{}, fmt.Errorf("store: %q WithLifecycle value must not be nil", name)
		}
		if implementsShutdowner {
			return registerPlan{}, fmt.Errorf(
				"store: %q implements credo.Shutdowner but not Lifecycle; Ping/Health and Shutdown cannot use different objects",
				name,
			)
		}
		if !o.callerOwned {
			return registerPlan{}, fmt.Errorf(
				"store: %q WithLifecycle requires explicit WithCallerOwnedLifecycle", name,
			)
		}
		lc = o.lifecycle
	case o.callerOwned:
		return registerPlan{}, fmt.Errorf("store: %q WithCallerOwnedLifecycle requires WithLifecycle", name)
	default:
		return registerPlan{}, fmt.Errorf("store: %q does not implement Lifecycle", name)
	}

	warningCodes, err := snapshotRegistrationWarningCodes(lc)
	if err != nil {
		return registerPlan{}, err
	}

	identity, err := identifyLifecycle(lc)
	if err != nil {
		return registerPlan{}, fmt.Errorf("store: lifecycle identity for %q: %w", name, err)
	}

	return registerPlan{
		name:              name,
		pingTimeout:       o.pingTimeout,
		lifecycle:         lc,
		lifecycleIdentity: identity,
		warningCodes:      warningCodes,
	}, nil
}

func snapshotRegistrationWarningCodes(value any) ([]string, error) {
	provider, ok := value.(registrationWarningProvider)
	if !ok {
		return nil, nil
	}

	codes := provider.StoreRegistrationWarningCodes()
	result := make([]string, 0, len(codes))
	seen := make(map[string]struct{}, len(codes))
	for i, code := range codes {
		if !validRegistrationWarningCode(code) {
			// Never include the provider value in this error: an invalid code may
			// accidentally contain a DSN, credential, or another secret.
			return nil, fmt.Errorf("store: registration warning code at index %d is invalid", i)
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		result = append(result, code)
	}
	return result, nil
}

func validRegistrationWarningCode(code string) bool {
	if code == "" || len(code) > maxRegistrationWarningCodeLength {
		return false
	}
	for i := range len(code) {
		c := code[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func registerName[R any]() string {
	rType := reflect.TypeFor[R]()
	for rType.Kind() == reflect.Pointer {
		rType = rType.Elem()
	}
	if rType.Name() == "" {
		return ""
	}
	return rType.String()
}

func pingLifecycle(plan registerPlan) error {
	pingCtx, cancel := context.WithTimeout(context.Background(), plan.pingTimeout)
	defer cancel()

	if err := plan.lifecycle.Ping(pingCtx); err != nil {
		return fmt.Errorf("store: ping %q: %w", plan.name, err)
	}
	return nil
}

func wireStoreHealth(app *credo.App, reg *Registry) error {
	if reg == nil {
		return fmt.Errorf("store: wire health reporting: registry must not be nil")
	}
	fn := internalhealth.StoreFunc(func() []internalhealth.StoreCheck {
		return reg.storeChecks()
	})
	// Replace is intentional: a composition root may have supplied Registry
	// before the first Register call, and a previous publish attempt may have
	// installed an obsolete or partial seam. Re-establishing this internal
	// value is idempotent and keeps readiness bound to the resolved Registry.
	if err := app.Replace[internalhealth.StoreFunc](fn); err != nil {
		return fmt.Errorf("store: wire health reporting: %w", err)
	}
	return nil
}

// ensureRegistry resolves or creates the store [Registry] in the DI
// container. Its binding is protected before use so DI and the readiness seam
// cannot later diverge through Replace. The internal health seam is
// idempotently re-established for both new and pre-provided registries, so an
// interrupted wiring attempt is retryable on the next Register call.
// The Registry has no Shutdown method, so the container's shutdown pass
// skips it — closing tracked connections is not its job.
func ensureRegistry(app *credo.App) (*Registry, error) {
	// Validate a pre-provided Registry before making its binding permanent.
	// Expected-value ProtectBinding atomically rejects a concurrent Replace
	// winner. A second Resolve then supplies the exact protected instance to the
	// readiness seam.
	if reg, err := app.Resolve[*Registry](); err == nil {
		if reg == nil {
			return nil, fmt.Errorf("store: resolved Registry must not be nil")
		}
		if err := app.ProtectBinding[*Registry](reg); err != nil {
			return nil, fmt.Errorf("store: protect Registry binding: %w", err)
		}
		return resolveAndWireRegistry(app)
	}

	// First store connection — create and register the registry.
	reg := &Registry{}
	if err := app.ProvideProtectedValue[*Registry](reg); err != nil {
		// Race or pre-provided constructor: adopt only a successfully resolved,
		// non-nil Registry, then protect and re-resolve it before wiring.
		resolved, resolveErr := app.Resolve[*Registry]()
		if resolveErr != nil || resolved == nil {
			if resolveErr == nil {
				resolveErr = fmt.Errorf("store: resolved Registry must not be nil")
			}
			return nil, fmt.Errorf("store: register registry: %w", errors.Join(err, resolveErr))
		}
		if protectErr := app.ProtectBinding[*Registry](resolved); protectErr != nil {
			return nil, fmt.Errorf("store: register registry: %w", errors.Join(err, protectErr))
		}
		return resolveAndWireRegistry(app)
	}
	if err := wireStoreHealth(app, reg); err != nil {
		return nil, err
	}
	return reg, nil
}

func resolveAndWireRegistry(app *credo.App) (*Registry, error) {
	reg, err := app.Resolve[*Registry]()
	if err != nil {
		return nil, fmt.Errorf("store: resolve protected Registry: %w", err)
	}
	if reg == nil {
		return nil, fmt.Errorf("store: resolved Registry must not be nil")
	}
	if err := wireStoreHealth(app, reg); err != nil {
		return nil, err
	}
	return reg, nil
}

// isNilValue reports whether value is a nil pointer, interface, or other nilable type.
func isNilValue[R any](value R) bool {
	return isNilDynamicValue(any(value))
}

func isNilDynamicValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	for v.Kind() == reflect.Interface {
		if v.IsNil() {
			return true
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice,
		reflect.UnsafePointer:
		return v.IsNil()
	default:
		return false
	}
}
