//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// testPKI holds paths to temp PEM files generated for a single test.
type testPKI struct {
	caCertFile     string
	serverCertFile string
	serverKeyFile  string
	clientCertFile string
	clientKeyFile  string
}

// generateTestPKI creates an ephemeral CA, a server cert signed by the CA
// (with 127.0.0.1 in the IP SAN so the spoke's TLS client can verify it),
// and a client cert signed by the CA (CN = spokeID, for LD-21 binding).
// All key material is written to dir and cleaned up by t.Cleanup.
func generateTestPKI(t *testing.T, dir, spokeID string) testPKI {
	t.Helper()

	// CA
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
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	// Server cert — IP SAN for 127.0.0.1 so the spoke's TLS client can verify it.
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "hub"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}

	// Client cert — CN = spokeID so the hub's LD-21 check passes.
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: spokeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTmpl, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}

	write := func(name string, blocks ...*pem.Block) string {
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		defer f.Close()
		for _, b := range blocks {
			if err := pem.Encode(f, b); err != nil {
				t.Fatalf("pem encode %s: %v", name, err)
			}
		}
		return path
	}

	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}

	return testPKI{
		caCertFile:     write("ca.pem", &pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		serverCertFile: write("server.crt", &pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		serverKeyFile:  write("server.key", &pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}),
		clientCertFile: write("client.crt", &pem.Block{Type: "CERTIFICATE", Bytes: clientDER}),
		clientKeyFile:  write("client.key", &pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER}),
	}
}

// waitForTCP polls addr until a TCP connection succeeds or the deadline passes.
func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub TCP port %s not open after %s", addr, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHubSpokeEndToEnd exercises the complete federation wire path:
//   - Ephemeral PKI is generated (CA, hub server cert, spoke client cert).
//   - hub.Serve starts an mTLS server on a random localhost port (LD-20).
//   - federation.NewSpoke builds an mTLS client from the same CA and client cert.
//   - spoke.Push sends a SpokePayload containing one device and one edge.
//   - The test verifies the hub's Prometheus metrics reflect the pushed topology
//     and that the LD-21 spoke_id/cert-CN binding was exercised (hub accepted
//     the push because the cert CN matches the spoke_id).
func TestHubSpokeEndToEnd(t *testing.T) {
	const spokeID = "integration-spoke"

	dir := t.TempDir()
	pki := generateTestPKI(t, dir, spokeID)

	// Claim a free port on loopback, then release it so hub.Serve can bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	hubAddr := ln.Addr().String()
	ln.Close()

	hubCfg := config.FederationConfig{
		SpokeTimeout: 5 * time.Minute,
		Hub: config.FederationHubConfig{
			ListenAddr: hubAddr,
			TLSCACert:  pki.caCertFile,
			TLSCert:    pki.serverCertFile,
			TLSKey:     pki.serverKeyFile,
		},
	}

	m := metrics.New(false)
	hub := federation.NewHub(hubCfg, m, slog.Default(), "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hubErr := make(chan error, 1)
	go func() { hubErr <- hub.Serve(ctx) }()

	// Wait for the hub's TCP port to be ready before creating the spoke.
	waitForTCP(t, hubAddr, 5*time.Second)

	spokeCfg := config.FederationConfig{
		Spoke: config.FederationSpokeConfig{
			SpokeID:   spokeID,
			HubURL:    "https://" + hubAddr,
			TLSCACert: pki.caCertFile,
			TLSCert:   pki.clientCertFile,
			TLSKey:    pki.clientKeyFile,
		},
	}
	spoke, err := federation.NewSpoke(spokeCfg, slog.Default(), nil, nil)
	if err != nil {
		t.Fatalf("NewSpoke: %v", err)
	}

	payload := federation.SpokePayload{
		SpokeID: spokeID,
		CycleAt: time.Now(),
		Devices: []discovery.Device{
			{ID: "sw-e2e", Vendor: "cisco", Model: "catalyst", OSVersion: "17.x", Site: "dc-test"},
		},
		Edges: []discovery.Edge{
			{
				SrcDevice: "sw-e2e", SrcPort: "Gi0/1",
				DstDevice: "sw-peer", DstPort: "Gi0/2",
				DiscoveryProto: "lldp",
				LinkKind:       "ethernet",
				Direction:      discovery.DirectionUnidirectional,
				PrecedenceRank: 1,
			},
		},
	}

	if err := spoke.Push(ctx, payload); err != nil {
		t.Fatalf("spoke.Push: %v", err)
	}

	// Shut down the hub now that the push has been accepted.
	cancel()
	if err := <-hubErr; err != nil {
		t.Logf("hub.Serve exited: %v", err) // ErrServerClosed is expected
	}

	// The hub should have published device and edge metrics from the pushed payload.
	const wantDevice = `
# HELP network_topology_device_info One series per discovered device. Value is always 1; inventory data is in the labels. sys_name is the case-preserving sysName, added for joining against Alloy/snmp_exporter's own sysName label — see docs/proposals/snmp-exporter-label-alignment.md.
# TYPE network_topology_device_info gauge
network_topology_device_info{device_id="sw-e2e",model="catalyst",os_version="17.x",site="dc-test",sys_name="",vendor="cisco"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(wantDevice), "network_topology_device_info"); err != nil {
		t.Errorf("DeviceInfo mismatch: %v", err)
	}
	if got := testutil.ToFloat64(m.FederationSpokeUp.WithLabelValues(spokeID)); got != 1 {
		t.Errorf("FederationSpokeUp{%s} = %v, want 1", spokeID, got)
	}
	if !hub.IsReady() {
		t.Error("hub.IsReady() = false after a successful push, want true")
	}
}
