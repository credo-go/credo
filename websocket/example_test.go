package websocket_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"

	coderwebsocket "github.com/coder/websocket"

	"github.com/credo-go/credo"
	credows "github.com/credo-go/credo/websocket"
)

func Example() {
	app, err := credo.New(credo.WithoutAccessLog())
	if err != nil {
		panic(err)
	}
	ws := credows.Use(app, credows.Config{
		Subprotocols:       []string{"echo.v1"},
		RequireSubprotocol: true,
	})
	app.GET("/echo", ws.Handler(func(_ *credo.Context, conn *credows.Conn) error {
		typ, payload, readErr := conn.Read(conn.Context())
		if readErr != nil {
			return readErr
		}
		return conn.Write(conn.Context(), typ, payload)
	}))

	httpServer := httptest.NewServer(app)
	defer httpServer.Close()
	client, _, err := coderwebsocket.Dial( //nolint:bodyclose // Dial owns the response body.
		context.Background(),
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/echo",
		&coderwebsocket.DialOptions{Subprotocols: []string{"echo.v1"}},
	)
	if err != nil {
		panic(err)
	}
	defer func() { _ = client.CloseNow() }()

	if writeErr := client.Write(context.Background(), coderwebsocket.MessageText, []byte("hello")); writeErr != nil {
		panic(writeErr)
	}
	_, payload, err := client.Read(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Println(string(payload))

	// Output: hello
}
