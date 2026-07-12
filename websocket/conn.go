package websocket

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	coderwebsocket "github.com/coder/websocket"
)

const maxCloseReasonBytes = 123

// Conn is a Credo-owned façade over an accepted WebSocket connection.
//
// Methods may be called concurrently except that only one Read may be active at
// a time. CloseRead claims the read side permanently; Read must not be called
// after it. Concurrent Write calls are serialized by the protocol engine.
type Conn struct {
	conn *coderwebsocket.Conn
	ctx  context.Context
}

func newConn(conn *coderwebsocket.Conn, ctx context.Context, readLimit int64) *Conn {
	conn.SetReadLimit(readLimit)
	return &Conn{conn: conn, ctx: ctx}
}

// Context returns the connection-lifetime context. It is independent of the
// HTTP request cancellation and is cancelled when the adapter finishes the
// connection lifecycle.
func (c *Conn) Context() context.Context {
	return c.ctx
}

// Read reads one text or binary message. Only one Read may be active at a time.
// Peer close errors are normalized to Credo CloseError values.
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	typ, data, err := c.conn.Read(ctx)
	if err != nil {
		return 0, nil, normalizeOperationError("read", err)
	}
	mapped, err := messageTypeFromUpstream(typ)
	if err != nil {
		return 0, nil, err
	}
	return mapped, data, nil
}

// Write writes one complete text or binary message. Concurrent calls are
// supported and serialized by the protocol engine.
func (c *Conn) Write(ctx context.Context, typ MessageType, data []byte) error {
	mapped, err := messageTypeToUpstream(typ)
	if err != nil {
		return err
	}
	return normalizeOperationError("write", c.conn.Write(ctx, mapped, data))
}

// Ping sends a ping and waits for its pong response within ctx.
func (c *Conn) Ping(ctx context.Context) error {
	return normalizeOperationError("ping", c.conn.Ping(ctx))
}

// CloseRead starts processing control frames when the application does not
// expect more data messages. It permanently claims the read side. The returned
// context is cancelled when the upstream reader stops; unexpected data closes
// the connection with StatusPolicyViolation. Its cancellation can be delayed
// by the upstream protocol engine's bounded close guard.
func (c *Conn) CloseRead(ctx context.Context) context.Context {
	return c.conn.CloseRead(ctx)
}

// Close performs the WebSocket close handshake. It rejects codes that a server
// cannot send, invalid UTF-8 reasons, and reasons longer than 123 bytes before
// writing a frame.
func (c *Conn) Close(code StatusCode, reason string) error {
	mapped, err := statusCodeToUpstream(code)
	if err != nil {
		return err
	}
	if !utf8.ValidString(reason) {
		return errors.New("websocket: close reason is not valid UTF-8")
	}
	if len(reason) > maxCloseReasonBytes {
		return fmt.Errorf(
			"websocket: close reason is %d bytes; maximum is %d",
			len(reason),
			maxCloseReasonBytes,
		)
	}
	return normalizeError(c.conn.Close(mapped, reason))
}

// Subprotocol returns the negotiated subprotocol, or an empty string when no
// subprotocol was negotiated.
func (c *Conn) Subprotocol() string {
	return c.conn.Subprotocol()
}

// Unwrap returns the underlying coder/websocket connection as an expert escape
// hatch. Ownership is not transferred: handler-return cleanup, connection
// context cancellation, and lifecycle tracking still apply. Raw calls bypass
// Credo's message validation, error normalization, logging, and close policy;
// the returned connection must not outlive the Handler.
func (c *Conn) Unwrap() *coderwebsocket.Conn {
	return c.conn
}

func messageTypeToUpstream(typ MessageType) (coderwebsocket.MessageType, error) {
	switch typ {
	case MessageText:
		return coderwebsocket.MessageText, nil
	case MessageBinary:
		return coderwebsocket.MessageBinary, nil
	default:
		return 0, fmt.Errorf("websocket: unsupported message type %d", typ)
	}
}

func messageTypeFromUpstream(typ coderwebsocket.MessageType) (MessageType, error) {
	switch typ {
	case coderwebsocket.MessageText:
		return MessageText, nil
	case coderwebsocket.MessageBinary:
		return MessageBinary, nil
	default:
		return 0, fmt.Errorf("websocket: unsupported upstream message type %d", typ)
	}
}

func statusCodeToUpstream(code StatusCode) (coderwebsocket.StatusCode, error) {
	switch code {
	case StatusNormalClosure:
		return coderwebsocket.StatusNormalClosure, nil
	case StatusGoingAway:
		return coderwebsocket.StatusGoingAway, nil
	case StatusProtocolError:
		return coderwebsocket.StatusProtocolError, nil
	case StatusUnsupportedData:
		return coderwebsocket.StatusUnsupportedData, nil
	case StatusInvalidFramePayloadData:
		return coderwebsocket.StatusInvalidFramePayloadData, nil
	case StatusPolicyViolation:
		return coderwebsocket.StatusPolicyViolation, nil
	case StatusMessageTooBig:
		return coderwebsocket.StatusMessageTooBig, nil
	case StatusInternalError:
		return coderwebsocket.StatusInternalError, nil
	case StatusServiceRestart:
		return coderwebsocket.StatusServiceRestart, nil
	case StatusTryAgainLater:
		return coderwebsocket.StatusTryAgainLater, nil
	case StatusBadGateway:
		return coderwebsocket.StatusBadGateway, nil
	default:
		if code >= 3000 && code <= 4999 {
			return coderwebsocket.StatusCode(code), nil
		}
		return 0, fmt.Errorf("websocket: status code %d cannot be sent by a server", code)
	}
}

func normalizeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, coderwebsocket.ErrMessageTooBig) {
		return CloseError{Code: StatusMessageTooBig}
	}
	if closeErr, ok := errors.AsType[coderwebsocket.CloseError](err); ok {
		return CloseError{Code: StatusCode(closeErr.Code), Reason: closeErr.Reason}
	}
	return err
}

type operationError struct {
	operation string
	cause     error
}

func (e *operationError) Error() string {
	return fmt.Sprintf("websocket: %s: %v", e.operation, e.cause)
}

func (e *operationError) Unwrap() error { return e.cause }

func normalizeOperationError(operation string, err error) error {
	err = normalizeError(err)
	if err == nil || CloseStatus(err) >= 0 {
		return err
	}
	return &operationError{operation: operation, cause: err}
}
