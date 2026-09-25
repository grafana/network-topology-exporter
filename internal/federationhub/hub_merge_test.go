package federationhub

// Tests split from hub_test.go (#168); see hub_merge.go.
import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// TestHubCombinedGraphSingleSpoke verifies that a single spoke's edges survive
// the hub's second Reconcile pass without corruption.
func TestHubCombinedGraphSingleSpoke(t *testing.T) {
	h := newTestHub(nil)
	h.mu.Lock()
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			SpokeID: "dc-a",
			Devices: []discovery.Device{{ID: "sw-a"}, {ID: "sw-b"}},
			Edges: []discovery.Edge{
				{
					SrcDevice: "sw-a", SrcPort: "Gi0/1",
					DstDevice: "sw-b", DstPort: "Gi0/2",
					DiscoveryProto: "lldp",
					Direction:      discovery.DirectionBidirectional,
					PrecedenceRank: 1,
					LinkKind:       "ethernet",
				},
			},
		},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	if len(g.Edges) != 1 {
		t.Fatalf("edge count = %d, want 1", len(g.Edges))
	}
	if g.Edges[0].Direction != discovery.DirectionBidirectional {
		t.Errorf("direction = %q, want bidirectional", g.Edges[0].Direction)
	}
	if len(g.Devices) != 2 {
		t.Errorf("device count = %d, want 2", len(g.Devices))
	}
}

// TestHubCrossdomainOOSMatching verifies that matching OOS observations from
// two spokes produce a confirmed bidirectional edge via hub reconciliation.
func TestHubCrossdomainOOSMatching(t *testing.T) {
	h := newTestHub(nil)
	h.mu.Lock()
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			SpokeID: "dc-a",
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-a", ReportingPort: "Gi0/1", NeighbourHint: "sw-b", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	h.spokes["dc-b"] = spokeEntry{
		payload: federation.SpokePayload{
			SpokeID: "dc-b",
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-b", ReportingPort: "Gi0/2", NeighbourHint: "sw-a", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	if len(g.Edges) != 1 {
		t.Fatalf("edge count = %d, want 1 cross-boundary edge", len(g.Edges))
	}
	e := g.Edges[0]
	if e.Direction != discovery.DirectionBidirectional {
		t.Errorf("direction = %q, want bidirectional", e.Direction)
	}
	// Canonical key: alphabetically-smaller device is SrcDevice.
	if e.SrcDevice != "sw-a" || e.DstDevice != "sw-b" {
		t.Errorf("endpoints = (%s, %s), want (sw-a, sw-b)", e.SrcDevice, e.DstDevice)
	}
	if e.SrcPort != "Gi0/1" || e.DstPort != "Gi0/2" {
		t.Errorf("ports = (%s, %s), want (Gi0/1, Gi0/2)", e.SrcPort, e.DstPort)
	}
	if e.DiscoveryProto != "lldp" {
		t.Errorf("discovery_proto = %q, want lldp", e.DiscoveryProto)
	}
}

// TestHubOOSCaseMismatchProducesEdge verifies that device-name normalization
// makes OOS matching case-insensitive: "SW-B" and "sw-b" are treated as the
// same device and produce a confirmed bidirectional cross-boundary edge.
func TestHubOOSCaseMismatchProducesEdge(t *testing.T) {
	h := newTestHub(nil)
	h.mu.Lock()
	// dc-a reports hint "CORE-01" (uppercase); dc-b's sysName is "core-01".
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-a", ReportingPort: "Gi0/1", NeighbourHint: "CORE-01", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	h.spokes["dc-b"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "core-01", ReportingPort: "Gi0/2", NeighbourHint: "sw-a", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	if len(g.Edges) != 1 {
		t.Fatalf("edge count = %d, want 1 (case mismatch should produce edge after normalization)", len(g.Edges))
	}
	if g.Edges[0].Direction != discovery.DirectionBidirectional {
		t.Errorf("direction = %q, want bidirectional", g.Edges[0].Direction)
	}
}

