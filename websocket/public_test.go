package websocket_test

import (
	"context"

	"github.com/coder/websocket"

	"github.com/credo-go/credo"
	credows "github.com/credo-go/credo/websocket"
)

type publicConnSurface interface {
	Context() context.Context
	Read(context.Context) (credows.MessageType, []byte, error)
	Write(context.Context, credows.MessageType, []byte) error
	Ping(context.Context) error
	CloseRead(context.Context) context.Context
	Close(credows.StatusCode, string) error
	Subprotocol() string
	Unwrap() *websocket.Conn
}

type publicServerSurface interface {
	Handler(credows.Handler) credo.Handler
	Shutdown(context.Context) error
}

var (
	_ publicConnSurface                                   = (*credows.Conn)(nil)
	_ publicServerSurface                                 = (*credows.Server)(nil)
	_ error                                               = credows.CloseError{}
	_ credows.Handler                                     = func(*credo.Context, *credows.Conn) error { return nil }
	_ func(*credo.App, ...credows.Config) *credows.Server = credows.Use
)

func ExampleConfig() {
	_ = credows.Config{
		AllowedOrigins:     []string{"https://app.example.com"},
		Subprotocols:       []string{"events.v1"},
		RequireSubprotocol: true,
		ReadLimit:          1 << 20,
		CompressionMode:    credows.CompressionNoContextTakeover,
	}
}
