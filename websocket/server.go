package websocket

import (
	"context"
	"fmt"

	"github.com/credo-go/credo"
)

// Server owns the immutable policy and managed lifecycle state for WebSocket
// handlers registered through one Credo application.
type Server struct {
	config resolvedConfig
}

// Use validates and freezes a WebSocket configuration for app. It accepts zero
// or one Config value and performs no I/O. Invalid configuration, a nil app, or
// registration after the app is frozen panics as startup misuse.
func Use(app *credo.App, cfg ...Config) *Server {
	if app == nil {
		panic("credo/websocket: Use called with a nil App")
	}
	if len(cfg) > 1 {
		panic("credo/websocket: Use accepts at most one Config")
	}
	var value Config
	if len(cfg) == 1 {
		value = cfg[0]
	}
	resolved, err := resolveConfig(value)
	if err != nil {
		panic(fmt.Sprintf("credo/websocket: invalid Config: %v", err))
	}
	server := &Server{config: resolved}
	// Register only after every mechanical validation succeeds so an invalid
	// configuration cannot leave a partial lifecycle mutation behind.
	app.OnDrain(server.onDrain)
	return server
}

func (s *Server) onDrain(context.Context) error {
	return nil
}
