package federationhub

// Tests split from hub_test.go (#168); see hub_server.go.
import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// waitForAddr polls addr with TCP dials until the server is accepting connections
// or the 2-second deadline expires. Replaces time.Sleep-based readiness checks.
func waitForAddr(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not become ready at %s", addr)
}

// writeTLSFiles generates a self-signed CA + server cert/key and writes them as
// PEM files under dir. Returns paths (caPath, certPath, keyPath). Suitable for
// populating config.FederationHubConfig in Serve() tests.
func writeTLSFiles(t *testing.T, dir string) (caPath, certPath, keyPath string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-hub"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}

	srvKeyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}

	caPath = filepath.Join(dir, "ca.crt")
	certPath = filepath.Join(dir, "server.crt")
	keyPath = filepath.Join(dir, "server.key")

	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}), 0o600); err != nil {
		t.Fatalf("write server cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyDER}), 0o600); err != nil {
		t.Fatalf("write server key: %v", err)
	}
	return caPath, certPath, keyPath
}

// TestServeErrorMissingCAFile verifies that Serve returns an error immediately
// when the CA cert file does not exist.
func TestServeErrorMissingCAFile(t *testing.T) {
	h := NewHub(
		config.FederationConfig{
			Hub: config.FederationHubConfig{
				TLSCACert:  "/nonexistent/ca.crt",
				TLSCert:    "/nonexistent/server.crt",
				TLSKey:     "/nonexistent/server.key",
				ListenAddr: "127.0.0.1:0",
			},
		},
		metrics.New(false), nil, "",
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately — error paths return before blocking

	err := h.Serve(ctx)
	if err == nil {
		t.Fatal("Serve: expected error for missing CA file, got nil")
	}
	if !strings.Contains(err.Error(), "read CA cert") {
		t.Errorf("error = %q, want to contain 'read CA cert'", err.Error())
	}
}

// TestServeErrorNoPEMInCAFile verifies that Serve returns an error when the CA
// cert file exists but contains no valid PEM blocks.
func TestServeErrorNoPEMInCAFile(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, []byte("not a pem block"), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}

	h := NewHub(
		config.FederationConfig{
			Hub: config.FederationHubConfig{
				TLSCACert:  caPath,
				TLSCert:    filepath.Join(dir, "server.crt"),
				TLSKey:     filepath.Join(dir, "server.key"),
				ListenAddr: "127.0.0.1:0",
			},
		},
		metrics.New(false), nil, "",
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := h.Serve(ctx)
	if err == nil {
		t.Fatal("Serve: expected error for non-PEM CA file, got nil")
	}
	if !strings.Contains(err.Error(), "no CA certs parsed") {
		t.Errorf("error = %q, want to contain 'no CA certs parsed'", err.Error())
	}
}

// TestServeErrorInvalidCertKeyPair verifies that Serve returns an error when
// the server cert/key pair cannot be loaded (corrupt key file).
func TestServeErrorInvalidCertKeyPair(t *testing.T) {
	dir := t.TempDir()
	caPath, certPath, _ := writeTLSFiles(t, dir)

	// Replace the key with garbage so LoadX509KeyPair fails.
	garbageKeyPath := filepath.Join(dir, "garbage.key")
	if err := os.WriteFile(garbageKeyPath, []byte("not a real key"), 0o600); err != nil {
		t.Fatalf("write garbage key: %v", err)
	}

	h := NewHub(
		config.FederationConfig{
			Hub: config.FederationHubConfig{
				TLSCACert:  caPath,
				TLSCert:    certPath,
				TLSKey:     garbageKeyPath,
				ListenAddr: "127.0.0.1:0",
			},
		},
		metrics.New(false), nil, "",
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := h.Serve(ctx)
	if err == nil {
		t.Fatal("Serve: expected error for invalid cert/key pair, got nil")
	}
	if !strings.Contains(err.Error(), "load server cert/key") {
		t.Errorf("error = %q, want to contain 'load server cert/key'", err.Error())
	}
}

// TestServeErrorListenAddrInUse verifies that Serve returns an error when the
// listen address is already bound by another listener.
func TestServeErrorListenAddrInUse(t *testing.T) {
	// Bind a port first so the hub's net.Listen call fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind listener: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().String()

	dir := t.TempDir()
	caPath, certPath, keyPath := writeTLSFiles(t, dir)

	h := NewHub(
		config.FederationConfig{
			Hub: config.FederationHubConfig{
				TLSCACert:  caPath,
				TLSCert:    certPath,
				TLSKey:     keyPath,
				ListenAddr: addr,
			},
		},
		metrics.New(false), nil, "",
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = h.Serve(ctx)
	if err == nil {
		t.Fatal("Serve: expected error for already-bound listen address, got nil")
	}
	if !strings.Contains(err.Error(), "listen on") {
		t.Errorf("error = %q, want to contain 'listen on'", err.Error())
	}
}

// TestServeStartsAndShutsDownCleanly verifies that Serve starts the mTLS server
// and returns nil when the context is cancelled (i.e. http.ErrServerClosed is
// swallowed). This covers the happy path through srv.Serve and the shutdown
// goroutine.
func TestServeStartsAndShutsDownCleanly(t *testing.T) {
	dir := t.TempDir()
	caPath, certPath, keyPath := writeTLSFiles(t, dir)

	// Probe for a free port then release it so Serve can bind the same address.
	// Note: there is an inherent TOCTOU window between Close and Serve's
	// net.Listen — this is test-only and accepted as a rare flake rather than a
	// production concern. The window is kept as small as possible by constructing
	// the Hub before closing the listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-allocate port: %v", err)
	}
	addr := ln.Addr().String()

	h := NewHub(
		config.FederationConfig{
			SpokeTimeout: 5 * time.Minute,
			Hub: config.FederationHubConfig{
				TLSCACert:  caPath,
				TLSCert:    certPath,
				TLSKey:     keyPath,
				ListenAddr: addr,
			},
		},
		metrics.New(false), nil, "",
	)

	_ = ln.Close() // release immediately before Serve binds to minimise the race window

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- h.Serve(ctx) }()

	// Wait until the server is actually accepting connections before cancelling,
	// replacing the fixed 50 ms sleep which was non-deterministic.
	waitForAddr(t, addr)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Serve did not return within 3s after context cancellation")
	}
}
