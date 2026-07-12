package websocket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coderwebsocket "github.com/coder/websocket"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func newHandlerTestApp(t *testing.T, cfg ...Config) (*credo.App, *Server) {
	t.Helper()
	app, err := credo.New(credo.WithoutAccessLog())
	if err != nil {
		t.Fatal(err)
	}
	return app, Use(app, cfg...)
}

func validHandlerTestRequest(method string) *http.Request {
	r := httptest.NewRequest(method, "http://example.com/ws", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return r
}

func TestServerHandlerRealConnection(t *testing.T) {
	app, server := newHandlerTestApp(t, Config{
		Subprotocols:       []string{"echo.v1"},
		RequireSubprotocol: true,
	})
	serverCtxSeen := make(chan context.Context, 1)
	app.GET("/ws", server.Handler(func(_ *credo.Context, conn *Conn) error {
		serverCtxSeen <- conn.Context()
		typ, data, err := conn.Read(conn.Context())
		if err != nil {
			return err
		}
		return conn.Write(conn.Context(), typ, data)
	})).Name("echo")

	httpServer := httptest.NewServer(app)
	t.Cleanup(httpServer.Close)
	client, response, err := coderwebsocket.Dial(
		t.Context(),
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws",
		&coderwebsocket.DialOptions{Subprotocols: []string{"echo.v1"}},
	)
	if err != nil {
		t.Fatalf("Dial() response=%v error=%v", response, err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	if client.Subprotocol() != "echo.v1" {
		t.Errorf("client subprotocol = %q", client.Subprotocol())
	}
	if err := client.Write(t.Context(), coderwebsocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := client.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if typ != coderwebsocket.MessageText || string(data) != "hello" {
		t.Errorf("echo = (%d, %q)", typ, data)
	}
	_, _, err = client.Read(t.Context())
	if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusNormalClosure {
		t.Fatalf("terminal close = %d, want 1000; error=%v", got, err)
	}
	connectionCtx := <-serverCtxSeen
	select {
	case <-connectionCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("connection context was not cancelled after normal close")
	}
	if context.Cause(connectionCtx) != context.Canceled {
		t.Errorf("connection context cause = %v", context.Cause(connectionCtx))
	}
}

func TestServerHandlerPreAcceptFailuresUseProblemDetails(t *testing.T) {
	app, server := newHandlerTestApp(t, Config{
		AllowedOrigins: []string{"https://allowed.example"},
		Subprotocols:   []string{"chat.v1"},
	})
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.Response().Header().Set("X-Security", "set")
			return next(ctx)
		}
	})
	app.GET("/ws", server.Handler(func(*credo.Context, *Conn) error {
		t.Fatal("application handler ran for rejected handshake")
		return nil
	}))

	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
		header string
		value  string
	}{
		{
			name: "missing upgrade", mutate: func(r *http.Request) { r.Header.Del("Connection") },
			want: http.StatusUpgradeRequired, header: "Upgrade", value: "websocket",
		},
		{name: "HEAD", want: http.StatusMethodNotAllowed, header: "Allow", value: "GET"},
		{
			name: "bad version", mutate: func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "12") },
			want: http.StatusBadRequest, header: "Sec-WebSocket-Version", value: "13",
		},
		{
			name: "forbidden origin", mutate: func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
			want: http.StatusForbidden,
		},
		{
			name: "subprotocol mismatch", mutate: func(r *http.Request) { r.Header.Set("Sec-WebSocket-Protocol", "other") },
			want: http.StatusBadRequest,
		},
		{name: "non hijacker", want: http.StatusNotImplemented},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodGet
			if tc.name == "HEAD" {
				method = http.MethodHead
			}
			r := validHandlerTestRequest(method)
			if tc.mutate != nil {
				tc.mutate(r)
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("X-Security") != "set" {
				t.Error("baseline security header was lost")
			}
			if tc.header != "" && w.Header().Get(tc.header) != tc.value {
				t.Errorf("%s = %q, want %q", tc.header, w.Header().Get(tc.header), tc.value)
			}
			if method != http.MethodHead {
				if contentType := w.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/problem+json") {
					t.Errorf("Content-Type = %q", contentType)
				}
				var problem credo.ProblemDetails
				if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
					t.Fatalf("problem JSON: %v; body=%s", err, w.Body.String())
				}
				if problem.Status != tc.want {
					t.Errorf("problem status = %d, want %d", problem.Status, tc.want)
				}
			}
			if strings.Contains(w.Body.String(), "WebSocket protocol") {
				t.Error("internal upstream/mechanical error leaked to response")
			}
		})
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.activeTokens != 0 || len(server.connections) != 0 || server.closeTasks != 0 {
		t.Errorf("pre-Accept failure leak: tokens=%d connections=%d tasks=%d",
			server.activeTokens, len(server.connections), server.closeTasks)
	}
}

