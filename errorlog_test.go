package credo_test

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// newLoggingApp builds (but does not start) an app bound to a free port with
// its logger captured by logs. Routes must be registered before [startApp],
// which freezes the app.
func newLoggingApp(t *testing.T, logs *syncBuffer, opts ...credo.Option) (*credo.App, string) {
	t.Helper()
	host, port, addr := freePort(t)
	all := append([]credo.Option{
		credo.WithAddr(host, port),
		credo.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		credo.WithoutAccessLog(),
	}, opts...)
	return mustNew(t, all...), addr
}

// startApp runs the app, waits until it is serving, and shuts it down on
// cleanup.
func startApp(t *testing.T, app *credo.App) {
	t.Helper()
	errC := make(chan error, 1)
	go func() { errC <- app.RunContext(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for !app.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("app did not reach running state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Shutdown(ctx)
		<-errC
	})
}

// TestServerErrorLog_TLSHandshakeFailure proves the ErrorLog bridge end to
// end: a plaintext request to a TLS port makes net/http report a handshake
// failure, which must reach the application logger (ERROR,
// component=net/http) instead of the standard log package's stderr output.
func TestServerErrorLog_TLSHandshakeFailure(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	logs := &syncBuffer{}
	app, addr := newLoggingApp(t, logs, credo.WithTLSFiles(certFile, keyFile))
	app.GET("/ping", pongHandler)
	startApp(t, app)

	// net/http answers a plaintext request on a TLS port with a 400 written
	// directly to the connection — and logs the handshake failure.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("plaintext request to the TLS port: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (net/http's plaintext-on-TLS reply)", resp.StatusCode)
	}

	got := logs.waitFor(t, "TLS handshake error")
	for _, want := range []string{"level=ERROR", "component=net/http", "http: TLS handshake error from"} {
		if !strings.Contains(got, want) {
			t.Errorf("captured logs missing %q:\n%s", want, got)
		}
	}
}

// TestServerErrorLog_PanicOutsideRecovery covers a second net/http call site:
// with the built-in recovery disabled a handler panic unwinds into net/http,
// whose "panic serving" report must also land in the application logger.
func TestServerErrorLog_PanicOutsideRecovery(t *testing.T) {
	logs := &syncBuffer{}
	app, addr := newLoggingApp(t, logs, credo.WithoutRecover())
	app.GET("/boom", func(*credo.Context) error { panic("boom") })
	startApp(t, app)

	client := &http.Client{Timeout: 5 * time.Second}
	if resp, err := client.Get("http://" + addr + "/boom"); err == nil {
		resp.Body.Close()
		t.Fatal("a panic without recovery should break the connection")
	}

	got := logs.waitFor(t, "panic serving")
	for _, want := range []string{"level=ERROR", "component=net/http"} {
		if !strings.Contains(got, want) {
			t.Errorf("captured logs missing %q:\n%s", want, got)
		}
	}
}
