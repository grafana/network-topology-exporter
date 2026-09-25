package metrics_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// The Prometheus edge_info / device_info label schemas are deliberately MINIMAL
// (issue #150): they omit high-cardinality Edge fields (Confidence, Adjacency,
// PrecedenceRank, Metadata) that the OTLP and YANG outputs DO carry. The three
// outputs diverge on purpose — see the projection notes on discovery.Edge /
// discovery.Device. These tests pin the Prometheus label-name sets so that
// adding a label is a conscious cardinality decision (and a prompt to review
// whether OTLP/YANG should change too), rather than silent drift.

func gatheredLabelNames(t *testing.T, m *metrics.Metrics, metricName string) []string {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != metricName {
			continue
		}
		if len(mf.GetMetric()) == 0 {
			t.Fatalf("%s has no series to inspect", metricName)
		}
		var names []string
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			names = append(names, lp.GetName())
		}
		sort.Strings(names)
		return names
	}
	t.Fatalf("metric %q not found in gather output", metricName)
	return nil
}

func TestEdgeInfoLabelSchemaStable(t *testing.T) {
	m := metrics.New(false)
	// Populate the high-cardinality fields too, to prove they do NOT leak into
	// the Prometheus label set (they are intentionally dropped; OTLP carries them).
	m.Topology.Update(discovery.Graph{
		Edges: []discovery.Edge{{
			SrcDevice: "dev-a", SrcPort: "Gi0/1",
			DstDevice: "dev-b", DstPort: "Gi0/2",
			DiscoveryProto: discovery.DiscoveryProtocolLLDP,
			Direction:      discovery.DirectionBidirectional,
			LinkKind:       discovery.LinkKind("ethernet"),
			Confidence:     discovery.Confidence("confirmed"),
			Adjacency:      discovery.Adjacency("direct"),
			PrecedenceRank: 3,
			Metadata:       map[string]string{"remote_as": "64512"},
		}},
	})

	want := []string{
		"direction", "discovery_proto", "dst_device", "dst_if_index", "dst_port",
		"link_kind", "src_device", "src_if_index", "src_port",
	}
	got := gatheredLabelNames(t, m, "network_topology_edge_info")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("network_topology_edge_info label schema changed.\n got: %v\nwant: %v\n"+
			"This is a cardinality-sensitive change (#150). If intentional, update this test, "+
			"and review whether the OTLP/YANG projections need the same field — see discovery.Edge.", got, want)
	}
}

func TestDeviceInfoLabelSchemaStable(t *testing.T) {
	m := metrics.New(false)
	m.Topology.Update(discovery.Graph{
		Devices: []discovery.Device{{
			ID: "dev-1", Vendor: "cisco", Model: "C9300", OSVersion: "17.6.4", Site: "lab",
			Labels: map[string]string{"team": "neteng"}, // free-form labels must NOT become series labels
		}},
	})

	want := []string{"device_id", "model", "os_version", "site", "sys_name", "vendor"}
	got := gatheredLabelNames(t, m, "network_topology_device_info")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("network_topology_device_info label schema changed.\n got: %v\nwant: %v\n"+
			"Conscious cardinality change (#150); review the OTLP/YANG device projections too — see discovery.Device.", got, want)
	}
}
