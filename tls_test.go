package credo_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/config"
)

// --- TLS test helpers ---

// pongHandler is a trivial 200 handler shared by the TLS serving tests.
func pongHandler(ctx *credo.Context) error {
	return ctx.Response().Text(http.StatusOK, "pong")
}

// generateSelfSignedCert writes a self-signed certificate and its private key
// to PEM files under t.TempDir and returns their paths. The cert is a minimal
// ed25519 CA valid for "localhost" / 127.0.0.1 for one hour — enough for the
// in-process HTTPS round-trips the lifecycle tests perform.
func generateSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

// tlsCertificate loads the PEM pair as an in-memory tls.Certificate, for the
// WithTLSConfig tests.
func tlsCertificate(t *testing.T, certFile, keyFile string) tls.Certificate {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	return cert
}

// httpsClient returns an HTTP client that trusts only certFile and pins the
// "localhost" server name, so a successful request proves the server served
// exactly that certificate.
func httpsClient(t *testing.T, certFile string) *http.Client {
	t.Helper()
	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("append cert to pool")
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"},
		},
	}
}

// rawConfigWithServer builds a RawConfig whose "server" section is the given
// map, decoded through the real config loader so nested keys such as
// server.tls.cert_file exercise the production unmarshal path. An unused env
// prefix keeps process environment variables from leaking into the result.
func rawConfigWithServer(t *testing.T, server map[string]any) credo.RawConfig {
	t.Helper()
	data, err := json.Marshal(map[string]any{"server": server})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	rc, err := config.LoadBytes(data, config.FormatJSON, config.WithPrefix("CREDO_TLS_TEST_UNUSED_"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return rc
}

// assertHTTPSPong runs app via RunContext, performs a TLS GET /ping trusting
// certFile, asserts a 200 "pong", then cancels and waits for graceful return.
// A handshake or transport failure here means the wrong (or no) certificate was
// served, which is how the precedence tests prove which source won.
func assertHTTPSPong(t *testing.T, app *credo.App, certFile string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.RunContext(ctx) }()
	waitRunning(t, app)

	resp, err := httpsClient(t, certFile).Get("https://" + app.Addr().String() + "/ping")
	if err != nil {
		cancel()
		<-errCh
		t.Fatalf("HTTPS request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if string(body) != "pong" {
		t.Errorf("body = %q, want %q", body, "pong")
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("RunContext() returned error: %v", err)
	}
}

// --- Preflight failure / rollback ---

// TestApp_TLSFiles_PreflightFailure_RollsBackState verifies that a TLS
// key-pair preflight failure is a pre-session failure: Run rolls back to
// building (free to run again), not the terminal stopped state used for session
// failures. The missing cert files fail the preflight before any listener is
// bound, so this exercises the preflight path specifically (a genuine listen
// failure shares tcpListen with plain Run — see
// TestApp_Run_ListenFailure_RollsBackState).
func TestApp_TLSFiles_PreflightFailure_RollsBackState(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSFiles("nonexistent.crt", "nonexistent.key"))
	app.GET("/ping", pongHandler)

	if err := app.Run(); err == nil {
		t.Fatal("Run() should fail with a missing key pair")
	}

	// Pre-session failure → building (may run again), not stopped.
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after preflight failure, want %q", got, "building")
	}
}

// TestApp_RunContext_TLSFiles_BadCertFailFast verifies an invalid key pair is
// caught before the server starts. A free port proves the failure is the
// preflight, not a listen error, and state rolls back to building.
func TestApp_RunContext_TLSFiles_BadCertFailFast(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSFiles("nonexistent.crt", "nonexistent.key"))
	app.GET("/ping", pongHandler)

	if err := app.RunContext(context.Background()); err == nil {
		t.Fatal("RunContext with a missing cert should fail")
	}
	if app.IsRunning() {
		t.Error("server should not be running after preflight failure")
	}
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after preflight failure, want building", got)
	}
}