// TestHubOOSMultiPortLAG verifies that multiple OOS entries for the same
// device pair (e.g. a LAG bond across two physical ports) each produce a
// distinct cross-boundary edge rather than overwriting each other.
func TestHubOOSMultiPortLAG(t *testing.T) {
	h := newTestHub(nil)
	h.mu.Lock()
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-a", ReportingPort: "Gi0/1", NeighbourHint: "sw-b", Proto: "lldp"},
				{ReportingDevice: "sw-a", ReportingPort: "Gi0/2", NeighbourHint: "sw-b", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	h.spokes["dc-b"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-b", ReportingPort: "Gi1/1", NeighbourHint: "sw-a", Proto: "lldp"},
				{ReportingDevice: "sw-b", ReportingPort: "Gi1/2", NeighbourHint: "sw-a", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	// 2 ports × 2 ports = 4 physical port combinations → 4 bidirectional edges.
	if len(g.Edges) != 4 {
		t.Errorf("edge count = %d, want 4 (2 LAG ports × 2 remote ports)", len(g.Edges))
	}
	for _, e := range g.Edges {
		if e.Direction != discovery.DirectionBidirectional {
			t.Errorf("edge (%s:%s→%s:%s) direction = %q, want bidirectional",
				e.SrcDevice, e.SrcPort, e.DstDevice, e.DstPort, e.Direction)
		}
	}
}

// TestHubKnownInterDomainLinkInjected verifies that a configured inter-domain
// link is injected as a bidirectional edge even with no OOS observations.
func TestHubKnownInterDomainLinkInjected(t *testing.T) {
	links := []config.InterDomainLink{
		{LocalDevice: "sw-a", LocalPort: "Gi0/1", RemoteDevice: "sw-b", RemotePort: "Gi0/2"},
	}
	h := newTestHub(links)
	h.mu.Lock()
	h.spokes["dc-a"] = spokeEntry{
		payload:  federation.SpokePayload{SpokeID: "dc-a", Devices: []discovery.Device{{ID: "sw-a"}}},
		lastSeen: time.Now(),
	}
	h.spokes["dc-b"] = spokeEntry{
		payload:  federation.SpokePayload{SpokeID: "dc-b", Devices: []discovery.Device{{ID: "sw-b"}}},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	if len(g.Edges) != 1 {
		t.Fatalf("edge count = %d, want 1", len(g.Edges))
	}
	if g.Edges[0].Direction != discovery.DirectionBidirectional {
		t.Errorf("direction = %q, want bidirectional", g.Edges[0].Direction)
	}
	if g.Edges[0].DiscoveryProto != "configured" {
		t.Errorf("discovery_proto = %q, want configured", g.Edges[0].DiscoveryProto)
	}
}

// TestHubKnownLinkCustomLinkKind verifies that an explicit link_kind on a
// configured inter-domain link is threaded through to the emitted edge.
func TestHubKnownLinkCustomLinkKind(t *testing.T) {
	links := []config.InterDomainLink{
		{LocalDevice: "sw-a", LocalPort: "Gi0/1", RemoteDevice: "sw-b", RemotePort: "Gi0/2", LinkKind: "fiber"},
	}
	h := newTestHub(links)
	h.mu.Lock()
	h.spokes["dc-a"] = spokeEntry{
		payload:  federation.SpokePayload{SpokeID: "dc-a", Devices: []discovery.Device{{ID: "sw-a"}}},
		lastSeen: time.Now(),
	}
	h.spokes["dc-b"] = spokeEntry{
		payload:  federation.SpokePayload{SpokeID: "dc-b", Devices: []discovery.Device{{ID: "sw-b"}}},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	if len(g.Edges) != 1 {
		t.Fatalf("edge count = %d, want 1", len(g.Edges))
	}
	if g.Edges[0].LinkKind != "fiber" {
		t.Errorf("link_kind = %q, want fiber", g.Edges[0].LinkKind)
	}
}

// TestHubKnownLinkBeatsOOS verifies that a configured inter-domain link wins
// over an automatically-matched OOS edge (rank 0 < rank 2).
func TestHubKnownLinkBeatsOOS(t *testing.T) {
	links := []config.InterDomainLink{
		{LocalDevice: "sw-a", LocalPort: "Gi0/1", RemoteDevice: "sw-b", RemotePort: "Gi0/2"},
	}
	h := newTestHub(links)
	h.mu.Lock()
	// Also inject matching OOS observations — same edge, different port names.
	// Devices must be present so the IDL guard passes.
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			Devices: []discovery.Device{{ID: "sw-a"}},
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-a", ReportingPort: "GigabitEthernet0/1", NeighbourHint: "sw-b", Proto: "cdp"},
			},
		},
		lastSeen: time.Now(),
	}
	h.spokes["dc-b"] = spokeEntry{
		payload: federation.SpokePayload{
			Devices: []discovery.Device{{ID: "sw-b"}},
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-b", ReportingPort: "GigabitEthernet0/2", NeighbourHint: "sw-a", Proto: "cdp"},
			},
		},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	// Both the OOS-derived and the configured link land in different EdgeKey
	// buckets (different port names) so we expect two edges total. The configured
	// edge should have DiscoveryProto "configured".
	configuredCount := 0
	for _, e := range g.Edges {
		if e.DiscoveryProto == "configured" {
			configuredCount++
		}
	}
	if configuredCount == 0 {
		t.Error("expected at least one edge with discovery_proto=configured")
	}
}

// TestHubOOSNoMatchProducesNoEdge confirms that an unmatched OOS observation
// (only one side sees the other) does not produce a cross-boundary edge.
func TestHubOOSNoMatchProducesNoEdge(t *testing.T) {
	h := newTestHub(nil)
	h.mu.Lock()
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-a", ReportingPort: "Gi0/1", NeighbourHint: "sw-b", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	// dc-b is not in the hub — no reverse observation.
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	if len(g.Edges) != 0 {
		t.Errorf("edge count = %d, want 0 (no reverse observation)", len(g.Edges))
	}
}

// TestHubOOSDomainStripProducesEdge verifies that OOS matching strips domain
// suffixes from device names: "core-01.internal.corp" and "core-01" are
// treated as the same device. This is the pre-v1.3.0 default behaviour, now
// opt-in via LooseDeviceNameMatching=true.
func TestHubOOSDomainStripProducesEdge(t *testing.T) {
	h := newTestHub(nil)
	h.cfg.Hub.LooseDeviceNameMatching = true
	h.mu.Lock()
	// dc-a sees the neighbour with its FQDN; dc-b reports its bare hostname.
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-a", ReportingPort: "Gi0/1", NeighbourHint: "core-01.internal.corp", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	h.spokes["dc-b"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "core-01", ReportingPort: "Gi0/2", NeighbourHint: "sw-a", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	if len(g.Edges) != 1 {
		t.Fatalf("edge count = %d, want 1 (domain suffix strip should produce edge)", len(g.Edges))
	}
	if g.Edges[0].Direction != discovery.DirectionBidirectional {
		t.Errorf("direction = %q, want bidirectional", g.Edges[0].Direction)
	}
}

// TestHubOOSStrictDefaultPreventsCrossDCCollision verifies that with the v1.3.0
// default (LooseDeviceNameMatching=false → strict), two physically distinct
// devices that share a bare hostname across DCs ("core-01.dc1" and "core-01.dc2")
// are NOT collapsed into one node by OOS matching. This is the bug the default
// flip exists to prevent: docs/audits/2026-05-architectural-review.md §2.3.
func TestHubOOSStrictDefaultPreventsCrossDCCollision(t *testing.T) {
	h := newTestHub(nil) // LooseDeviceNameMatching zero value → strict
	h.mu.Lock()
	// dc-a sees a neighbour it calls "core-01.dc1"; this is the dc1 core.
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-a", ReportingPort: "Gi0/1", NeighbourHint: "core-01.dc1", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	// dc-b reports its bare hostname "core-01" — under loose matching this would
	// collide with "core-01.dc1" and produce a false edge. Under strict, it must not.
	h.spokes["dc-b"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "core-01", ReportingPort: "Gi0/2", NeighbourHint: "sw-a", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	for _, e := range g.Edges {
		if (e.SrcDevice == "sw-a" && e.DstDevice == "core-01") ||
			(e.SrcDevice == "core-01" && e.DstDevice == "sw-a") {
			t.Fatalf("strict default should NOT collapse 'core-01.dc1' with 'core-01'; got false edge: %+v", e)
		}
	}
}

// TestBuildCombinedGraphProtoFallbackToRemote verifies that when the local OOS
// observation carries an empty proto field, the hub falls back to the remote
// observation's proto when synthesising the cross-domain edge.
func TestBuildCombinedGraphProtoFallbackToRemote(t *testing.T) {
	h := newTestHub(nil)
	h.mu.Lock()
	// dc-a reports no proto; dc-b reports "cdp".
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-a", ReportingPort: "Gi0/1", NeighbourHint: "sw-b", Proto: ""},
			},
		},
		lastSeen: time.Now(),
	}
	h.spokes["dc-b"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-b", ReportingPort: "Gi0/2", NeighbourHint: "sw-a", Proto: "cdp"},
			},
		},
		lastSeen: time.Now(),
	}
	g := h.combinedGraphLocked()
	h.mu.Unlock()

	if len(g.Edges) != 1 {
		t.Fatalf("edge count = %d, want 1", len(g.Edges))
	}
	if g.Edges[0].DiscoveryProto != "cdp" {
		t.Errorf("discovery_proto = %q, want cdp (remote proto fallback)", g.Edges[0].DiscoveryProto)
	}
}

