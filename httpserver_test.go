package credo_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/config"
)

// rawRequest opens a plain TCP connection to addr, writes req verbatim, and
// returns the status line. It bypasses net/http on the client side because the
// cases below are about how the server reacts to malformed or oversized
// headers, which a well-behaved client would never send.
func rawRequest(t *testing.T, addr, req string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	return strings.TrimSpace(line)
}

// TestMaxHeaderValueCount_RejectsAndStaysSilent covers the server.
// max_header_value_count key end to end and locks the documented logging
// caveat: net/http writes the 431 straight to the connection, so unlike the
// diagnostics the ErrorLog bridge carries, this rejection never reaches the
// application logger. An operator who needs visibility must count 431s at the
// proxy, not grep the logs.
func TestMaxHeaderValueCount_RejectsAndStaysSilent(t *testing.T) {
	rc, err := config.LoadBytes([]byte(`{"server":{"max_header_value_count":3}}`),
		config.FormatJSON, config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	logs := &syncBuffer{}
	app, addr := newLoggingApp(t, logs, credo.WithRawConfig(rc))
	app.GET("/ping", pongHandler)
	startApp(t, app)

	// Host plus two headers is within the limit.
	if got := rawRequest(t, addr, "GET /ping HTTP/1.1\r\nHost: x\r\nA: 1\r\nB: 2\r\n\r\n"); !strings.Contains(got, "200") {
		t.Errorf("within-limit request: status line = %q, want 200", got)
	}
	// Host plus four headers exceeds it.
	over := "GET /ping HTTP/1.1\r\nHost: x\r\nA: 1\r\nB: 2\r\nC: 3\r\nD: 4\r\n\r\n"
	if got := rawRequest(t, addr, over); !strings.Contains(got, "431") {
		t.Errorf("over-limit request: status line = %q, want 431", got)
	}
	if got := logs.String(); strings.Contains(got, "431") || strings.Contains(got, "too large") {
		t.Errorf("431 rejections are written to the connection by net/http and must not appear in the logs:\n%s", got)
	}
}

// TestNew_RejectsNegativeMaxHeaderValueCount proves the fail-fast validation:
// net/http treats every value below 1 as "use the default", so a negative
// number would silently do nothing instead of what the operator meant.
func TestNew_RejectsNegativeMaxHeaderValueCount(t *testing.T) {
	rc, err := config.LoadBytes([]byte(`{"server":{"max_header_value_count":-1}}`),
		config.FormatJSON, config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if _, err := credo.New(credo.WithRawConfig(rc)); err == nil {
		t.Fatal("New accepted a negative server.max_header_value_count")
	} else if !strings.Contains(err.Error(), "MaxHeaderValueCount") {
		t.Errorf("error = %q, want it to name the field", err)
	}
}

// TestWithHTTPServer_AppliedToRunningServer proves the callback reaches the
// server that actually serves traffic: a ConnState hook installed through it
// observes real connections, and a limit set there behaves exactly like the
// config key.
func TestWithHTTPServer_AppliedToRunningServer(t *testing.T) {
	var newConns atomic.Int64
	app, addr := newLoggingApp(t, &syncBuffer{}, credo.WithHTTPServer(func(s *http.Server) {
		s.MaxHeaderValueCount = 3
		prev := s.ConnState
		s.ConnState = func(c net.Conn, state http.ConnState) {
			if state == http.StateNew {
				newConns.Add(1)
			}
			if prev != nil {
				prev(c, state)
			}
		}
	}))
	app.GET("/ping", pongHandler)
	startApp(t, app)

	over := "GET /ping HTTP/1.1\r\nHost: x\r\nA: 1\r\nB: 2\r\nC: 3\r\nD: 4\r\n\r\n"
	if got := rawRequest(t, addr, over); !strings.Contains(got, "431") {
		t.Errorf("status line = %q, want 431 from the callback-set limit", got)
	}
	if newConns.Load() == 0 {
		t.Error("ConnState installed by the callback never fired")
	}
}

// TestWithHTTPServer_FrameworkOwnedFields proves the two re-imposed fields
// hold on a live server: a callback that hijacks Handler and Addr changes
// neither where the server listens nor what answers.
func TestWithHTTPServer_FrameworkOwnedFields(t *testing.T) {
	app, addr := newLoggingApp(t, &syncBuffer{}, credo.WithHTTPServer(func(s *http.Server) {
		s.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
		s.Addr = "127.0.0.1:1" // a port the test could never bind
	}))
	app.GET("/ping", pongHandler)
	startApp(t, app)

	if got := app.Addr().String(); got != addr {
		t.Errorf("bound address = %q, want the configured %q", got, addr)
	}
	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — the App must remain the handler", resp.StatusCode)
	}
}

// TestWithHTTPServer_TLSConfigNotHonoured pins the documented corner: TLS is
// configured through WithTLSConfig/WithTLSFiles, and a TLSConfig assigned in
// the callback never upgrades a plaintext listener. Silently serving TLS from
// here would make the TLS precedence chain unauditable.
func TestWithHTTPServer_TLSConfigNotHonoured(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	app, addr := newLoggingApp(t, &syncBuffer{}, credo.WithHTTPServer(func(s *http.Server) {
		s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}))
	app.GET("/ping", pongHandler)
	startApp(t, app)

	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("plaintext GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 over plaintext", resp.StatusCode)
	}
}

