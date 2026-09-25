//go:build integration

// Package integration provides end-to-end tests that exercise the full
// discovery → reconcile → metrics pipeline using in-process SNMP agents.
// Run with: go test ./tests/integration/... -tags integration -race
package integration

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	gsnmp "github.com/gosnmp/gosnmp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/discovery/lldp"
	snmputil "github.com/grafana/network-topology-exporter/internal/discovery/snmp"
	"github.com/grafana/network-topology-exporter/internal/graph"
	"github.com/grafana/network-topology-exporter/internal/metrics"
	"github.com/grafana/network-topology-exporter/internal/snmptest"
)

// OID constants copied from the lldp package (unexported there).
const (
	lldpLocBase = ".1.0.8802.1.1.2.1.3.7.1."
	lldpRemBase = ".1.0.8802.1.1.2.1.4.1.1."

	// lldpRemTable column numbers
	remColChassisSubtype = "4"
	remColChassisID      = "5"
	remColPortSubtype    = "6"
	remColPortID         = "7"
	remColSysName        = "9"

	chassisSubtypeMACAddress = 4
	portSubtypeInterfaceName = 5
)

// lldpPDUs builds a minimal LLDP MIB for a device with one local port (portID)
// that sees one remote neighbour (remoteSysName, remotePortID).
func lldpPDUs(portID, remoteSysName, remotePortID string, remoteMac [6]byte) []gsnmp.SnmpPDU {
	portNum := "1"
	remSuffix := "0." + portNum + ".1" // timeMark=0, portNum, remIdx=1

	return []gsnmp.SnmpPDU{
		// lldpLocPortTable
		{Name: lldpLocBase + "2." + portNum, Type: gsnmp.Integer, Value: portSubtypeInterfaceName},
		{Name: lldpLocBase + "3." + portNum, Type: gsnmp.OctetString, Value: []byte(portID)},

		// lldpRemTable
		{Name: lldpRemBase + remColChassisSubtype + "." + remSuffix, Type: gsnmp.Integer, Value: chassisSubtypeMACAddress},
		{Name: lldpRemBase + remColChassisID + "." + remSuffix, Type: gsnmp.OctetString, Value: remoteMac[:]},
		{Name: lldpRemBase + remColPortSubtype + "." + remSuffix, Type: gsnmp.Integer, Value: portSubtypeInterfaceName},
		{Name: lldpRemBase + remColPortID + "." + remSuffix, Type: gsnmp.OctetString, Value: []byte(remotePortID)},
		{Name: lldpRemBase + remColSysName + "." + remSuffix, Type: gsnmp.OctetString, Value: []byte(remoteSysName)},
	}
}

func snmpParams(addr string, timeout time.Duration) snmputil.Params {
	ip, port := snmptest.ParseAddr(addr)
	return snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   timeout,
	}
}

