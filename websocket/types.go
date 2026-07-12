package websocket

import (
	"errors"
	"fmt"

	"github.com/credo-go/credo"
)

// Handler handles an accepted WebSocket connection. The handler runs
// synchronously for the connection lifetime; req and conn must not be retained
// after it returns.
type Handler func(req *credo.Context, conn *Conn) error

// MessageType identifies a WebSocket data message type.
type MessageType int

const (
	// MessageText identifies a UTF-8 text message.
	MessageText MessageType = iota + 1
	// MessageBinary identifies a binary message.
	MessageBinary
)

// CompressionMode controls per-message deflate negotiation.
type CompressionMode int

const (
	// CompressionDisabled disables per-message compression. This is the default.
	CompressionDisabled CompressionMode = iota
	// CompressionNoContextTakeover resets compression state between messages.
	CompressionNoContextTakeover
	// CompressionContextTakeover reuses compression state between messages.
	CompressionContextTakeover
)

// StatusCode is a WebSocket close status code.
type StatusCode int

const (
	// StatusNormalClosure indicates that the connection fulfilled its purpose.
	StatusNormalClosure StatusCode = 1000
	// StatusGoingAway indicates that an endpoint is going away.
	StatusGoingAway StatusCode = 1001
	// StatusProtocolError indicates a protocol error.
	StatusProtocolError StatusCode = 1002
	// StatusUnsupportedData indicates an unsupported data type.
	StatusUnsupportedData StatusCode = 1003
	// StatusNoStatusReceived represents a close frame without a status code.
	// It cannot be sent on the wire.
	StatusNoStatusReceived StatusCode = 1005
	// StatusAbnormalClosure represents an abnormal close without a close frame.
	// It cannot be sent on the wire.
	StatusAbnormalClosure StatusCode = 1006
	// StatusInvalidFramePayloadData indicates invalid frame payload data.
	StatusInvalidFramePayloadData StatusCode = 1007
	// StatusPolicyViolation indicates an application policy violation.
	StatusPolicyViolation StatusCode = 1008
	// StatusMessageTooBig indicates that a message exceeded an endpoint limit.
	StatusMessageTooBig StatusCode = 1009
	// StatusMandatoryExtension indicates a client-required extension was absent.
	// A server cannot send this status.
	StatusMandatoryExtension StatusCode = 1010
	// StatusInternalError indicates an unexpected server condition.
	StatusInternalError StatusCode = 1011
	// StatusServiceRestart indicates that the service is restarting.
	StatusServiceRestart StatusCode = 1012
	// StatusTryAgainLater indicates a temporary server condition.
	StatusTryAgainLater StatusCode = 1013
	// StatusBadGateway indicates an invalid response from an upstream service.
	StatusBadGateway StatusCode = 1014
	// StatusTLSHandshake represents a TLS handshake failure.
	// It cannot be sent on the wire.
	StatusTLSHandshake StatusCode = 1015
)

// CloseError reports a WebSocket close status received from the peer. Reason is
// untrusted peer input and must not be logged or displayed without sanitizing.
// CloseError deliberately does not unwrap an upstream implementation error.
type CloseError struct {
	Code   StatusCode
	Reason string
}

// Error returns a human-readable close diagnostic. Its text is not a parsing
// contract; use Code, Reason, or CloseStatus for programmatic inspection.
func (e CloseError) Error() string {
	return fmt.Sprintf("websocket: closed with status %d and reason %q", e.Code, e.Reason)
}

// CloseStatus returns the first CloseError status found in err's wrapped or
// joined error tree. It accepts both CloseError values and pointers and returns
// StatusCode(-1) for nil or non-close errors.
func CloseStatus(err error) StatusCode {
	code, ok := findCloseStatus(err)
	if !ok {
		return StatusCode(-1)
	}
	return code
}

func findCloseStatus(err error) (StatusCode, bool) {
	if err == nil {
		return 0, false
	}
	switch closeErr := err.(type) {
	case CloseError:
		return closeErr.Code, true
	case *CloseError:
		if closeErr != nil {
			return closeErr.Code, true
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if code, found := findCloseStatus(child); found {
				return code, true
			}
		}
		return 0, false
	}
	return findCloseStatus(errors.Unwrap(err))
}
