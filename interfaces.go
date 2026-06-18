package credo

import (
	"context"

	"github.com/credo-go/credo/config"
)

// RawConfig is an alias for [config.RawConfig].
// The interface is defined in the config package to avoid circular imports
// between the root package and the config package. This alias ensures that
// credo.RawConfig continues to work seamlessly throughout the framework.
//
// RawConfig provides low-level access to the merged configuration.
// This is a bootstrap mechanism — application code should use typed config
// structs injected via DI instead of calling RawConfig directly.
//
// Unmarshal decodes both struct sections and primitive values:
//
//	var port int
//	rawCfg.Unmarshal("server.port", &port)
//
//	var dbCfg DatabaseConfig
//	rawCfg.Unmarshal("databases.default", &dbCfg)
type RawConfig = config.RawConfig

// Shutdowner is implemented by services that need cleanup on shutdown.
// The context carries a deadline from the application's graceful shutdown
// timeout; implementations should respect ctx.Done() for timely cleanup.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}
