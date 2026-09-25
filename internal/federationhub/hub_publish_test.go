package federationhub

// Tests split from hub_test.go (#168); see hub_publish.go.
import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// TestHubPublishMetricsClearStale verifies that passing clearStale=true sets
// GraphStale to 0 as part of the publish.
func TestHubPublishMetricsClearStale(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{}, m, nil, "")

	m.GraphStale.Set(1) // simulate startup state
	h.publishMetrics(discovery.Graph{}, true)

	if got := testutil.ToFloat64(m.GraphStale); got != 0 {
		t.Errorf("GraphStale = %v after clearStale=true, want 0", got)
	}
}

// TestHubPublishMetricsPreservesStaleWhenFalse verifies that clearStale=false
// does not touch GraphStale.
func TestHubPublishMetricsPreservesStaleWhenFalse(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{}, m, nil, "")

	m.GraphStale.Set(1)
	h.publishMetrics(discovery.Graph{}, false)

	if got := testutil.ToFloat64(m.GraphStale); got != 1 {
		t.Errorf("GraphStale = %v after clearStale=false, want 1 (unchanged)", got)
	}
}

// TestHubRestoreGraphPublishesMetrics verifies that RestoreGraph pushes device
// and edge info metrics so the hub can serve stale data immediately after startup.
func TestHubRestoreGraphPublishesMetrics(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{}, m, nil, "")

	g := discovery.Graph{
		Devices: []discovery.Device{{ID: "sw-restore", Vendor: "cisco"}},
		Edges: []discovery.Edge{{
			SrcDevice: "sw-restore", SrcPort: "Gi0/1",
			DstDevice: "sw-peer", DstPort: "Gi0/2",
			DiscoveryProto: "lldp",
			Direction:      discovery.DirectionBidirectional,
			LinkKind:       "ethernet",
		}},
	}
	m.GraphStale.Set(1) // caller is responsible for setting this before RestoreGraph
	h.RestoreGraph(g)

	// GraphStale should remain 1 — RestoreGraph calls publishMetrics(g, false).
	if got := testutil.ToFloat64(m.GraphStale); got != 1 {
		t.Errorf("GraphStale = %v after RestoreGraph, want 1 (clearStale=false)", got)
	}
	// Topology collector should have been populated with the device.
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var deviceSeries int
	for _, mf := range mfs {
		if mf.GetName() == "network_topology_device_info" {
			deviceSeries = len(mf.GetMetric())
		}
	}
	if deviceSeries != 1 {
		t.Errorf("network_topology_device_info series after RestoreGraph = %d, want 1", deviceSeries)
	}
}

// TestPublishIfWinnerOversizeDoesNotBurnGeneration pins the #147 defect #2 fix:
// an oversize graph must be rejected WITHOUT advancing lastPublishedGen, so a
// concurrent valid graph with a lower generation can still win.
func TestPublishIfWinnerOversizeDoesNotBurnGeneration(t *testing.T) {
	h := NewHub(config.FederationConfig{
		Hub: config.FederationHubConfig{MaxGraphEdges: 1, MaxGraphDevices: 1},
	}, metrics.New(false), nil, "")

	oversize := discovery.Graph{
		Devices: []discovery.Device{{ID: "a"}, {ID: "b"}},
		Edges:   []discovery.Edge{{SrcDevice: "a", DstDevice: "b"}, {SrcDevice: "b", DstDevice: "a"}},
	}
	ok, reason := h.publishIfWinner(5, oversize, 0, nil)
	if ok || reason != metrics.RejectReasonSizeBudgetExceeded {
		t.Fatalf("oversize: got (%v, %q), want (false, size_budget_exceeded)", ok, reason)
	}
	h.mu.Lock()
	gotGen := h.lastPublishedGen
	h.mu.Unlock()
	if gotGen != 0 {
		t.Fatalf("oversize burned the generation: lastPublishedGen=%d, want 0", gotGen)
	}

	valid := discovery.Graph{Devices: []discovery.Device{{ID: "a"}}}
	ok, reason = h.publishIfWinner(4, valid, 0, nil)
	if !ok {
		t.Fatalf("valid lower-gen graph rejected after oversize: reason=%q", reason)
	}
}

// TestPublishIfWinnerRejectsOversizedGraphEdges verifies that publishIfWinner
// increments GraphUpdatesRejectedTotal and does NOT update Topology when the
// combined graph exceeds MaxGraphEdges.
func TestPublishIfWinnerRejectsOversizedGraphEdges(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(
		config.FederationConfig{
			Hub: config.FederationHubConfig{MaxGraphEdges: 2},
		},
		m, nil, "",
	)

	g := discovery.Graph{
		Edges: []discovery.Edge{
			{SrcDevice: "a", DstDevice: "b", DiscoveryProto: "lldp"},
			{SrcDevice: "b", DstDevice: "c", DiscoveryProto: "lldp"},
			{SrcDevice: "c", DstDevice: "d", DiscoveryProto: "lldp"},
			{SrcDevice: "d", DstDevice: "e", DiscoveryProto: "lldp"},
			{SrcDevice: "e", DstDevice: "f", DiscoveryProto: "lldp"},
		},
	}

	h.publishIfWinner(1, g, 0, nil)

	h.mu.Lock()
	if h.lastPublishedGen != 0 {
		t.Fatalf("oversize reject advanced lastPublishedGen to %d, want 0", h.lastPublishedGen)
	}
	h.mu.Unlock()

	if got := testutil.ToFloat64(m.GraphUpdatesRejectedTotal.WithLabelValues(string(rejectReasonSizeBudgetExceeded))); got != 1 {
		t.Errorf("GraphUpdatesRejectedTotal{reason=size_budget_exceeded} = %v, want 1", got)
	}

	// Topology must NOT have been updated — gather metrics and confirm no
	// network_topology_edge_info series were written.
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "network_topology_edge_info" && len(mf.GetMetric()) > 0 {
			t.Errorf("network_topology_edge_info has %d series after rejected update, want 0",
				len(mf.GetMetric()))
		}
	}
}

// TestPublishIfWinnerRejectsOversizedGraphDevices verifies that
// publishIfWinner increments GraphUpdatesRejectedTotal and does NOT update
// Topology when the combined graph exceeds MaxGraphDevices.
func TestPublishIfWinnerRejectsOversizedGraphDevices(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(
		config.FederationConfig{
			Hub: config.FederationHubConfig{MaxGraphDevices: 1},
		},
		m, nil, "",
	)

	g := discovery.Graph{
		Devices: []discovery.Device{
			{ID: "sw-1"},
			{ID: "sw-2"},
			{ID: "sw-3"},
		},
	}

	h.publishIfWinner(1, g, 0, nil)

	h.mu.Lock()
	if h.lastPublishedGen != 0 {
		t.Fatalf("oversize reject advanced lastPublishedGen to %d, want 0", h.lastPublishedGen)
	}
	h.mu.Unlock()

	if got := testutil.ToFloat64(m.GraphUpdatesRejectedTotal.WithLabelValues(string(rejectReasonSizeBudgetExceeded))); got != 1 {
		t.Errorf("GraphUpdatesRejectedTotal{reason=size_budget_exceeded} = %v, want 1", got)
	}

	// Topology must NOT have been updated — gather metrics and confirm no
	// network_topology_device_info series were written.
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "network_topology_device_info" && len(mf.GetMetric()) > 0 {
			t.Errorf("network_topology_device_info has %d series after rejected update, want 0",
				len(mf.GetMetric()))
		}
	}
}
