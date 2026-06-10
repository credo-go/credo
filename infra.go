package credo

import (
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
// Logger is scoped per service: each service receives a logger with
// a "service" attribute set to the service type name.
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

	// Logger is a structured logger, scoped to the service.
	Logger *slog.Logger
}

// NewInfra creates a scoped [Infra] with the given name.
// The Logger is tagged with "service"=name and falls back to the
// framework default logger when the application has none configured.
//
// Use NewInfra for components that live outside the DI container
// (middleware factories, startup helpers, workers created manually).
// For DI-managed services, Infra is injected automatically.
func (app *App) NewInfra(name string) Infra {
	return Infra{
		Logger: app.Logger().With("service", name),
	}
}

// newInfra creates a fully initialized Infra with the given base logger.
// A nil logger is replaced with the framework default.
func newInfra(logger *slog.Logger) Infra {
	if logger == nil {
		logger = defaultLogger
	}
	return Infra{Logger: logger}
}