func TestServerHandlerValidationPrecedesDraining(t *testing.T) {
	app, server := newHandlerTestApp(t, Config{Subprotocols: []string{"chat"}})
	app.GET("/ws", server.Handler(func(*credo.Context, *Conn) error { return nil }))
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	badOrigin := validHandlerTestRequest(http.MethodGet)
	badOrigin.Header.Set("Origin", "null")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, badOrigin)
	if w.Code != http.StatusForbidden {
		t.Errorf("bad origin while draining status = %d, want 403", w.Code)
	}

	badProtocol := validHandlerTestRequest(http.MethodGet)
	badProtocol.Header.Set("Sec-WebSocket-Protocol", "other")
	w = httptest.NewRecorder()
	app.ServeHTTP(w, badProtocol)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad protocol while draining status = %d, want 400", w.Code)
	}

	valid := validHandlerTestRequest(http.MethodGet)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, valid)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("valid handshake while draining status = %d, want 503", w.Code)
	}
}

func TestServerHandlerActualHijackFailureDoesNotPublishSecondBody(t *testing.T) {
	app, server := newHandlerTestApp(t)
	app.GET("/ws", server.Handler(func(*credo.Context, *Conn) error {
		t.Fatal("handler ran after failed hijack")
		return nil
	}))
	wantErr := errors.New("hijack failed")
	base := newCoderHandshakeRecorder()
	w := &coderHijackRecorder{coderHandshakeRecorder: base, hijackErr: wantErr}
	app.ServeHTTP(w, validHandlerTestRequest(http.MethodGet))
	if len(base.statuses) != 1 || base.statuses[0] != http.StatusSwitchingProtocols {
		t.Fatalf("statuses = %v, want [101]", base.statuses)
	}
	if base.body.Len() != 0 {
		t.Fatalf("post-101 body leaked: %q", base.body.String())
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.activeTokens != 0 || len(server.connections) != 0 {
		t.Errorf("hijack failure leak: tokens=%d connections=%d",
			server.activeTokens, len(server.connections))
	}
}

type blockingHijackFailureWriter struct {
	*coderHandshakeRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *blockingHijackFailureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	close(w.entered)
	<-w.release
	return nil, nil, errors.New("blocked hijack failed")
}

func TestServerAdmissionTokenPrecedesAcceptAndBlocksDrainCompletion(t *testing.T) {
	app, server := newHandlerTestApp(t)
	app.GET("/ws", server.Handler(func(*credo.Context, *Conn) error {
		t.Fatal("handler ran after failed blocked hijack")
		return nil
	}))
	w := &blockingHijackFailureWriter{
		coderHandshakeRecorder: newCoderHandshakeRecorder(),
		entered:                make(chan struct{}),
		release:                make(chan struct{}),
	}
	serveDone := make(chan struct{})
	go func() {
		app.ServeHTTP(w, validHandlerTestRequest(http.MethodGet))
		close(serveDone)
	}()
	select {
	case <-w.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Accept did not reach blocked Hijack")
	}
	server.mu.Lock()
	if server.activeTokens != 1 || len(server.connections) != 0 {
		t.Fatalf("during Accept tokens=%d connections=%d, want 1/0",
			server.activeTokens, len(server.connections))
	}
	server.mu.Unlock()

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- server.Shutdown(t.Context()) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		draining := server.state == serverDraining
		server.mu.Unlock()
		if draining {
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("drain completed while Accept token was held: %v", err)
	default:
	}
	close(w.release)
	<-serveDone
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.state != serverClosed || server.activeTokens != 0 {
		t.Errorf("final state=%d tokens=%d", server.state, server.activeTokens)
	}
}

func TestServerHandlerApplicationErrorAndPanicClose1011(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler Handler
	}{
		{name: "error", handler: func(*credo.Context, *Conn) error { return errors.New("secret") }},
		{name: "panic", handler: func(*credo.Context, *Conn) error { panic("secret panic") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, server := newHandlerTestApp(t)
			app.GET("/ws", server.Handler(tc.handler))
			httpServer := httptest.NewServer(app)
			defer httpServer.Close()
			client, _, err := coderwebsocket.Dial(
				t.Context(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws", nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseNow()
			_, _, err = client.Read(t.Context())
			if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusInternalError {
				t.Fatalf("close status = %d, want 1011; error=%v", got, err)
			}
			if closeErr, ok := errors.AsType[coderwebsocket.CloseError](err); ok && closeErr.Reason != "internal error" {
				t.Errorf("wire reason = %q", closeErr.Reason)
			}
			shutdownCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				t.Fatalf("post-terminal Shutdown() error: %v", err)
			}
		})
	}
}

func TestServerHandlerNilPanics(t *testing.T) {
	_, server := newHandlerTestApp(t)
	defer func() {
		if recover() == nil {
			t.Fatal("Handler(nil) did not panic")
		}
	}()
	server.Handler(nil)
}

func TestServerShutdownDrainsHandlerAndIsStable(t *testing.T) {
	app, server := newHandlerTestApp(t)
	handlerStarted := make(chan struct{})
	app.GET("/ws", server.Handler(func(_ *credo.Context, conn *Conn) error {
		close(handlerStarted)
		_, _, err := conn.Read(conn.Context())
		return err
	}))
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()
	client, _, err := coderwebsocket.Dial(
		t.Context(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	<-handlerStarted

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- server.Shutdown(t.Context()) }()
	_, _, err = client.Read(t.Context())
	if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusGoingAway {
		t.Fatalf("shutdown close = %d, want 1001; error=%v", got, err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated Shutdown() error: %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.state != serverClosed || server.activeTokens != 0 || len(server.connections) != 0 || server.closeTasks != 0 {
		t.Errorf("final state=%d tokens=%d connections=%d tasks=%d",
			server.state, server.activeTokens, len(server.connections), server.closeTasks)
	}
}

func TestServerShutdownDeadlineIsIncompleteThenEventuallyClosed(t *testing.T) {
	app, server := newHandlerTestApp(t)
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	var releaseOnce sync.Once
	app.GET("/ws", server.Handler(func(*credo.Context, *Conn) error {
		close(handlerStarted)
		<-releaseHandler
		return nil
	}))
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()
	client, _, err := coderwebsocket.Dial(
		t.Context(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	client.CloseRead(t.Context())
	<-handlerStarted
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseHandler) }) })

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err = server.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want deadline", err)
	}
	if !strings.Contains(err.Error(), "handlers=1") {
		t.Errorf("incomplete error lacks handler count: %v", err)
	}
	server.mu.Lock()
	if server.state != serverDraining {
		t.Errorf("state = %d after incomplete drain, want draining", server.state)
	}
	server.mu.Unlock()

	releaseOnce.Do(func() { close(releaseHandler) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		closed := server.state == serverClosed
		server.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server did not reach eventual closed state")
}

func TestServerShutdownWaiterCancellationDoesNotChangeOwner(t *testing.T) {
	app, server := newHandlerTestApp(t)
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	app.GET("/ws", server.Handler(func(*credo.Context, *Conn) error {
		close(handlerStarted)
		<-releaseHandler
		return nil
	}))
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()
	client, _, err := coderwebsocket.Dial(
		t.Context(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	client.CloseRead(t.Context())
	<-handlerStarted

	ownerDone := make(chan error, 1)
	ownerCtx, ownerCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer ownerCancel()
	go func() { ownerDone <- server.Shutdown(ownerCtx) }()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		started := server.drainStarted
		server.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	waiterCtx, waiterCancel := context.WithCancel(t.Context())
	waiterCancel()
	if err := server.Shutdown(waiterCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter Shutdown() error = %v, want canceled", err)
	}
	select {
	case err := <-ownerDone:
		t.Fatalf("owner returned before handler release: %v", err)
	default:
	}
	close(releaseHandler)
	if err := <-ownerDone; err != nil {
		t.Fatalf("owner Shutdown() error: %v", err)
	}
	if err := server.Shutdown(waiterCtx); err != nil {
		t.Fatalf("completed stable Shutdown() error: %v", err)
	}
}

type handlerTestResource struct {
	alive atomic.Bool
}

func (r *handlerTestResource) Shutdown(context.Context) error {
	r.alive.Store(false)
	return nil
}

func TestManagedOnDrainFinishesHandlerBeforeDIShutdown(t *testing.T) {
	app, err := credo.New(
		credo.WithAddr("127.0.0.1", 0),
		credo.WithoutAccessLog(),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := Use(app)
	resource := &handlerTestResource{}
	resource.alive.Store(true)
	app.MustProvideValue[*handlerTestResource](resource)
	handlerStarted := make(chan struct{})
	var aliveDuringCleanup atomic.Bool
	app.GET("/ws", server.Handler(func(_ *credo.Context, conn *Conn) error {
		close(handlerStarted)
		_, _, readErr := conn.Read(conn.Context())
		aliveDuringCleanup.Store(resource.alive.Load())
		return readErr
	}))

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- app.RunContext(runCtx) }()
	deadline := time.Now().Add(3 * time.Second)
	for !app.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !app.IsRunning() {
		t.Fatal("app did not start")
	}
	client, _, err := coderwebsocket.Dial(
		t.Context(), "ws://"+app.Addr().String()+"/ws", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	<-handlerStarted
	cancelRun()
	_, _, err = client.Read(t.Context())
	if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusGoingAway {
		t.Fatalf("managed close = %d, want 1001; error=%v", got, err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("RunContext() error: %v", err)
	}
	if !aliveDuringCleanup.Load() {
		t.Fatal("DI resource was shut down before WebSocket handler cleanup")
	}
	if resource.alive.Load() {
		t.Fatal("DI resource remained alive after App drain")
	}
	server.mu.Lock()
	managedCtx := server.managedCtx
	server.mu.Unlock()
	if managedCtx == nil || managedCtx.Err() == nil {
		t.Fatal("managed lifecycle context was not captured and cancelled")
	}
}

func TestServerHandlerWSSAndHTTP2Negative(t *testing.T) {
	app, server := newHandlerTestApp(t)
	var requestProto atomic.Int32
	app.GET("/ws", server.Handler(func(ctx *credo.Context, _ *Conn) error {
		requestProto.Store(int32(ctx.Request().ProtoMajor))
		return nil
	}))
	tlsServer := httptest.NewUnstartedServer(app)
	tlsServer.EnableHTTP2 = true
	tlsServer.StartTLS()
	defer tlsServer.Close()

	client, _, err := coderwebsocket.Dial(
		t.Context(),
		"wss"+strings.TrimPrefix(tlsServer.URL, "https")+"/ws",
		&coderwebsocket.DialOptions{HTTPClient: tlsServer.Client()},
	)
	if err != nil {
		t.Fatalf("WSS Dial() error: %v", err)
	}
	_, _, err = client.Read(t.Context())
	if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusNormalClosure {
		t.Fatalf("WSS close = %d, want 1000; error=%v", got, err)
	}
	_ = client.CloseNow()
	if requestProto.Load() != 1 {
		t.Errorf("classic WSS handshake ProtoMajor = %d, want 1", requestProto.Load())
	}

	response, err := tlsServer.Client().Get(tlsServer.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("negative request ProtoMajor = %d, want 2", response.ProtoMajor)
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("HTTP/2 request unexpectedly produced 101")
	}
}

func TestServerHandlerThroughCompressionMiddleware(t *testing.T) {
	app, server := newHandlerTestApp(t)
	app.GET("/ws", server.Handler(func(*credo.Context, *Conn) error { return nil })).
		Middleware(middleware.Compress())
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()
	client, _, err := coderwebsocket.Dial(
		t.Context(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	_, _, err = client.Read(t.Context())
	if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusNormalClosure {
		t.Fatalf("close through compression middleware = %d, want 1000; error=%v", got, err)
	}
}

func TestServerShutdownForceUnblocksTrackedCloseTask(t *testing.T) {
	server := &Server{
		connections: make(map[*connectionRecord]struct{}),
		changed:     make(chan struct{}),
		drainDone:   make(chan struct{}),
	}
	closeRelease := make(chan struct{})
	closeNowCalled := make(chan struct{})
	var releaseOnce sync.Once
	connectionCtx, cancelConnection := context.WithCancelCause(context.Background())
	record := &connectionRecord{
		cancel: cancelConnection,
		close: func(StatusCode, string) error {
			<-closeRelease
			return nil
		},
		closeNow: func() error {
			close(closeNowCalled)
			releaseOnce.Do(func() { close(closeRelease) })
			return nil
		},
		connectionID: "blocked-close",
		closeDone:    make(chan struct{}),
	}
	server.connections[record] = struct{}{}
	server.activeTokens = 1
	go server.finish(record)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := server.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want deadline", err)
	}
	if !strings.Contains(err.Error(), "close_tasks=1") {
		t.Errorf("incomplete error lacks close-task count: %v", err)
	}
	select {
	case <-closeNowCalled:
	case <-time.After(time.Second):
		t.Fatal("force path did not invoke best-effort CloseNow")
	}
	select {
	case <-connectionCtx.Done():
		if !errors.Is(context.Cause(connectionCtx), context.DeadlineExceeded) {
			t.Errorf("connection cause = %v, want deadline", context.Cause(connectionCtx))
		}
	case <-time.After(time.Second):
		t.Fatal("force path did not cancel connection context")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		closed := server.state == serverClosed
		server.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("forced close task did not reach eventual closed state")
}

func TestServerAttachRechecksDrainAfterAccept(t *testing.T) {
	server := &Server{
		state:        serverDraining,
		connections:  make(map[*connectionRecord]struct{}),
		changed:      make(chan struct{}),
		drainDone:    make(chan struct{}),
		activeTokens: 1,
	}
	connectionCtx, cancelConnection := context.WithCancelCause(context.Background())
	record := &connectionRecord{
		cancel:       cancelConnection,
		close:        func(StatusCode, string) error { return nil },
		closeNow:     func() error { return nil },
		connectionID: "late-attach",
		closeDone:    make(chan struct{}),
	}
	if server.attach(record) {
		t.Fatal("attach accepted a connection after drain began")
	}
	server.finish(record)
	if !errors.Is(context.Cause(connectionCtx), context.Canceled) {
		t.Errorf("late-attach context cause = %v, want canceled", context.Cause(connectionCtx))
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.state != serverClosed || server.activeTokens != 0 || len(server.connections) != 0 {
		t.Errorf("late attach cleanup state=%d tokens=%d connections=%d",
			server.state, server.activeTokens, len(server.connections))
	}
}
