package websocket

import (
	"context"
	"net/http"
	"testing"

	coderwebsocket "github.com/coder/websocket"
)

// These declarations intentionally pin the public coder/websocket surface that
// Credo's adapter plans to wrap. A dependency update that changes a required
// signature must fail at compile time before it can reach the adapter.
type acceptFunc func(
	http.ResponseWriter,
	*http.Request,
	*coderwebsocket.AcceptOptions,
) (*coderwebsocket.Conn, error)

type requiredConn interface {
	Read(context.Context) (coderwebsocket.MessageType, []byte, error)
	Write(context.Context, coderwebsocket.MessageType, []byte) error
	Ping(context.Context) error
	CloseRead(context.Context) context.Context
	Close(coderwebsocket.StatusCode, string) error
	CloseNow() error
	Subprotocol() string
	SetReadLimit(int64)
}

var (
	_ acceptFunc   = coderwebsocket.Accept
	_ requiredConn = (*coderwebsocket.Conn)(nil)
)

func TestCoderWebSocketPublicSurface(t *testing.T) {
	t.Parallel()

	opts := coderwebsocket.AcceptOptions{
		Subprotocols:         []string{"credo.conformance.v1"},
		InsecureSkipVerify:   true,
		CompressionMode:      coderwebsocket.CompressionNoContextTakeover,
		CompressionThreshold: 512,
	}
	if opts.CompressionMode == coderwebsocket.CompressionDisabled {
		t.Fatal("compression mode was not retained")
	}
	if coderwebsocket.CompressionContextTakeover == opts.CompressionMode {
		t.Fatal("compression modes are not distinct")
	}
	if coderwebsocket.MessageText == coderwebsocket.MessageBinary {
		t.Fatal("message types are not distinct")
	}

	closeErr := coderwebsocket.CloseError{
		Code:   coderwebsocket.StatusNormalClosure,
		Reason: "conformance",
	}
	if got := coderwebsocket.CloseStatus(closeErr); got != closeErr.Code {
		t.Fatalf("CloseStatus() = %d, want %d", got, closeErr.Code)
	}
}