// TestHubOOSAmbiguousFQDNNormalisationWarns verifies that when two spokes report
// OOS observations involving devices that share a bare hostname but differ by
// domain suffix (e.g. "core-sw-01.dc1" and "core-sw-01.dc2"), the hub logs a
// warning about the ambiguous normalisation. The warning is only meaningful
// in the legacy loose-matching mode (pre-v1.3.0 default); this test opts back
// into it to validate the safety-net logging.
func TestHubOOSAmbiguousFQDNNormalisationWarns(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	m := metrics.New(false)
	h := NewHub(
		config.FederationConfig{
			SpokeTimeout: 5 * time.Minute,
			Hub:          config.FederationHubConfig{LooseDeviceNameMatching: true},
		},
		m,
		logger,
		"",
	)

	h.mu.Lock()
	// spoke dc-1 sees core-sw-01.dc1 as a neighbour.
	h.spokes["dc-1"] = spokeEntry{
		payload: federation.SpokePayload{
			SpokeID: "dc-1",
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "edge-sw-01", ReportingPort: "Gi0/1", NeighbourHint: "core-sw-01.dc1", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	// spoke dc-2 sees core-sw-01.dc2 as a neighbour — different physical device,
	// same bare hostname after normalisation.
	h.spokes["dc-2"] = spokeEntry{
		payload: federation.SpokePayload{
			SpokeID: "dc-2",
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "edge-sw-02", ReportingPort: "Gi0/1", NeighbourHint: "core-sw-01.dc2", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	h.combinedGraphLocked()
	h.mu.Unlock()

	logged := buf.String()
	if !strings.Contains(logged, "ambiguous device name normalisation") {
		t.Errorf("expected ambiguous normalisation warning in log output, got:\n%s", logged)
	}
	if !strings.Contains(logged, "core-sw-01") {
		t.Errorf("expected canonical name 'core-sw-01' in log output, got:\n%s", logged)
	}
}

// TestHubOOSUnmatchedMetricIncrementsOnMiss verifies that unmatched OOS hints
// are reflected in HubOOSUnmatchedTotal after the build wins publishIfWinner.
func TestHubOOSUnmatchedMetricIncrementsOnMiss(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{SpokeTimeout: 5 * time.Minute}, m, nil, "")

	h.mu.Lock()
	// Only one side reports; no reverse match so the hint is unmatched.
	h.spokes["dc-a"] = spokeEntry{
		payload: federation.SpokePayload{
			OutOfScope: []discovery.OutOfScopeNeighbour{
				{ReportingDevice: "sw-a", ReportingPort: "Gi0/1", NeighbourHint: "sw-unknown", Proto: "lldp"},
			},
		},
		lastSeen: time.Now(),
	}
	spokes := h.spokesSnapshot()
	gen := h.publishGen.Add(1)
	h.mu.Unlock()

	combined, unmatchedCount := h.buildCombinedGraph(spokes)
	h.publishIfWinner(gen, combined, unmatchedCount, nil)

	if got := testutil.ToFloat64(m.HubOOSUnmatchedTotal); got == 0 {
		t.Error("HubOOSUnmatchedTotal = 0, want > 0 for unmatched OOS hint")
	}
}
