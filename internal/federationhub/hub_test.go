package federationhub

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// makeCert returns a self-signed certificate with the given Common Name,
// suitable for populating req.TLS.PeerCertificates in handlePush tests.
func makeCert(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}

func newTestHub(links []config.InterDomainLink) *Hub {
	return NewHub(
		config.FederationConfig{
			SpokeTimeout:          3 * time.Minute,
			KnownInterDomainLinks: links,
		},
		metrics.New(false),
		nil,
		"", // no snapshot path in tests
	)
}

// TestHubIsReadyFalseBeforeAnyPush verifies that a freshly constructed Hub
// returns false from IsReady() before any spoke has pushed.
func TestHubIsReadyFalseBeforeAnyPush(t *testing.T) {
	h := newTestHub(nil)
	if h.IsReady() {
		t.Error("IsReady() = true on new hub, want false")
	}
}

// TestHubIsReadyTrueAfterPush verifies that IsReady() returns true after at
// least one successful spoke push (i.e., after handlePush calls publishMetrics
// with clearStale=true the first time via firstLive).
func TestHubIsReadyTrueAfterPush(t *testing.T) {
	h := newTestHub(nil)

	payload := federation.SpokePayload{SpokeID: "dc-ready", CycleAt: time.Now()}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("handlePush status = %d, want 204", rec.Code)
	}
	if !h.IsReady() {
		t.Error("IsReady() = false after first successful push, want true")
	}
}
