package store

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/credo-go/credo"
	internalhealth "github.com/credo-go/credo/internal/health"
	"github.com/credo-go/credo/internal/kernel"
)

// DefaultPingTimeout is the default context deadline of the ping the start
// phase sends a registered store. Lifecycle.Ping implementations must honor
// the context; the start phase abandons a Ping that ignores it only at the
// rollback deadline.
const DefaultPingTimeout = 5 * time.Second

// RegisterOption configures a [Register] call.
type RegisterOption func(*registerOptions)

type registerOptions struct {
	name        string
	nameSet     bool
	pingTimeout time.Duration
}

const maxRegistrationWarningCodeLength = 64

// registrationWarningProvider is an optional, deliberately private seam used
// by store implementations to surface low-cardinality, secret-free startup
// diagnostics through the application logger once the store is pinged.
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

// WithPingTimeout overrides the default deadline (5s) of the ping the start
// phase sends the store.
func WithPingTimeout(d time.Duration) RegisterOption {
	return func(o *registerOptions) {
		o.pingTimeout = d
	}
}

// Register adds the binding of R to the App's store registry. The store
// itself is bound where its ownership is decided — app.ProvideValue, or
// app.Provide with a constructor, with credo.Borrowed() for a handle the
// caller shares — and Register names that binding; it performs no I/O.
//
// The start phase resolves R once, after Finalize and so after every
// override, and pings that value as its first start step, before the
// components that depend on it start; a failed ping is a start failure. The
// pinged value is what /ready reports, with its typed [Health], and nothing
// is resolved per readiness request. The binding is a component like any
// other: the App shuts it down after its consumers unless it is borrowed,
// and holders of one resource — a *sqldb.DB bound raw and through a wrapper
// that embeds it — share one teardown.
//
// R may be bound directly or be an interface an Alias names. A registration
// whose R has no binding fails Finalize. Register panics on a nil app, a nil
// option, an invalid name or ping timeout, a type or name registered twice,
// and after Finalize or shutdown.
func Register[R Lifecycle](app *credo.App, opts ...RegisterOption) {
	rType := reflect.TypeFor[R]()
	call := "store.Register[" + rType.String() + "]"
	if app == nil {
		panic("credo: " + call + ": app must not be nil")
	}
	o := registerOptions{pingTimeout: DefaultPingTimeout}
	for _, opt := range opts {
		if opt == nil {
			panic("credo: " + call + ": a nil RegisterOption")
		}
		opt(&o)
	}
	if o.pingTimeout <= 0 {
		panic(fmt.Sprintf("credo: %s: the ping timeout must be > 0, got %s", call, o.pingTimeout))
	}
	name := o.name
	if !o.nameSet {
		name = registerName[R]()
		if name == "" {
			panic(fmt.Sprintf("credo: %s: %s has no stable default name; give it one with store.WithName",
				call, rType))
		}
	}
	if err := internalhealth.ValidateName(name); err != nil {
		panic(fmt.Sprintf("credo: %s: invalid store name: %v", call, err))
	}

	logger := app.Logger()
	timeout := o.pingTimeout
	kernel.RegisterStore(app, kernel.Store{
		Name: name,
		Type: rType,
		Ping: func(ctx context.Context, value any) error {
			lc, _ := value.(R)
			return pingStore(ctx, logger, name, timeout, lc)
		},
		Probe: func(value any) *internalhealth.Probe {
			lc, _ := value.(R)
			return newLifecycleProbe(lc)
		},
	})
}

// pingStore pings a store within timeout, then logs the validated, secret-free
// warning codes its implementation reports.
func pingStore(ctx context.Context, logger *slog.Logger, name string, timeout time.Duration, lc Lifecycle) error {
	if isNilDynamicValue(lc) {
		return fmt.Errorf("store: %q: the bound value is nil", name)
	}
	codes, err := snapshotRegistrationWarningCodes(lc)
	if err != nil {
		return fmt.Errorf("store: %q: %w", name, err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := lc.Ping(pingCtx); err != nil {
		return fmt.Errorf("store: ping %q: %w", name, err)
	}
	for _, code := range codes {
		logger.Warn(
			"credo: store configuration warning",
			"component", "store",
			"store", name,
			"code", code,
		)
	}
	return nil
}

func newLifecycleProbe(lc Lifecycle) *internalhealth.Probe {
	return internalhealth.NewProbe(func(ctx context.Context) internalhealth.Result {
		health := lc.Health(ctx).Clone()
		return internalhealth.Result{
			Status:  string(health.Status),
			Latency: health.Latency,
			Cause:   health.Cause,
		}
	})
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

// isNilDynamicValue reports whether value is a nil pointer, interface, or
// other nilable type.
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