// TestWithHTTPServer_H2C makes the ServeContext documentation true: the
// listener comes from the caller, the protocol from the callback. Before
// WithHTTPServer, Protocols was unreachable and H2C could not be served at
// all.
func TestWithHTTPServer_H2C(t *testing.T) {
	app := mustNew(t, credo.WithoutAccessLog(), credo.WithHTTPServer(func(s *http.Server) {
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		s.Protocols = protocols
	}))
	app.GET("/ping", pongHandler)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.ServeContext(ctx, l) }()
	waitRunning(t, app)
	t.Cleanup(func() {
		cancel()
		<-errCh
	})

	clientProtocols := new(http.Protocols)
	clientProtocols.SetUnencryptedHTTP2(true)
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Protocols: clientProtocols},
	}
	resp, err := client.Get("http://" + l.Addr().String() + "/ping")
	if err != nil {
		t.Fatalf("h2c GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Errorf("resp.Proto = %q, want HTTP/2", resp.Proto)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestWithHTTPServer_RedirectServerExcluded locks D4 of the escape-hatch
// contract: the callback configures the application server only. The
// WithHTTPRedirect listener is a fixed-function 301/308 responder, so user
// knobs must not silently reach it.
func TestWithHTTPServer_RedirectServerExcluded(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	_, _, redirectAddr := freePort(t)

	var calls atomic.Int64
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithoutAccessLog(),
		credo.WithTLSFiles(certFile, keyFile),
		credo.WithHTTPRedirect(redirectAddr),
		credo.WithHTTPServer(func(s *http.Server) {
			calls.Add(1)
			// A limit low enough that the redirect request below would be
			// rejected with 431 if the callback leaked into that server.
			s.MaxHeaderValueCount = 1
		}))
	app.GET("/ping", pongHandler)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.RunContext(ctx) }()
	waitRunning(t, app)
	defer func() {
		cancel()
		if err := <-errCh; err != nil {
			t.Errorf("RunContext: %v", err)
		}
	}()

	if got := calls.Load(); got != 1 {
		t.Fatalf("callback ran %d times, want exactly 1 (the application server)", got)
	}
	req := fmt.Sprintf("GET /path HTTP/1.1\r\nHost: %s\r\nA: 1\r\nB: 2\r\n\r\n", redirectAddr)
	if got := rawRequest(t, redirectAddr, req); !strings.Contains(got, "301") {
		t.Errorf("redirect listener status line = %q, want 301 — the callback must not apply to it", got)
	}
}
