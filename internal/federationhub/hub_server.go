package federationhub

// mTLS federation server bootstrap and lifecycle. Split from hub.go (#168) —
// same-package move, no behaviour change.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// Serve starts the mTLS federation server and the spoke-eviction goroutine,
// blocking until ctx is cancelled. Per LD-20, connections without a valid
// client certificate signed by the configured CA are rejected at the TLS
// handshake before any payload is read.
func (h *Hub) Serve(ctx context.Context) error {
	caCert, err := os.ReadFile(h.cfg.Hub.TLSCACert)
	if err != nil {
		return fmt.Errorf("hub: read CA cert: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return fmt.Errorf("hub: no CA certs parsed from %q", h.cfg.Hub.TLSCACert)
	}
	serverCert, err := tls.LoadX509KeyPair(h.cfg.Hub.TLSCert, h.cfg.Hub.TLSKey)
	if err != nil {
		return fmt.Errorf("hub: load server cert/key: %w", err)
	}
	tlsCfg := &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS13,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/spoke/push", h.handlePush)

	srv := &http.Server{
		Addr:              h.cfg.Hub.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", h.cfg.Hub.ListenAddr)
	if err != nil {
		return fmt.Errorf("hub: listen on %s: %w", h.cfg.Hub.ListenAddr, err)
	}
	tlsLn := tls.NewListener(ln, tlsCfg)

	go h.runEviction(ctx)
	if h.snapshotCh != nil {
		go h.runSnapshotWriter(ctx)
	}

	go func() { //nolint:gosec
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	h.logger.Info("hub federation server listening", "addr", h.cfg.Hub.ListenAddr)
	if err := srv.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("hub server: %w", err)
	}
	return nil
}
