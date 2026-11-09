package credo

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writePair writes a fresh self-signed certificate and key into dir as
// cert.pem and key.pem, with modification time mtime.
func writePair(t *testing.T, dir string, mtime time.Time) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeAt(t, filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), mtime)
	writeAt(t, filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), mtime)
}

// writeAt writes data to path and sets its modification time.
func writeAt(t *testing.T, path string, data []byte, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// certSourceFixture is a fileCertSource on a fake clock with a captured log.
type certSourceFixture struct {
	src   *fileCertSource
	clock time.Time
	logs  *bytes.Buffer
	mu    sync.Mutex
}

func newCertSourceFixture(t *testing.T, certFile, keyFile string) *certSourceFixture {
	t.Helper()
	f := &certSourceFixture{src: &fileCertSource{}, clock: time.Unix(1_000_000, 0), logs: &bytes.Buffer{}}
	f.src.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.clock
	}
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &f.mu, w: f.logs}, nil))
	if err := f.src.load(certFile, keyFile, logger); err != nil {
		t.Fatalf("load: %v", err)
	}
	return f
}

// lockedWriter serializes writes to the fixture's log buffer.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (f *certSourceFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

func (f *certSourceFixture) served(t *testing.T) *tls.Certificate {
	t.Helper()
	c, err := f.src.getCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *certSourceFixture) logCount(msg string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Count(f.logs.String(), msg)
}

const failedLoadMsg = "TLS certificate pair failed to load after a change on disk"

func TestFileCertSource_RotatesAfterTheInterval(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	writePair(t, dir, base)
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	f := newCertSourceFixture(t, cert, key)
	first := f.served(t)

	writePair(t, dir, base.Add(time.Minute))
	f.advance(certCheckInterval / 2)
	if f.served(t) != first {
		t.Fatal("the pair changed before the check interval had passed")
	}
	f.advance(certCheckInterval / 2)
	if f.served(t) == first {
		t.Fatal("the rewritten pair was not served at the first handshake after the interval")
	}
	if f.logCount("TLS certificate reloaded") != 1 {
		t.Errorf("rotation log missing:\n%s", f.logs)
	}
}

func TestFileCertSource_MismatchedPairKeepsTheOldOne(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	base := time.Now().Add(-time.Hour)
	writePair(t, dir, base)
	writePair(t, other, base)
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	f := newCertSourceFixture(t, cert, key)
	first := f.served(t)

	// Half of a new pair: the certificate is written, the key not yet.
	newCert, err := os.ReadFile(filepath.Join(other, "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, cert, newCert, base.Add(time.Minute))
	f.advance(certCheckInterval)
	if f.served(t) != first {
		t.Fatal("a pair whose key does not match its certificate replaced the old pair")
	}
	f.advance(certCheckInterval)
	_ = f.served(t)
	if n := f.logCount(failedLoadMsg); n != 1 {
		t.Fatalf("failed load logged %d times, want once per change:\n%s", n, f.logs)
	}
	logs := f.logs.String()
	for _, part := range []string{"cert_file=", "cert.pem", "key_file=", "key.pem", "error="} {
		if !strings.Contains(logs, part) {
			t.Errorf("failed-load log does not contain %q:\n%s", part, logs)
		}
	}

	// The key arrives: the second file's change retries the load.
	newKey, err := os.ReadFile(filepath.Join(other, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, key, newKey, base.Add(2*time.Minute))
	f.advance(certCheckInterval)
	if f.served(t) == first {
		t.Fatal("the fixed pair was not picked up when its second file changed")
	}
}

func TestFileCertSource_LinkSwapIsAChange(t *testing.T) {
	root := t.TempDir()
	base := time.Now().Add(-time.Hour)
	// The same modification time and sizes in both directories: only the
	// files' identity tells them apart, as after a Secret volume update.
	writePair(t, filepath.Join(root, "data-1"), base)
	writePair(t, filepath.Join(root, "data-2"), base)
	link := filepath.Join(root, "current")
	if err := os.Symlink(filepath.Join(root, "data-1"), link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	f := newCertSourceFixture(t, filepath.Join(link, "cert.pem"), filepath.Join(link, "key.pem"))
	first := f.served(t)

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "data-2"), link); err != nil {
		t.Fatal(err)
	}
	f.advance(certCheckInterval)
	if f.served(t) == first {
		t.Fatal("a link swapped to a new directory was not seen as a change")
	}
}

func TestFileCertSource_OneCheckPerInterval(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, time.Now().Add(-time.Hour))
	f := newCertSourceFixture(t, filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	f.advance(certCheckInterval)

	var wg sync.WaitGroup
	var nilCerts atomic.Int32
	for range 64 {
		wg.Go(func() {
			if c, _ := f.src.getCertificate(nil); c == nil {
				nilCerts.Add(1)
			}
		})
	}
	wg.Wait()
	if got := f.src.checks.Load(); got != 1 {
		t.Errorf("concurrent handshakes ran %d checks in one interval, want 1", got)
	}
	if nilCerts.Load() != 0 {
		t.Error("a handshake was served no certificate")
	}
}