// TestTwoDeviceLLDPBidirectional exercises the full pipeline:
//   - Two in-process SNMP agents simulate sw-a and sw-b, each advertising
//     the link to the other via LLDP.
//   - lldp.Walk is called for each device.
//   - All edges are fed to graph.Reconcile.
//   - The result must be exactly one bidirectional edge.
func TestTwoDeviceLLDPBidirectional(t *testing.T) {
	macA := [6]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}
	macB := [6]byte{0x00, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e}

	// sw-a sees sw-b via GigabitEthernet0/1 → GigabitEthernet0/2
	addrA := snmptest.Start(t, "public", lldpPDUs("GigabitEthernet0/1", "sw-b", "GigabitEthernet0/2", macB))
	// sw-b sees sw-a via GigabitEthernet0/2 → GigabitEthernet0/1
	addrB := snmptest.Start(t, "public", lldpPDUs("GigabitEthernet0/2", "sw-a", "GigabitEthernet0/1", macA))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tout := 3 * time.Second

	edgesA, oosA, err := lldp.Walk(ctx, snmpParams(addrA, tout), "sw-a", nil)
	if err != nil {
		t.Fatalf("lldp.Walk sw-a: %v", err)
	}
	if len(oosA) != 0 {
		t.Errorf("sw-a: unexpected OOS entries: %v", oosA)
	}

	edgesB, oosB, err := lldp.Walk(ctx, snmpParams(addrB, tout), "sw-b", nil)
	if err != nil {
		t.Fatalf("lldp.Walk sw-b: %v", err)
	}
	if len(oosB) != 0 {
		t.Errorf("sw-b: unexpected OOS entries: %v", oosB)
	}

	if len(edgesA) != 1 {
		t.Fatalf("sw-a emitted %d edges, want 1", len(edgesA))
	}
	if len(edgesB) != 1 {
		t.Fatalf("sw-b emitted %d edges, want 1", len(edgesB))
	}

	all := append(edgesA, edgesB...)
	reconEdges, conflicts := graph.Reconcile(all)

	if len(conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", conflicts)
	}
	if len(reconEdges) != 1 {
		t.Fatalf("reconciled edge count = %d, want 1", len(reconEdges))
	}

	e := reconEdges[0]
	if e.Direction != discovery.DirectionBidirectional {
		t.Errorf("direction = %q, want bidirectional", e.Direction)
	}
	// Canonical direction: alphabetically-smaller device is SrcDevice.
	if e.SrcDevice != "sw-a" || e.DstDevice != "sw-b" {
		t.Errorf("endpoints = (%s, %s), want (sw-a, sw-b)", e.SrcDevice, e.DstDevice)
	}
	// graph.Reconcile applies NormalizePortName to the chosen edge's SrcPort
	// and DstPort (graph.go:187-188) so EdgeKey stays stable across cycles
	// when the winning protocol flips between LLDP (long form) and CDP
	// (short form). The PDUs above feed "GigabitEthernet0/1" and
	// "GigabitEthernet0/2"; the reconciler abbreviates these to "Gi0/1" /
	// "Gi0/2" via the interfaceAbbreviations table.
	if e.SrcPort != "Gi0/1" {
		t.Errorf("SrcPort = %q, want Gi0/1 (normalized from GigabitEthernet0/1)", e.SrcPort)
	}
	if e.DstPort != "Gi0/2" {
		t.Errorf("DstPort = %q, want Gi0/2 (normalized from GigabitEthernet0/2)", e.DstPort)
	}
	if e.DiscoveryProto != "lldp" {
		t.Errorf("DiscoveryProto = %q, want lldp", e.DiscoveryProto)
	}
}

// TestLLDPOutOfScopeNeighbour verifies that a neighbour whose IP falls outside
// the CIDR allow-list is recorded as out-of-scope and not emitted as an edge.
func TestLLDPOutOfScopeNeighbour(t *testing.T) {
	// sw-a sees an out-of-scope device whose chassis-id is an IP (networkAddress subtype).
	const (
		chassisSubtypeNetworkAddress = 5
	)
	portNum := "1"
	remSuffix := "0." + portNum + ".1"
	pdus := []gsnmp.SnmpPDU{
		// lldpLocPortTable
		{Name: lldpLocBase + "2." + portNum, Type: gsnmp.Integer, Value: portSubtypeInterfaceName}, // interfaceName subtype
		{Name: lldpLocBase + "3." + portNum, Type: gsnmp.OctetString, Value: []byte("Gi0/1")},
		// lldpRemTable — remote chassis is networkAddress 10.99.0.1 (outside allow-list)
		{Name: lldpRemBase + remColChassisSubtype + "." + remSuffix, Type: gsnmp.Integer, Value: chassisSubtypeNetworkAddress},
		{Name: lldpRemBase + remColChassisID + "." + remSuffix, Type: gsnmp.OctetString, Value: []byte{1, 10, 99, 0, 1}}, // IANA family 1 + IPv4
		{Name: lldpRemBase + remColPortSubtype + "." + remSuffix, Type: gsnmp.Integer, Value: portSubtypeInterfaceName},
		{Name: lldpRemBase + remColPortID + "." + remSuffix, Type: gsnmp.OctetString, Value: []byte("Gi0/2")},
		{Name: lldpRemBase + remColSysName + "." + remSuffix, Type: gsnmp.OctetString, Value: []byte("oos-peer")},
	}

	addr := snmptest.Start(t, "public", pdus)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Allow-list covers 10.0.0.0/24; the remote device is 10.99.0.1 — outside.
	_, allowed, _ := net.ParseCIDR("10.0.0.0/24")
	edges, oos, err := lldp.Walk(ctx, snmpParams(addr, 3*time.Second), "sw-a", []*net.IPNet{allowed})
	if err != nil {
		t.Fatalf("lldp.Walk: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for out-of-scope neighbour, got %d", len(edges))
	}
	if len(oos) != 1 {
		t.Fatalf("expected 1 OOS entry, got %d: %v", len(oos), oos)
	}
	if oos[0].ReportingDevice != "sw-a" {
		t.Errorf("OOS ReportingDevice = %q, want sw-a", oos[0].ReportingDevice)
	}
}