// TestApp_WithTLSConfig_NoCertSource_PreflightError verifies that a tls.Config
// with no certificate source (no Certificates, GetCertificate, or
// GetConfigForClient) fails at preflight and rolls back to building, for both
// signal-aware Run and caller-driven RunContext.
func TestApp_WithTLSConfig_NoCertSource_PreflightError(t *testing.T) {
	t.Run("RunContext", func(t *testing.T) {
		app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
			credo.WithTLSConfig(&tls.Config{}))
		app.GET("/ping", pongHandler)

		if err := app.RunContext(context.Background()); err == nil {
			t.Fatal("empty tls.Config should fail at preflight")
		}
		if got := app.State(); got != "building" {
			t.Errorf("State() = %q after preflight failure, want building", got)
		}
	})

	t.Run("Run", func(t *testing.T) {
		app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
			credo.WithTLSConfig(&tls.Config{}))
		app.GET("/ping", pongHandler)

		if err := app.Run(); err == nil {
			t.Fatal("empty tls.Config should fail at preflight")
		}
		if got := app.State(); got != "building" {
			t.Errorf("State() = %q after preflight failure, want building", got)
		}
	})
}

// TestApp_TLSConfigKeys_PartialFails verifies that a config providing only
// server.tls.cert_file (no key_file) fails at preflight and rolls back to
// building — the cert-XOR-key guard in resolveTLSConfig.
func TestApp_TLSConfigKeys_PartialFails(t *testing.T) {
	certFile, _ := generateSelfSignedCert(t)
	rc := rawConfigWithServer(t, map[string]any{
		"tls": map[string]any{"cert_file": certFile},
	})
	app := mustNew(t, credo.WithRawConfig(rc), credo.WithAddr("127.0.0.1", 0))
	app.GET("/ping", pongHandler)

	if err := app.RunContext(context.Background()); err == nil {
		t.Fatal("partial TLS config (cert without key) should fail at preflight")
	}
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after preflight failure, want building", got)
	}
}

// TestApp_WithTLSConfig_Nil_PreflightError verifies WithTLSConfig(nil) is a
// fail-fast configuration error, not a silent fall through to the file sources or
// plaintext: the option records that it was set, so a nil config fails preflight
// and rolls back to building.
func TestApp_WithTLSConfig_Nil_PreflightError(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSConfig(nil))
	app.GET("/ping", pongHandler)

	if err := app.RunContext(context.Background()); err == nil {
		t.Fatal("WithTLSConfig(nil) should fail at preflight")
	}
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after preflight failure, want building", got)
	}
}

// TestApp_WithTLSFiles_Empty_PreflightError verifies WithTLSFiles with empty
// paths is a fail-fast error, not a silent fall through. Valid server.tls.* keys
// are present, yet the explicit (empty) option wins and fails preflight rather
// than serving the config's certificate — proving the option does not silently
// defer to the lower-precedence config source — and the state rolls back to
// building.
func TestApp_WithTLSFiles_Empty_PreflightError(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	rc := rawConfigWithServer(t, map[string]any{
		"tls": map[string]any{"cert_file": certFile, "key_file": keyFile},
	})
	app := mustNew(t, credo.WithRawConfig(rc), credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSFiles("", ""))
	app.GET("/ping", pongHandler)

	if err := app.RunContext(context.Background()); err == nil {
		t.Fatal(`WithTLSFiles("", "") should fail at preflight, not fall back to the config keys`)
	}
	if got := app.State(); got != "building" {
		t.Errorf("State() = %q after preflight failure, want building", got)
	}
}

// --- Serving HTTPS ---

// TestApp_RunContext_TLSFiles_ServesHTTPS verifies WithTLSFiles serves real
// HTTPS end-to-end: a client trusting the generated certificate gets 200.
func TestApp_RunContext_TLSFiles_ServesHTTPS(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSFiles(certFile, keyFile))
	app.GET("/ping", pongHandler)

	assertHTTPSPong(t, app, certFile)
}

// TestApp_WithTLSConfig_ServesHTTPS verifies an in-memory *tls.Config serves
// HTTPS end-to-end.
func TestApp_WithTLSConfig_ServesHTTPS(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	cert := tlsCertificate(t, certFile, keyFile)
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}))
	app.GET("/ping", pongHandler)

	assertHTTPSPong(t, app, certFile)
}

// TestApp_WithTLSConfig_GetConfigForClient_ServesHTTPS verifies the cert-source
// check accepts a *tls.Config whose only source is GetConfigForClient — the
// third arm of the configHasCert mirror, with no Certificates or GetCertificate
// of its own — and serves HTTPS end-to-end via the config it returns.
func TestApp_WithTLSConfig_GetConfigForClient_ServesHTTPS(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	cert := tlsCertificate(t, certFile, keyFile)
	served := &tls.Config{Certificates: []tls.Certificate{cert}}
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSConfig(&tls.Config{
			GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { return served, nil },
		}))
	app.GET("/ping", pongHandler)

	assertHTTPSPong(t, app, certFile)
}

