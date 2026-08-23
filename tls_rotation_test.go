package credo_test

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/credo-go/credo"
)

// copyFile overwrites dst with src's bytes, simulating a certificate rotated
// in place by an ACME client or a deploy hook.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertServes makes one HTTPS GET /ping trusting only certFile and reports
// whether the handshake succeeded — i.e. whether that exact certificate is the
// one currently served. Each call opens a fresh connection (no keep-alive
// reuse) so it observes the current GetCertificate result.
func assertServes(t *testing.T, app *credo.App, certFile string, want bool) {
	t.Helper()
	client := httpsClient(t, certFile)
	client.Transport.(*http.Transport).DisableKeepAlives = true
	resp, err := client.Get("https://" + app.Addr().String() + "/ping")
	if err == nil {
		resp.Body.Close()
	}
	if got := err == nil; got != want {
		t.Fatalf("serving cert %s = %v, want %v (err=%v)", filepath.Base(filepath.Dir(certFile)), got, want, err)
	}
}

// tlsYAML renders a server.tls section for the reload fixture's YAML file.
func tlsYAML(certFile, keyFile string) string {
	return fmt.Sprintf("server:\n  tls:\n    cert_file: '%s'\n    key_file: '%s'\n",
		filepath.ToSlash(certFile), filepath.ToSlash(keyFile))
}

// TestReload_TLSFiles_RotatesInPlace is the ADR-020 rotation path for
// WithTLSFiles: the pair is rewritten on disk with no config change, a reload
// picks it up, new handshakes see the new certificate, and the old one is no
// longer served.
func TestReload_TLSFiles_RotatesInPlace(t *testing.T) {
	certA, keyA := generateSelfSignedCert(t)
	certB, keyB := generateSelfSignedCert(t)
	f := newReloadFixture(t, "feature: 1\n", credo.WithTLSFiles(certA, keyA))
	f.app.GET("/ping", pongHandler)
	f.start(t)

	assertServes(t, f.app, certA, true)
	assertServes(t, f.app, certB, false)

	// Rotate in place: same paths, new material; nothing in the config diff.
	copyFile(t, certB, certA)
	copyFile(t, keyB, keyA)
	if err := f.app.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	assertServes(t, f.app, certB, true)
	if !strings.Contains(f.logs.String(), "credo: TLS certificate reloaded") {
		t.Errorf("missing rotation log:\n%s", f.logs)
	}
}

// TestReload_TLSFiles_BadPairKeepsCurrent locks in the failure mode: a
// corrupt key on disk makes Reload return an error, and the previous pair
// keeps serving.
func TestReload_TLSFiles_BadPairKeepsCurrent(t *testing.T) {
	certA, keyA := generateSelfSignedCert(t)
	f := newReloadFixture(t, "feature: 1\n", credo.WithTLSFiles(certA, keyA))
	f.app.GET("/ping", pongHandler)
	f.start(t)

	if err := os.WriteFile(keyA, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := f.app.Reload(t.Context())
	if err == nil || !strings.Contains(err.Error(), "tls rotation") {
		t.Fatalf("Reload error = %v, want a tls rotation error", err)
	}
	if !f.app.IsRunning() {
		t.Fatal("a failed rotation must not stop the server")
	}
	assertServes(t, f.app, certA, true)
}

// TestReload_TLSConfigKeys_FollowsNewPaths covers the server.tls.* source:
// a reload that changes the paths in the config file moves the served pair to
// the new location, and a subsequent in-place rotation at the new paths works
// too. The changed keys are owned by the participant, so no restart-required
// warning is logged.
func TestReload_TLSConfigKeys_FollowsNewPaths(t *testing.T) {
	certA, keyA := generateSelfSignedCert(t)
	certB, keyB := generateSelfSignedCert(t)
	f := newReloadFixture(t, tlsYAML(certA, keyA))
	f.app.GET("/ping", pongHandler)
	f.start(t)
	assertServes(t, f.app, certA, true)

	f.rewrite(t, tlsYAML(certB, keyB))
	if err := f.app.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	assertServes(t, f.app, certB, true)
	assertServes(t, f.app, certA, false)
	if strings.Contains(f.logs.String(), "restart required") {
		t.Errorf("server.tls keys are owned by the rotation participant; got restart-required warning:\n%s", f.logs)
	}

	// A partial pair in the new snapshot is rejected and the current pair stays.
	f.rewrite(t, fmt.Sprintf("server:\n  tls:\n    cert_file: '%s'\n", filepath.ToSlash(certA)))
	if err := f.app.Reload(t.Context()); err == nil || !strings.Contains(err.Error(), "requires both") {
		t.Fatalf("Reload error = %v, want partial-pair rejection", err)
	}
	assertServes(t, f.app, certB, true)
}

// TestReload_WithTLSConfig_Untouched verifies the caller-owned source is left
// alone: Reload succeeds, the participant does nothing, and changed
// server.tls.* keys are reported as restart-required because no active
// participant owns them.
func TestReload_WithTLSConfig_Untouched(t *testing.T) {
	certA, keyA := generateSelfSignedCert(t)
	certB, keyB := generateSelfSignedCert(t)
	cfg := &tls.Config{Certificates: []tls.Certificate{tlsCertificate(t, certA, keyA)}}
	f := newReloadFixture(t, tlsYAML(certA, keyA), credo.WithTLSConfig(cfg))
	f.app.GET("/ping", pongHandler)
	f.start(t)
	assertServes(t, f.app, certA, true)

	f.rewrite(t, tlsYAML(certB, keyB))
	if err := f.app.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	assertServes(t, f.app, certA, true)
	logs := f.logs.String()
	if strings.Contains(logs, "TLS certificate reloaded") {
		t.Errorf("WithTLSConfig must not be rotated by the framework:\n%s", logs)
	}
	if !strings.Contains(logs, "restart required") {
		t.Errorf("server.tls keys changed under WithTLSConfig should be restart-required:\n%s", logs)
	}
}
