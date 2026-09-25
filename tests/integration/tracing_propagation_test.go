//go:build integration

package integration

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/federationhub"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// installTracing installs an always-sample SDK TracerProvider backed by a
// SpanRecorder plus the W3C TraceContext propagator, restoring the previous
// globals at test end. Mirrors what tracing.New does in production but without
// an OTLP exporter so the test stays in-memory.
func installTracing(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return sr
}

// TestSpokeToHubTraceparentPropagation proves the spoke→hub HTTP push carries
// the W3C traceparent end to end: the spoke injects its spoke.push span's
// trace context into the outbound request headers, and the hub's handlePush
// extracts it so hub.handlePush shares the spoke.push trace ID. This is the
// propagation acceptance test for issue #68.
//
// Relocated here from internal/federation/tracing_propagation_test.go by the
// core/hub binary split (docs/proposals/core-hub-split.md §3.2): the hub side
// (now federationhub.NewHub) and the spoke side (federation.NewSpoke) live in
// two different packages that cannot import each other outside of a test
// binary, so this cross-package acceptance test moved to tests/integration
// alongside TestHubSpokeEndToEnd. It now drives a real mTLS hub.Serve
// listener via the public API (like TestHubSpokeEndToEnd) instead of
// wrapping the package-internal hub.handlePush directly in an
// httptest.Server — the only way to reach the real hub type once it left
// internal/federation. The assertions are unchanged: same two span names,
// same trace-ID/parent-ID checks as before the move.
func TestSpokeToHubTraceparentPropagation(t *testing.T) {
	sr := installTracing(t)

	const spokeID = "trace-test-spoke"
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
		SpokeTimeout: time.Minute,
		Hub: config.FederationHubConfig{
			ListenAddr: hubAddr,
			TLSCACert:  pki.caCertFile,
			TLSCert:    pki.serverCertFile,
			TLSKey:     pki.serverKeyFile,
		},
	}
	h := federationhub.NewHub(hubCfg, metrics.New(false), slog.Default(), "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hubErr := make(chan error, 1)
	go func() { hubErr <- h.Serve(ctx) }()
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
	s, err := federation.NewSpoke(spokeCfg, slog.Default(), nil, nil)
	if err != nil {
		t.Fatalf("NewSpoke: %v", err)
	}

	err = s.Push(context.Background(), federation.SpokePayload{
		SpokeID: spokeID,
		CycleAt: time.Now(),
		Devices: []discovery.Device{{ID: "sw-1"}},
		Edges: []discovery.Edge{{
			SrcDevice:      "sw-1",
			SrcPort:        "Gi0/1",
			DstDevice:      "sw-2",
			DiscoveryProto: discovery.DiscoveryProtocolLLDP,
			Direction:      discovery.DirectionUnidirectional,
		}},
	})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Shut down the hub now that the push has been accepted.
	cancel()
	if err := <-hubErr; err != nil {
		t.Logf("hub.Serve exited: %v", err) // ErrServerClosed is expected
	}

	var spokeSpan, hubSpan sdktrace.ReadOnlySpan
	for _, sp := range sr.Ended() {
		switch sp.Name() {
		case "spoke.push":
			spokeSpan = sp
		case "hub.handlePush":
			hubSpan = sp
		}
	}
	if spokeSpan == nil {
		t.Fatal("no spoke.push span recorded")
	}
	if hubSpan == nil {
		t.Fatal("no hub.handlePush span recorded")
	}

	spokeTID := spokeSpan.SpanContext().TraceID()
	hubTID := hubSpan.SpanContext().TraceID()
	if spokeTID != hubTID {
		t.Errorf("trace ID mismatch: spoke.push %s != hub.handlePush %s", spokeTID, hubTID)
	}
	// The hub span's parent must be the spoke.push span (continued across the
	// wire via traceparent).
	if hubSpan.Parent().SpanID() != spokeSpan.SpanContext().SpanID() {
		t.Errorf("hub.handlePush parent = %s, want spoke.push %s",
			hubSpan.Parent().SpanID(), spokeSpan.SpanContext().SpanID())
	}
}