// TestApp_RunContext_NoTLS_ServesPlain is the regression guard: with no TLS
// configured, RunContext serves plaintext HTTP.
func TestApp_RunContext_NoTLS_ServesPlain(t *testing.T) {
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0))
	app.GET("/ping", pongHandler)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.RunContext(ctx) }()
	waitRunning(t, app)

	resp, err := http.Get("http://" + app.Addr().String() + "/ping")
	if err != nil {
		cancel()
		<-errCh
		t.Fatalf("plain HTTP request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("RunContext() returned error: %v", err)
	}
}

// --- Precedence ---

// TestApp_TLSPrecedence_ConfigOverFiles verifies WithTLSConfig outranks
// WithTLSFiles: the files point at nonexistent paths that would fail preflight
// if consulted, yet the server serves the config's certificate without error.
func TestApp_TLSPrecedence_ConfigOverFiles(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	cert := tlsCertificate(t, certFile, keyFile)
	app := mustNew(t, credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSFiles("nonexistent.crt", "nonexistent.key"),
		credo.WithTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}))
	app.GET("/ping", pongHandler)

	assertHTTPSPong(t, app, certFile)
}

// TestApp_TLSPrecedence_FilesOverConfigKeys verifies WithTLSFiles outranks the
// server.tls.* config keys: the config points at nonexistent paths, yet the
// server serves the WithTLSFiles certificate without error.
func TestApp_TLSPrecedence_FilesOverConfigKeys(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	rc := rawConfigWithServer(t, map[string]any{
		"tls": map[string]any{"cert_file": "nonexistent.crt", "key_file": "nonexistent.key"},
	})
	app := mustNew(t, credo.WithRawConfig(rc), credo.WithAddr("127.0.0.1", 0),
		credo.WithTLSFiles(certFile, keyFile))
	app.GET("/ping", pongHandler)

	assertHTTPSPong(t, app, certFile)
}

// TestApp_ConfigKeys_ServesHTTPS verifies the server.tls.* keys alone (no
// option) configure HTTPS, proving the nested config path works end-to-end.
func TestApp_ConfigKeys_ServesHTTPS(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	rc := rawConfigWithServer(t, map[string]any{
		"tls": map[string]any{"cert_file": certFile, "key_file": keyFile},
	})
	app := mustNew(t, credo.WithRawConfig(rc), credo.WithAddr("127.0.0.1", 0))
	app.GET("/ping", pongHandler)

	assertHTTPSPong(t, app, certFile)
}

// --- ServeContext is TLS-exempt ---

// TestApp_ServeContext_IgnoresConfiguredTLS verifies ServeContext serves its
// listener as-is even when TLS is configured on the app: a plain HTTP request
// succeeds, proving the configured certificate did not wrap the listener.
func TestApp_ServeContext_IgnoresConfiguredTLS(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	app := mustNew(t, credo.WithTLSFiles(certFile, keyFile))
	app.GET("/ping", pongHandler)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.ServeContext(ctx, l) }()
	waitRunning(t, app)

	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		cancel()
		<-errCh
		t.Fatalf("plain HTTP request failed (ServeContext should be TLS-exempt): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("ServeContext() returned error: %v", err)
	}
}

// TestApp_ServeContext_TLSNewListener_ServesHTTPS verifies the documented
// escape hatch: wrapping the listener with tls.NewListener serves HTTPS through
// ServeContext even though the app itself configures no TLS.
func TestApp_ServeContext_TLSNewListener_ServesHTTPS(t *testing.T) {
	certFile, keyFile := generateSelfSignedCert(t)
	cert := tlsCertificate(t, certFile, keyFile)
	app := mustNew(t)
	app.GET("/ping", pongHandler)

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := tcpL.Addr().String()
	tlsL := tls.NewListener(tcpL, &tls.Config{Certificates: []tls.Certificate{cert}})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.ServeContext(ctx, tlsL) }()
	waitRunning(t, app)

	resp, err := httpsClient(t, certFile).Get("https://" + addr + "/ping")
	if err != nil {
		cancel()
		<-errCh
		t.Fatalf("HTTPS request via tls.NewListener failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("ServeContext() returned error: %v", err)
	}
}
