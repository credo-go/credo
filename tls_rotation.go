package credo

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// certCheckInterval bounds how often a handshake checks the certificate files.
const certCheckInterval = time.Second

// fileCertSource serves the key pair loaded from a PEM file pair through
// tls.Config.GetCertificate and rotates it when the files change on disk
// (ADR-020). The handshake that finds the last check older than
// certCheckInterval stats both files and compares them with the last load by
// os.SameFile, modification time and size; concurrent handshakes serve the
// current pair without waiting. A change loads the pair and swaps it
// atomically: new handshakes see the new certificate, open connections are
// unaffected, and a pair that fails to load keeps the old one and is logged
// once per change, so a half-written pair is retried when its other file
// changes. os.Stat follows symbolic links, so the link a Kubernetes Secret
// volume swaps on an update is a change.
//
// The zero value is inactive; preflight activates it by loading the initial
// pair, which keeps the startup failure modes identical to loading into
// tls.Config.Certificates. The paths are read once, at start.
type fileCertSource struct {
	cur atomic.Pointer[tls.Certificate]

	// lastCheck is the time of the last check, in Unix nanoseconds; the
	// handshake that advances it runs the check. checks counts the checks run.
	lastCheck atomic.Int64
	checks    atomic.Int64

	// now and logger are set by preflight; now is replaceable in tests.
	now    func() time.Time
	logger *slog.Logger

	// mu guards the paths and the file signatures. Only the handshake that won
	// the check holds it there; preflight, which may run again after a
	// rollback, holds it too.
	mu       sync.Mutex
	certFile string
	keyFile  string
	seen     pairSignature // the files as last checked, loaded or not
}

// pairSignature identifies the two files as a check saw them.
type pairSignature struct {
	cert, key os.FileInfo
	err       string // a stat failure, so it is logged once
}

// same reports whether two signatures name the same files unchanged.
func (p pairSignature) same(o pairSignature) bool {
	return p.err == o.err && sameFile(p.cert, o.cert) && sameFile(p.key, o.key)
}

func sameFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

// statPair takes the signature of the pair. On some platforms (Windows)
// os.SameFile reads a file's identity lazily, by opening its path again,
// which after a link swap would resolve to the new target; comparing each
// info with itself reads the identity now, while the path still names the
// file this stat saw.
func statPair(certFile, keyFile string) pairSignature {
	cert, cerr := os.Stat(certFile)
	key, kerr := os.Stat(keyFile)
	if err := errors.Join(cerr, kerr); err != nil {
		return pairSignature{err: err.Error()}
	}
	_, _ = os.SameFile(cert, cert), os.SameFile(key, key)
	return pairSignature{cert: cert, key: key}
}

// active reports whether a pair has been loaded, i.e. the server serves
// file-based TLS.
func (s *fileCertSource) active() bool { return s.cur.Load() != nil }

// getCertificate is the tls.Config.GetCertificate callback: an atomic load per
// handshake, and a file check when the last one is older than the interval.
func (s *fileCertSource) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	now := s.now().UnixNano()
	last := s.lastCheck.Load()
	if now-last >= int64(certCheckInterval) && s.lastCheck.CompareAndSwap(last, now) {
		s.check()
	}
	return s.cur.Load(), nil
}

// check reloads the pair when its files changed since the last check.
func (s *fileCertSource) check() {
	s.checks.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	sig := statPair(s.certFile, s.keyFile)
	if sig.same(s.seen) {
		return
	}
	s.seen = sig
	ctx := context.Background()
	if sig.err != "" {
		s.logger.LogAttrs(ctx, slog.LevelError, "credo: TLS certificate files changed but cannot be read; keeping the current pair",
			slog.String("cert_file", s.certFile), slog.String("key_file", s.keyFile), slog.String("error", sig.err))
		return
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		s.logger.LogAttrs(ctx, slog.LevelError, "credo: TLS certificate pair failed to load after a change on disk; keeping the current pair",
			slog.String("cert_file", s.certFile), slog.String("key_file", s.keyFile), slog.String("error", err.Error()))
		return
	}
	s.cur.Store(&cert)
	s.logger.LogAttrs(ctx, slog.LevelInfo, "credo: TLS certificate reloaded",
		slog.String("cert_file", s.certFile), slog.String("key_file", s.keyFile))
}

// load reads the pair from certFile/keyFile at preflight and, on success,
// publishes it, remembers the paths and the files' signature, and arms the
// check. On failure nothing changes.
func (s *fileCertSource) load(certFile, keyFile string, logger *slog.Logger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sig := statPair(certFile, keyFile)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("load TLS key pair: %w", err)
	}
	s.certFile, s.keyFile, s.seen = certFile, keyFile, sig
	s.logger = logger
	if s.now == nil {
		s.now = time.Now
	}
	s.lastCheck.Store(s.now().UnixNano())
	s.cur.Store(&cert)
	return nil
}
