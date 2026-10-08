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
	app, err := credo.New()
	if err != nil {
		panic(err)
	}
	ws := credows.New(app.NewInfra("websocket"), credows.Config{
		Subprotocols:       []string{"echo.v1"},
		RequireSubprotocol: true,
	})
	// An ingress component: it starts with the App and drains beside the
	// HTTP drain, before the components its handlers use.
	app.Manage(ws, credo.Ingress())
	app.GET("/echo", ws.Handler(func(_ *credo.Context, conn *credows.Conn) error {
		typ, payload, readErr := conn.Read(conn.Context())
		if readErr != nil {
			return readErr
		}
		return conn.Write(conn.Context(), typ, payload)
	}))

	// Served through httptest, the App is started with Start and shut down
	// after the server it serves.
	if startErr := app.Start(context.Background()); startErr != nil {
		panic(startErr)
	}
	defer func() { _ = app.Shutdown(context.Background()) }()
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
