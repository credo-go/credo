package websocket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coderwebsocket "github.com/coder/websocket"
)

type connTestContextKey struct{}

type connTestPair struct {
	server    *Conn
	client    *coderwebsocket.Conn
	serverCtx context.Context
}

func openConnTestPair(
	t *testing.T,
	cfg Config,
	dialOptions *coderwebsocket.DialOptions,
) connTestPair {
	t.Helper()
	resolved, err := resolveConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	serverConn := make(chan *Conn, 1)
	acceptErr := make(chan error, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var serverCtx context.Context

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := coderwebsocket.Accept(w, r, resolved.acceptOptions())
		if err != nil {
			acceptErr <- err
			return
		}
		ctx, cancel := context.WithCancelCause(
			context.WithValue(context.Background(), connTestContextKey{}, "connection-value"),
		)
		serverCtx = ctx
		serverConn <- newConn(raw, ctx, resolved.readLimit)
		<-release
		cancel(context.Canceled)
		_ = raw.CloseNow()
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		httpServer.Close()
	})

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	client, _, err := coderwebsocket.Dial(t.Context(), wsURL, dialOptions)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })

	select {
	case conn := <-serverConn:
		return connTestPair{server: conn, client: client, serverCtx: serverCtx}
	case err := <-acceptErr:
		t.Fatalf("Accept() error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for accepted connection")
	}
	return connTestPair{}
}

func TestConnFacadeRoundTripContextAndSubprotocol(t *testing.T) {
	pair := openConnTestPair(t, Config{
		Subprotocols:       []string{"echo.v1"},
		RequireSubprotocol: true,
	}, &coderwebsocket.DialOptions{Subprotocols: []string{"other", "echo.v1"}})

	if pair.server.Context() != pair.serverCtx {
		t.Fatal("Context() did not return the connection context")
	}
	if got := pair.server.Context().Value(connTestContextKey{}); got != "connection-value" {
		t.Errorf("connection context value = %v", got)
	}
	if pair.server.Unwrap() == nil || pair.server.Unwrap() != pair.server.conn {
		t.Fatal("Unwrap() did not return the borrowed upstream connection")
	}
	if pair.server.Subprotocol() != "echo.v1" {
		t.Errorf("Subprotocol() = %q, want echo.v1", pair.server.Subprotocol())
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := pair.client.Write(ctx, coderwebsocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := pair.server.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != MessageText || string(data) != "hello" {
		t.Errorf("server Read() = (%d, %q), want text hello", typ, data)
	}
	if err := pair.server.Write(ctx, MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	clientType, clientData, err := pair.client.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if clientType != coderwebsocket.MessageBinary || string(clientData) != string([]byte{1, 2, 3}) {
		t.Errorf("client Read() = (%d, %v)", clientType, clientData)
	}
}

func TestConnFacadeConcurrentWrites(t *testing.T) {
	pair := openConnTestPair(t, Config{}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	const writers = 12
	writeErrs := make(chan error, writers)
	for i := range writers {
		go func() {
			payload := fmt.Sprintf("message-%02d", i)
			writeErrs <- pair.server.Write(ctx, MessageText, []byte(payload))
		}()
	}
	received := make(map[string]bool, writers)
	for range writers {
		typ, data, err := pair.client.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ != coderwebsocket.MessageText {
			t.Fatalf("message type = %d, want text", typ)
		}
		received[string(data)] = true
	}
	for range writers {
		if err := <-writeErrs; err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != writers {
		t.Errorf("received %d unique messages, want %d", len(received), writers)
	}
}

func TestConnFacadePing(t *testing.T) {
	pair := openConnTestPair(t, Config{}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pair.client.CloseRead(ctx)
	pair.server.CloseRead(ctx)
	if err := pair.server.Ping(ctx); err != nil {
		t.Fatalf("Ping() error: %v", err)
	}
	if err := pair.server.Close(StatusNormalClosure, ""); err != nil {
		t.Fatalf("Close() after Ping error: %v", err)
	}
}

func TestConnFacadeClose(t *testing.T) {
	pair := openConnTestPair(t, Config{}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	closeDone := make(chan error, 1)
	reason := strings.Repeat("x", maxCloseReasonBytes)
	go func() { closeDone <- pair.server.Close(StatusNormalClosure, reason) }()
	_, _, err := pair.client.Read(ctx)
	if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusNormalClosure {
		t.Fatalf("client close status = %d, want 1000; error=%v", got, err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error: %v", err)
	}
}

func TestConnFacadeCloseReadProcessesControlFrames(t *testing.T) {
	pair := openConnTestPair(t, Config{}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	readDone := pair.server.CloseRead(ctx)
	pair.client.CloseRead(ctx)
	if again := pair.server.CloseRead(ctx); again != readDone {
		t.Fatal("CloseRead() was not idempotent")
	}
	if err := pair.client.Ping(ctx); err != nil {
		t.Fatalf("peer Ping() error: %v", err)
	}
	if err := pair.client.Close(coderwebsocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("peer Close() error: %v", err)
	}
	select {
	case <-readDone.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("CloseRead context was not cancelled after peer close")
	}
}

func TestConnFacadeCloseReadRejectsUnexpectedData(t *testing.T) {
	pair := openConnTestPair(t, Config{}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	readDone := pair.server.CloseRead(ctx)
	if readDone == nil {
		t.Fatal("CloseRead() returned nil context")
	}
	if err := pair.client.Write(ctx, coderwebsocket.MessageText, []byte("unexpected")); err != nil {
		t.Fatal(err)
	}
	_, _, err := pair.client.Read(ctx)
	if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusPolicyViolation {
		t.Fatalf("unexpected-data close = %d, want 1008; error=%v", got, err)
	}
}

func TestConnFacadeAppliesExplicitReadLimit(t *testing.T) {
	pair := openConnTestPair(t, Config{ReadLimit: 32}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := pair.client.Write(ctx, coderwebsocket.MessageText, []byte(strings.Repeat("x", 33))); err != nil {
		t.Fatal(err)
	}
	_, _, err := pair.server.Read(ctx)
	if got := CloseStatus(err); got != StatusMessageTooBig {
		t.Fatalf("Read() close status = %d, want %d; error=%v", got, StatusMessageTooBig, err)
	}
}

func TestConnFacadeCompressionRoundTrip(t *testing.T) {
	pair := openConnTestPair(t, Config{
		CompressionMode:      CompressionNoContextTakeover,
		CompressionThreshold: 1,
	}, &coderwebsocket.DialOptions{
		CompressionMode:      coderwebsocket.CompressionNoContextTakeover,
		CompressionThreshold: 1,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	payload := []byte(strings.Repeat("compressible-payload-", 256))
	if err := pair.server.Write(ctx, MessageText, payload); err != nil {
		t.Fatal(err)
	}
	typ, got, err := pair.client.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != coderwebsocket.MessageText || string(got) != string(payload) {
		t.Fatalf("compressed round trip mismatch: type=%d len=%d", typ, len(got))
	}
}
