package websocket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/credo-go/credo"
)

type serverState uint8

const (
	serverOpen serverState = iota
	serverDraining
	serverClosed
)

type connectionRecord struct {
	conn             *Conn
	close            func(StatusCode, string) error
	closeNow         func() error
	cancel           context.CancelCauseFunc
	logger           *slog.Logger
	connectionID     string
	requestID        string
	route            string
	subprotocol      string
	startedAt        time.Time
	closeDone        chan struct{}
	terminal         bool
	terminalCause    error
	shutdownTerminal bool
	classification   string
	closeCode        StatusCode
}

// Server owns the immutable policy and managed lifecycle state for WebSocket
// handlers registered through one Credo application. A Server must be created
// with [Use].
type Server struct {
	config resolvedConfig
	logger *slog.Logger

	mu           sync.Mutex
	state        serverState
	managedCtx   context.Context
	connections  map[*connectionRecord]struct{}
	activeTokens int
	closeTasks   int
	changed      chan struct{}
	drainStarted bool
	drainDone    chan struct{}
	drainResult  error
	drainErrors  []error
}

// Use validates and freezes a WebSocket configuration for app. It accepts zero
// or one Config value and performs no I/O or DI publication. Invalid
// configuration, a nil app, or registration after the app is frozen panics as
// startup misuse.
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
	server := &Server{
		config:      resolved,
		logger:      app.Logger().With("module", "websocket"),
		connections: make(map[*connectionRecord]struct{}),
		changed:     make(chan struct{}),
		drainDone:   make(chan struct{}),
	}
	// Register only after every mechanical validation succeeds so an invalid
	// configuration cannot leave a partial lifecycle mutation behind.
	app.OnStart(server.onStart)
	app.OnDrain(server.onDrain)
	return server
}

func (s *Server) onStart(ctx context.Context) error {
	s.mu.Lock()
	s.managedCtx = ctx
	s.mu.Unlock()
	return nil
}

func (s *Server) onDrain(ctx context.Context) error {
	return s.Shutdown(ctx)
}

// Shutdown stops admitting connections, sends active peers a Going Away close,
// and waits for every synchronous Handler and adapter cleanup to return. The
// first caller owns the global drain budget; concurrent callers only wait and
// do not alter that budget. Repeated calls return the first owner's stable
// result. A deadline can leave the server draining until late handlers return.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		panic("credo/websocket: Server.Shutdown called with a nil context")
	}
	s.mu.Lock()
	if s.drainStarted {
		done := s.drainDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			result := s.drainResult
			s.mu.Unlock()
			return result
		default:
		}
		select {
		case <-done:
			s.mu.Lock()
			result := s.drainResult
			s.mu.Unlock()
			return result
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.drainStarted = true
	s.state = serverDraining
	for record := range s.connections {
		s.startTerminalLocked(record, StatusGoingAway, "", context.Canceled, true, "shutdown")
	}
	s.notifyLocked()
	s.mu.Unlock()

	result := s.ownDrain(ctx)
	s.mu.Lock()
	s.drainResult = result
	close(s.drainDone)
	s.mu.Unlock()
	return result
}

func (s *Server) ownDrain(ctx context.Context) error {
	for {
		s.mu.Lock()
		if s.activeTokens == 0 && len(s.connections) == 0 && s.closeTasks == 0 {
			s.state = serverClosed
			result := errors.Join(s.drainErrors...)
			s.mu.Unlock()
			return result
		}
		changed := s.changed
		s.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return s.forceIncomplete(ctx.Err())
		}
	}
}

func (s *Server) forceIncomplete(cause error) error {
	s.mu.Lock()
	remainingHandlers := s.activeTokens
	remainingConnections := len(s.connections)
	remainingCloseTasks := s.closeTasks
	for record := range s.connections {
		cancelCause := record.terminalCause
		if record.shutdownTerminal || cancelCause == nil {
			cancelCause = cause
		}
		record.cancel(cancelCause)
		go func(closeNow func() error) { _ = closeNow() }(record.closeNow)
	}
	errs := append([]error(nil), s.drainErrors...)
	s.mu.Unlock()
	incomplete := &shutdownIncompleteError{
		cause:                cause,
		remainingHandlers:    remainingHandlers,
		remainingConnections: remainingConnections,
		remainingCloseTasks:  remainingCloseTasks,
	}
	if s.logger != nil {
		s.logger.LogAttrs(
			context.Background(),
			slog.LevelError,
			"websocket: shutdown incomplete",
			slog.String("classification", "shutdown_incomplete"),
			slog.Int("remaining_handlers", remainingHandlers),
			slog.Int("remaining_connections", remainingConnections),
			slog.Int("remaining_close_tasks", remainingCloseTasks),
		)
	}
	return errors.Join(append(errs, incomplete)...)
}

type shutdownIncompleteError struct {
	cause                error
	remainingHandlers    int
	remainingConnections int
	remainingCloseTasks  int
}

func (e *shutdownIncompleteError) Error() string {
	return fmt.Sprintf(
		"websocket: shutdown incomplete: handlers=%d connections=%d close_tasks=%d: %v",
		e.remainingHandlers,
		e.remainingConnections,
		e.remainingCloseTasks,
		e.cause,
	)
}

func (e *shutdownIncompleteError) Unwrap() error { return e.cause }

func (s *Server) acquireToken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != serverOpen {
		return false
	}
	s.activeTokens++
	s.notifyLocked()
	return true
}

func (s *Server) attach(record *connectionRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connections[record] = struct{}{}
	open := s.state == serverOpen
	if !open {
		s.startTerminalLocked(record, StatusGoingAway, "", context.Canceled, true, "shutdown")
	}
	s.notifyLocked()
	return open
}

func (s *Server) finish(record *connectionRecord) {
	<-record.closeDone
	s.mu.Lock()
	delete(s.connections, record)
	s.activeTokens--
	if s.state == serverDraining && s.activeTokens == 0 && len(s.connections) == 0 && s.closeTasks == 0 {
		s.state = serverClosed
	}
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *Server) releaseToken() {
	s.mu.Lock()
	s.activeTokens--
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *Server) startTerminal(
	record *connectionRecord,
	code StatusCode,
	reason string,
	cause error,
	shutdown bool,
	classification string,
) {
	s.mu.Lock()
	s.startTerminalLocked(record, code, reason, cause, shutdown, classification)
	s.mu.Unlock()
}

func (s *Server) startTerminalLocked(
	record *connectionRecord,
	code StatusCode,
	reason string,
	cause error,
	shutdown bool,
	classification string,
) {
	if record.terminal {
		return
	}
	record.terminal = true
	record.terminalCause = cause
	record.shutdownTerminal = shutdown
	record.classification = classification
	record.closeCode = code
	s.closeTasks++
	s.notifyLocked()
	go func() {
		var err error
		if code >= 0 {
			err = record.close(code, reason)
		}
		terminalCause := cause
		if shutdown && err != nil {
			terminalCause = err
		}
		if terminalCause == nil {
			terminalCause = context.Canceled
		}
		record.cancel(terminalCause)

		s.mu.Lock()
		if shutdown && err != nil {
			s.drainErrors = append(s.drainErrors, fmt.Errorf(
				"websocket: close connection %s: %w", record.connectionID, err,
			))
		}
		s.closeTasks--
		close(record.closeDone)
		s.notifyLocked()
		s.mu.Unlock()
		if shutdown && err != nil {
			logConnectionFailure(record, "shutdown_close_error", err)
		}
	}()
}

func (s *Server) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}
