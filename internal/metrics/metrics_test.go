package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grafana/network-topology-exporter/internal/discovery"
)

func TestNewRegistersExpectedMetrics(t *testing.T) {
	m := New(false)

	m.Topology.Update(discovery.Graph{
		Devices: []discovery.Device{
			{ID: "dev-1", Vendor: "cisco", Model: "C9300", OSVersion: "17.6.4", Site: "lab", Uptime: 0},
		},
	})

	const want = `
# HELP network_topology_device_info One series per discovered device. Value is always 1; inventory data is in the labels. sys_name is the case-preserving sysName, added for joining against Alloy/snmp_exporter's own sysName label — see docs/proposals/snmp-exporter-label-alignment.md.
# TYPE network_topology_device_info gauge
network_topology_device_info{device_id="dev-1",model="C9300",os_version="17.6.4",site="lab",sys_name="",vendor="cisco"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "network_topology_device_info"); err != nil {
		t.Fatalf("metric mismatch: %v", err)
	}
}

func TestTopologyCollectorUptimeUpdatesEveryCycle(t *testing.T) {
	m := New(false)

	m.Topology.Update(discovery.Graph{
		Devices: []discovery.Device{{ID: "dev-1", Uptime: 10 * time.Second}},
	})
	m.Topology.Update(discovery.Graph{
		Devices: []discovery.Device{{ID: "dev-1", Uptime: 25 * time.Second}},
	})

	const want = `
# HELP network_topology_device_uptime_seconds Per-device uptime from the SNMP SYSTEM group (sysUpTime).
# TYPE network_topology_device_uptime_seconds gauge
network_topology_device_uptime_seconds{device_id="dev-1"} 25
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "network_topology_device_uptime_seconds"); err != nil {
		t.Fatalf("uptime not updated: %v", err)
	}
}

func TestTopologyConflictTotalIsRegistered(t *testing.T) {
	m := New(false)

	m.TopologyConflictTotal.WithLabelValues("neighbour_disagreement").Inc()

	const want = `
# HELP network_topology_conflict_total Source disagreements detected during reconciliation, by conflict type.
# TYPE network_topology_conflict_total counter
network_topology_conflict_total{conflict_type="neighbour_disagreement"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "network_topology_conflict_total"); err != nil {
		t.Fatalf("metric mismatch: %v", err)
	}
}

func TestMetricNamespaceConsistency(t *testing.T) {
	m := New(false)
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, mf := range mfs {
		name := mf.GetName()
		// Skip standard Go/process collectors.
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		if !strings.HasPrefix(name, "network_") {
			t.Errorf("metric %q does not use network_ prefix", name)
		}
	}
}

func TestRegistryExposesGoCollector(t *testing.T) {
	m := New(false)
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sawGo bool
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "go_") {
			sawGo = true
			break
		}
	}
	if !sawGo {
		t.Fatal("expected at least one go_* metric from the standard Go collector")
	}
}

func TestSpokePushMetricsRegistered(t *testing.T) {
	m := New(false)
	m.FederationSpokePushLastSuccessUnix.Set(1)
	m.FederationSpokePushDropsTotal.WithLabelValues("superseded").Inc()
	m.FederationSpokePushQueueDepth.Set(1)

	fams, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]bool{
		"network_topology_federation_spoke_push_last_success_unix": false,
		"network_topology_federation_spoke_push_drops_total":       false,
		"network_topology_federation_spoke_push_queue_depth":       false,
	}
	for _, f := range fams {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric %q not registered", name)
		}
	}
}
