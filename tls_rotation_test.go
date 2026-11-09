package credo_test

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// eventually polls cond until it holds or the deadline passes, so a test can
// wait out the certificate check interval.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 5s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// serves reports whether one HTTPS GET /ping trusting only certFile succeeds.
func serves(t *testing.T, app *credo.App, certFile string) bool {
	t.Helper()
	client := httpsClient(t, certFile)
	client.Transport.(*http.Transport).DisableKeepAlives = true
	resp, err := client.Get("https://" + app.Addr().String() + "/ping")
	if err == nil {
		resp.Body.Close()
	}
	return err == nil
}

// TestTLSFiles_RotatesOnChange is the end-to-end rotation path for
// WithTLSFiles: the pair is rewritten on disk, no signal and no reload is
// sent, and a handshake after the check interval serves the new certificate.
func TestTLSFiles_RotatesOnChange(t *testing.T) {
	certA, keyA := generateSelfSignedCert(t)
	certB, keyB := generateSelfSignedCert(t)
	f := newReloadFixture(t, "feature: 1\n", credo.WithTLSFiles(certA, keyA))
	f.app.GET("/ping", pongHandler)
	f.start(t)

	assertServes(t, f.app, certA, true)
	assertServes(t, f.app, certB, false)

	copyFile(t, certB, certA)
	copyFile(t, keyB, keyA)
	eventually(t, "serving the rotated certificate", func() bool { return serves(t, f.app, certB) })
	if !strings.Contains(f.logs.String(), "credo: TLS certificate reloaded") {
		t.Errorf("missing rotation log:\n%s", f.logs)
	}
}

// TestTLSFiles_BadPairKeepsCurrent locks in the failure mode: a corrupt key
// on disk is logged once, the previous pair keeps serving, and the server
// keeps running.
func TestTLSFiles_BadPairKeepsCurrent(t *testing.T) {
	certA, keyA := generateSelfSignedCert(t)
	f := newReloadFixture(t, "feature: 1\n", credo.WithTLSFiles(certA, keyA))
	f.app.GET("/ping", pongHandler)
	f.start(t)

	if err := os.WriteFile(keyA, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the failed load being logged", func() bool {
		assertServes(t, f.app, certA, true)
		return strings.Contains(f.logs.String(), "failed to load after a change on disk")
	})
	if !f.app.IsRunning() {
		t.Fatal("a failed rotation must not stop the server")
	}
}

// TestReload_TLSPathsTakeARestart covers the server.tls.* source: the paths
// are read at start, so a reload that changes them reports a restart and the
// served pair stays where it was.
func TestReload_TLSPathsTakeARestart(t *testing.T) {
	certA, keyA := generateSelfSignedCert(t)
	certB, keyB := generateSelfSignedCert(t)
	f := newReloadFixture(t, tlsYAML(certA, keyA))
	f.app.GET("/ping", pongHandler)
	f.start(t)
	assertServes(t, f.app, certA, true)

	f.rewrite(t, tlsYAML(certB, keyB))
	res, err := f.app.Reload(t.Context())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if want := []string{"server.tls.cert_file", "server.tls.key_file"}; !slices.Equal(res.RestartRequired, want) {
		t.Errorf("RestartRequired = %v, want %v", res.RestartRequired, want)
	}
	assertServes(t, f.app, certA, true)
	assertServes(t, f.app, certB, false)
}

// TestTLSConfig_Untouched verifies the caller-owned source is left alone:
// rewriting the files it was built from changes nothing served.
func TestTLSConfig_Untouched(t *testing.T) {
	certA, keyA := generateSelfSignedCert(t)
	certB, keyB := generateSelfSignedCert(t)
	cfg := &tls.Config{Certificates: []tls.Certificate{tlsCertificate(t, certA, keyA)}}
	f := newReloadFixture(t, "feature: 1\n", credo.WithTLSConfig(cfg))
	f.app.GET("/ping", pongHandler)
	f.start(t)
	assertServes(t, f.app, certA, true)

	copyFile(t, certB, certA)
	copyFile(t, keyB, keyA)
	time.Sleep(1200 * time.Millisecond) // past the check interval of a file source
	assertServes(t, f.app, certB, false)
	if strings.Contains(f.logs.String(), "TLS certificate reloaded") {
		t.Errorf("WithTLSConfig must not be rotated by the framework:\n%s", f.logs)
	}
}
