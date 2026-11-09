package credo

import (
	"context"
	"log/slog"
	"os"
)

// defaultLogger is the framework-owned fallback logger. It writes to stderr
// like slog.Default() but is not affected by slog.SetDefault(), keeping
// framework behavior independent of global mutable state.
// This preserves "observable by default": logs are visible even when
// the user does not call WithLogger.
var defaultLogger = slog.New(slog.NewTextHandler(os.Stderr, nil))

// Infra carries framework infrastructure to services. The container produces
// it automatically when detected as a constructor parameter (Model 1).
//
// Logger is scoped to the application part it serves: a constructor's
// logger carries component=<name>, the name of the binding it builds — its
// type as the lifecycle prints it, such as *users.Service, or its
// [ComponentName] name — the key and value the lifecycle's own records about
// that component use. Framework packages add module=<name> to the records
// they write.
//
// When no logger is configured via [WithLogger], Infra falls back to a
// default stderr logger. Future infrastructure (metrics, tracing) will be
// added as new fields by the observability release.
//
// For testing, construct Infra directly:
//
//	infra := credo.Infra{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}
type Infra struct {
	// Force keyed literals (Infra{Logger: ...}) so that new infrastructure
	// fields can be added without breaking existing constructors.
	_ struct{}

	// Logger is a structured logger, scoped to the component it serves.
	Logger *slog.Logger
}

// NewInfra creates a scoped [Infra] with the given name, for code that runs
// outside a call the App makes on a component (middleware factories,
// startup helpers). The Logger is tagged with component=name and falls back
// to the framework default logger when the application has none configured.
// A constructor receives its Infra automatically, and a component's Start,
// Ready and Shutdown read theirs with [InfraFrom].
func (app *App) NewInfra(name string) Infra {
	return Infra{
		Logger: app.Logger().With("component", name),
	}
}

// infraKey is the context key of the scoped Infra.
type infraKey struct{}

// InfraFrom returns the scoped [Infra] the App puts in the context of every
// call it makes on a component — Start, Ready, Shutdown — and on an OnStart
// or OnStop hook. Its Logger is the App's logger with component=<name>, the
// component's name as its readiness entry, a [LifecycleError] and the
// lifecycle's own records show it. context.WithoutCancel keeps the value,
// so work derived from the Start context sees the same Infra.
//
// It is the path for a framework module whose value is built outside the
// App and started by it: the module logs to its own configured logger when
// one is given, otherwise to InfraFrom(ctx).Logger, otherwise to the
// framework default. Only framework infrastructure travels in the context,
// never application services.
//
// Outside such a call InfraFrom returns the zero Infra, whose nil Logger
// means the framework default. Request code logs through the request's
// [Context.Logger], and a constructor takes Infra as a parameter.
func InfraFrom(ctx context.Context) Infra {
	if ctx == nil {
		return Infra{}
	}
	infra, _ := ctx.Value(infraKey{}).(Infra)
	return infra
}

// withComponentInfra returns ctx carrying the Infra of the component name.
func (app *App) withComponentInfra(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, infraKey{}, app.NewInfra(name))
}

// newInfra creates a fully initialized Infra with the given base logger.
// A nil logger is replaced with the framework default.
func newInfra(logger *slog.Logger) Infra {
	if logger == nil {
		logger = defaultLogger
	}
	return Infra{Logger: logger}
}
