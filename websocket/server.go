package websocket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/credo-go/credo"
)

type serverState uint8

const (
	serverNew serverState = iota
	serverOpen
	serverDraining
	serverClosed
)

// errNotStarted is the handshake's error for a server whose Start never ran:
// the server was never handed to the App, so nothing would drain it.
var errNotStarted = errors.New("credo/websocket: the server was never started; hand it to the App with " +
	"app.Manage(server), or bind it")

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
// with [New] and handed to the App, whose ingress tier it declares
// ([credo.IngressComponent]).
type Server struct {
	config resolvedConfig
	// logger is the server's own logger, settled by Start: Config.Logger,
	// else the App's logger scoped to the server's component, else the
	// framework default; always with module=websocket. Written under mu
	// before admission opens.
	logger *slog.Logger

	mu           sync.Mutex
	state        serverState
	connections  map[*connectionRecord]struct{}
	activeTokens int
	closeTasks   int
	changed      chan struct{}
	drainStarted bool
	drainDone    chan struct{}
	drainResult  error
	drainErrors  []error
}

// New validates and freezes a WebSocket configuration and returns the server.
// It accepts zero or one Config value, copies its slices defensively, and
// performs no I/O; it registers nothing.
//
// The application hands the server to the App, so it starts with the App
// and drains beside the HTTP drain, before the internal components its
// handlers use; the server declares the ingress tier itself:
//
//	ws := websocket.New(cfg)
//	app.Manage(ws)
//
// or binds it, with app.ProvideValue(ws), when controllers take it as a
// dependency.
// The server logs to [Config.Logger] when it is set and otherwise to the
// App's logger, which Start receives. Invalid configuration or more than
// one Config panics as startup misuse.
func New(cfg ...Config) *Server {
	if len(cfg) > 1 {
		panic("credo/websocket: New accepts at most one Config")
	}
	var value Config
	if len(cfg) == 1 {
		value = cfg[0]
	}
	resolved, err := resolveServerConfig(value)
	if err != nil {
		panic(fmt.Sprintf("credo/websocket: invalid Config: %v", err))
	}
	return &Server{
		config:      resolved,
		connections: make(map[*connectionRecord]struct{}),
		changed:     make(chan struct{}),
		drainDone:   make(chan struct{}),
	}
}

// Start opens admission. The App calls it in the start phase; until it has
// run, the handler refuses every upgrade with an error naming the missing
// registration. Start settles the server's logger — [Config.Logger], else
// the App's logger that ctx carries ([credo.InfraFrom]), else the framework
// default — does no I/O and starts no goroutine; it fails once the server
// has been started or shut down.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case serverNew:
		s.logger = moduleLogger(s.config.logger, credo.InfraFrom(ctx).Logger)
		s.state = serverOpen
		return nil
	case serverOpen:
		return errors.New("credo/websocket: Server.Start: the server is already started")
	default:
		return errors.New("credo/websocket: Server.Start: the server is shut down")
	}
}

// IngressTier declares the server an ingress component: it is where work
// enters the process, so it drains beside the HTTP drain. It is never
// called.
func (*Server) IngressTier() {}

// moduleLogger applies the framework module rule: the module's own logger,
// else the App's, else the framework default, each with module=websocket.
func moduleLogger(own, app *slog.Logger) *slog.Logger {
	logger := own
	if logger == nil {
		logger = app
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return logger.With("module", "websocket")
}

// Shutdown stops admitting connections, sends active peers a Going Away close,
// and waits for every synchronous Handler and adapter cleanup to return. The
// first caller owns the global drain budget. A concurrent caller waits for that
// result unless its own context ends first; it never changes the owner's budget.
// Calls made after the owner finishes return its stable result.
//
// If the owner context is cancelled or reaches its deadline before cleanup
// finishes, Shutdown reports an incomplete drain and the server remains
// draining until late handlers return. If cleanup finishes but a tracked close
// operation failed, the server is closed and Shutdown returns that error.
// A nil ctx is rejected without starting the drain.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("credo/websocket: Server.Shutdown: nil context")
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
	errs := slices.Clone(s.drainErrors)
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

func (s *Server) acquireToken() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case serverOpen:
	case serverNew:
		return errNotStarted
	default:
		return credo.NewHTTPError(http.StatusServiceUnavailable)
	}
	s.activeTokens++
	s.notifyLocked()
	return nil
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
