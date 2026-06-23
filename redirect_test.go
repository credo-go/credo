package credo_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// TestApp_WithHTTPRedirect_RequiresTLS verifies that WithHTTPRedirect without
// any TLS source fails fast at preflight — redirecting to an HTTPS server that
// does not exist is a configuration error — and rolls the state back to
// building.
func TestApp_WithHTTPRedirect_RequiresTLS(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithHTTPRedirect("127.0.0.1:0"))
	app.GET("/ping", pongHandler)

	if err := app.RunContext(context.Background()); err == nil {
		t.Fatal("WithHTTPRedirect without TLS should fail at preflight")
	}
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after preflight failure, want building", got)
	}
}

// TestApp_WithHTTPRedirect_RedirectsToHTTPS verifies the redirect listener
// permanently redirects to the HTTPS equivalent on the bound TLS port: 301 for
// GET/HEAD, 308 for other methods, preserving path and query.
func TestApp_WithHTTPRedirect_RedirectsToHTTPS(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	_, _, redirectAddr := freePort(t)

	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSFiles(certFile, keyFile),
		credo.WithHTTPRedirect(redirectAddr))
	app.GET("/ping", pongHandler)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.RunContext(ctx) }()
	waitRunning(t, app)

	_, tlsPort, err := net.SplitHostPort(app.Addr().String())
	if err != nil {
		t.Fatalf("split TLS addr: %v", err)
	}

	// A client that does not follow redirects, so the 3xx is observable.
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	cases := []struct {
		name       string
		method     string
		wantStatus int
	}{
		{"GET", http.MethodGet, http.StatusMovedPermanently},
		{"HEAD", http.MethodHead, http.StatusMovedPermanently},
		{"POST", http.MethodPost, http.StatusPermanentRedirect},
		{"PUT", http.MethodPut, http.StatusPermanentRedirect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, "http://"+redirectAddr+"/path?q=1", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("redirect request failed: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			want := "https://127.0.0.1:" + tlsPort + "/path?q=1"
			if got := resp.Header.Get("Location"); got != want {
				t.Errorf("Location = %q, want %q", got, want)
			}
		})
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("RunContext() returned error: %v", err)
	}
}

// TestApp_WithHTTPRedirect_ListenFailure_RollsBackState verifies that a failure
// to bind the redirect listener (here: the port is already in use) is a
// pre-session failure — the main listener is torn down and the state rolls back
// to building, free to run again.
func TestApp_WithHTTPRedirect_ListenFailure_RollsBackState(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSFiles(certFile, keyFile),
		credo.WithHTTPRedirect(occupied.Addr().String()))
	app.GET("/ping", pongHandler)

	if err := app.RunContext(context.Background()); err == nil {
		t.Fatal("redirect listen on an occupied port should fail")
	}
	if app.IsRunning() {
		t.Error("server should not be running after a redirect listen failure")
	}
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after redirect listen failure, want building", got)
	}
}
