package websocket

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	coderwebsocket "github.com/coder/websocket"

	"github.com/credo-go/credo"
)

func TestServerBoundWithIngressIsOneComponent(t *testing.T) {
	app, err := credo.New()
	if err != nil {
		t.Fatal(err)
	}
	server := New(app.NewInfra("websocket"))
	app.ProvideValue(server, credo.Ingress())
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Manage of a server a ProvideValue binding holds did not panic")
		}
		app.GET("/ws", server.Handler(func(*credo.Context, *Conn) error { return nil }))
		startTestApp(t, app)
		if err := server.acquireToken(); err != nil {
			t.Fatalf("the bound server was not started: %v", err)
		}
		server.releaseToken()
	}()
	app.Manage(server, credo.Ingress())
}

// peerRegistry is the application's own connection registry: the handlers
// fill it and the WebSocket drain empties it.
type peerRegistry struct {
	mu     sync.Mutex
	peers  map[*Conn]struct{}
	events *[]string
}

func (r *peerRegistry) add(c *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers[c] = struct{}{}
}

func (r *peerRegistry) remove(c *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.peers, c)
}

func (r *peerRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}

func (r *peerRegistry) Shutdown(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.events = append(*r.events, "registry stop")
	return nil
}

// broadcaster is an internal consumer that sends through the registry.
type broadcaster struct {
	registry *peerRegistry
	reached  int
}

func (b *broadcaster) Shutdown(context.Context) error {
	// What it still sends on its way out reaches every peer left.
	b.reached = b.registry.count()
	b.registry.mu.Lock()
	defer b.registry.mu.Unlock()
	*b.registry.events = append(*b.registry.events, "broadcaster stop")
	return nil
}

func TestBroadcastAfterTheDrainFindsNoPeersAndStopsBeforeTheRegistry(t *testing.T) {
	app, err := credo.New(credo.WithAddr("127.0.0.1", 0))
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	registry := &peerRegistry{peers: make(map[*Conn]struct{}), events: &events}
	app.ProvideValue(registry)
	var b *broadcaster
	app.Provide[*broadcaster](func(r *peerRegistry) *broadcaster {
		b = &broadcaster{registry: r}
		return b
	})
	server := New(app.NewInfra("websocket"))
	app.Manage(server, credo.Ingress())
	connected := make(chan struct{})
	app.GET("/ws", server.Handler(func(_ *credo.Context, conn *Conn) error {
		registry.add(conn)
		defer registry.remove(conn)
		close(connected)
		_, _, readErr := conn.Read(conn.Context())
		return readErr
	}))
	// The broadcaster is built before the drain, as a running App's would be.
	app.OnStart(func(context.Context) error {
		_, resolveErr := app.Resolve[*broadcaster]()
		return resolveErr
	})

	runCtx, cancelRun := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- app.RunContext(runCtx) }()
	deadline := time.Now().Add(3 * time.Second)
	for !app.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !app.IsRunning() {
		t.Fatal("app did not start")
	}
	client, err := dialHandlerTest(t.Context(), "ws://"+app.Addr().String()+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	<-connected

	cancelRun()
	_, _, err = client.Read(t.Context())
	if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusGoingAway {
		t.Fatalf("drain close = %d, want 1001; error=%v", got, err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("RunContext() = %v", err)
	}
	if b.reached != 0 {
		t.Fatalf("the broadcaster reached %d peers after the drain, want 0", b.reached)
	}
	if got := strings.Join(events, ","); got != "broadcaster stop,registry stop" {
		t.Fatalf("stop order = %q, want the broadcaster before the registry", got)
	}
}