// TestLLDPUnidirectionalLink verifies that when only one device reports a link,
// graph.Reconcile produces a unidirectional edge (not bidirectional).
func TestLLDPUnidirectionalLink(t *testing.T) {
	macB := [6]byte{0x00, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e}

	// Only sw-a reports the link; sw-b has no LLDP agent (no agent started for it).
	addrA := snmptest.Start(t, "public", lldpPDUs("Gi0/1", "sw-b", "Gi0/2", macB))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	edges, _, err := lldp.Walk(ctx, snmpParams(addrA, 3*time.Second), "sw-a", nil)
	if err != nil {
		t.Fatalf("lldp.Walk sw-a: %v", err)
	}

	reconEdges, _ := graph.Reconcile(edges)

	if len(reconEdges) != 1 {
		t.Fatalf("edge count = %d, want 1", len(reconEdges))
	}
	if reconEdges[0].Direction != discovery.DirectionUnidirectional {
		t.Errorf("direction = %q, want unidirectional (only one side reported)", reconEdges[0].Direction)
	}
}

// TestMetricsEmittedAfterReconcile verifies that after a successful reconcile
// the metrics.Metrics object emits the expected network_topology_edge_info series.
func TestMetricsEmittedAfterReconcile(t *testing.T) {
	macA := [6]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}
	macB := [6]byte{0x00, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e}

	addrA := snmptest.Start(t, "public", lldpPDUs("GigabitEthernet0/1", "sw-b", "GigabitEthernet0/2", macB))
	addrB := snmptest.Start(t, "public", lldpPDUs("GigabitEthernet0/2", "sw-a", "GigabitEthernet0/1", macA))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tout := 3 * time.Second
	edgesA, _, err := lldp.Walk(ctx, snmpParams(addrA, tout), "sw-a", nil)
	if err != nil {
		t.Fatalf("lldp.Walk sw-a: %v", err)
	}
	edgesB, _, err := lldp.Walk(ctx, snmpParams(addrB, tout), "sw-b", nil)
	if err != nil {
		t.Fatalf("lldp.Walk sw-b: %v", err)
	}

	reconEdges, _ := graph.Reconcile(append(edgesA, edgesB...))

	m := metrics.New(false)
	m.Topology.Update(discovery.Graph{Edges: reconEdges})

	// Verify the edge_info collector emits the expected series.
	// graph.Reconcile applies NormalizePortName to the chosen edge's
	// SrcPort/DstPort (graph.go:187-188), so the metric series carry the
	// abbreviated forms ("Gi0/1" / "Gi0/2") even though the LLDP PDUs above
	// advertised the long forms.
	const want = `
# HELP network_topology_edge_info One series per discovered topology edge. Value is always 1. src_if_index/dst_if_index carry the IF-MIB ifIndex for each endpoint when the discovery protocol resolves one (empty otherwise) — the join key against snmp_exporter's ifIndex-keyed if_mib rows.
# TYPE network_topology_edge_info gauge
network_topology_edge_info{direction="bidirectional",discovery_proto="lldp",dst_device="sw-b",dst_if_index="",dst_port="Gi0/2",link_kind="ethernet",src_device="sw-a",src_if_index="1",src_port="Gi0/1"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "network_topology_edge_info"); err != nil {
		t.Errorf("edge_info mismatch: %v", err)
	}
}
