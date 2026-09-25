package isis

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	gsnmp "github.com/gosnmp/gosnmp"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	snmputil "github.com/grafana/network-topology-exporter/internal/discovery/snmp"
	"github.com/grafana/network-topology-exporter/internal/snmptest"
)

const (
	adjStateBase = ".1.3.6.1.2.1.138.1.6.1.1.2."
	adjIPBase    = ".1.3.6.1.2.1.138.1.6.2.1.2."
	adjKey       = "0.1.1"
	neighborIP   = "192.0.2.1"
)

// Walk: up adjacency (state=3) with in-scope neighbour → one edge.
func TestWalkUpAdjacencyInScope(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}

	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope, got %d", len(oos))
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d: %v", len(edges), edges)
	}
	e := edges[0]
	if e.SrcDevice != "router-a" {
		t.Errorf("SrcDevice = %q, want router-a", e.SrcDevice)
	}
	if e.DstDevice != neighborIP {
		t.Errorf("DstDevice = %q, want %s", e.DstDevice, neighborIP)
	}
	if e.DiscoveryProto != "isis" {
		t.Errorf("DiscoveryProto = %q, want isis", e.DiscoveryProto)
	}
	if e.Direction != discovery.DirectionUnidirectional {
		t.Errorf("Direction = %q, want unidirectional", e.Direction)
	}
	if e.Confidence != discovery.ConfidenceMedium {
		t.Errorf("Confidence = %q, want medium", e.Confidence)
	}
	if e.Adjacency != discovery.AdjacencyDirect {
		t.Errorf("Adjacency = %q, want direct", e.Adjacency)
	}
	if e.PrecedenceRank != precedenceRank {
		t.Errorf("PrecedenceRank = %d, want %d", e.PrecedenceRank, precedenceRank)
	}
	if e.LinkKind != "ip" {
		t.Errorf("LinkKind = %q, want ip", e.LinkKind)
	}
	if e.ObservedAt.IsZero() {
		t.Error("ObservedAt is zero")
	}
}

// Walk: down adjacency (state=1) → no edge, no OOS.
func TestWalkDownAdjacency(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: 1},
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for down adjacency, got %d", len(edges))
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope, got %d", len(oos))
	}
}

// Walk: initializing adjacency (state=2) → no edge, no OOS.
func TestWalkInitializingAdjacency(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: 2},
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for initializing adjacency, got %d", len(edges))
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope, got %d", len(oos))
	}
}

// Walk: up adjacency with out-of-scope neighbour → OutOfScopeNeighbour, no edge.
func TestWalkUpAdjacencyOutOfScope(t *testing.T) {
	_, allow, _ := net.ParseCIDR("172.16.0.0/12")
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", []*net.IPNet{allow})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 in-scope edges, got %d", len(edges))
	}
	if len(oos) != 1 {
		t.Fatalf("expected 1 out-of-scope entry, got %d", len(oos))
	}
	if oos[0].NeighbourHint != neighborIP {
		t.Errorf("NeighbourHint = %q, want %s", oos[0].NeighbourHint, neighborIP)
	}
	if oos[0].ReportingDevice != "router-a" {
		t.Errorf("ReportingDevice = %q, want router-a", oos[0].ReportingDevice)
	}
}

// Walk: IP entry exists with no matching state entry → no edge (treat as not-up).
func TestWalkMissingStateEntry(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		// No state entry for adjKey; only an IP address entry.
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for missing state, got %d", len(edges))
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope, got %d", len(oos))
	}
}

// Walk: empty walk (no PDUs) → empty results, no error.
func TestWalkEmpty(t *testing.T) {
	addr := snmptest.Start(t, "public", nil)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for empty walk, got %d", len(edges))
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope for empty walk, got %d", len(oos))
	}
}

// TestWalkAdjIPAddrsAdjKeyExtraction verifies the tail-count adjKey extraction
// for two cases:
//
//  1. Normal: adjIdx=1, IP=10.0.0.1 → adjKey "0.1.1", edge produced.
//  2. Ambiguous: adjIdx=4, IP=1.4.5.6 → the old LastIndex approach would
//     match the wrong ".1.4." and produce adjKey "0.1.4.1" (missing in the
//     state map → edge dropped). The new tail-count approach correctly
//     produces adjKey "0.1.4" and emits the edge.
func TestWalkAdjIPAddrsAdjKeyExtraction(t *testing.T) {
	cases := []struct {
		name      string
		stateKey  string // key stored in the states map (state=up)
		ipSuffix  string // OID suffix appended to adjIPBase
		ipBytes   []byte // raw IPv4 bytes for the PDU value
		wantEdge  bool
		wantDstIP string
	}{
		{
			name:      "normal adjIdx=1 IP=10.0.0.1",
			stateKey:  "0.1.1",
			ipSuffix:  "0.1.1.1.4.10.0.0.1",
			ipBytes:   []byte{10, 0, 0, 1},
			wantEdge:  true,
			wantDstIP: "10.0.0.1",
		},
		{
			name:      "ambiguous adjIdx=4 IP=1.4.5.6",
			stateKey:  "0.1.4",
			ipSuffix:  "0.1.4.1.4.1.4.5.6",
			ipBytes:   []byte{1, 4, 5, 6},
			wantEdge:  true,
			wantDstIP: "1.4.5.6",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pdus := []gsnmp.SnmpPDU{
				{Name: adjStateBase + tc.stateKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
				{Name: adjIPBase + tc.ipSuffix, Type: gsnmp.OctetString, Value: tc.ipBytes},
			}
			addr := snmptest.Start(t, "public", pdus)
			ip, port := snmptest.ParseAddr(addr)

			p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
			edges, _, err := Walk(context.Background(), p, "router-a", nil)
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if tc.wantEdge {
				if len(edges) != 1 {
					t.Fatalf("expected 1 edge, got %d", len(edges))
				}
				if edges[0].DstDevice != tc.wantDstIP {
					t.Errorf("DstDevice = %q, want %s", edges[0].DstDevice, tc.wantDstIP)
				}
			} else if len(edges) != 0 {
				t.Errorf("expected 0 edges, got %d", len(edges))
			}
		})
	}
}

// isisCircPDUs returns PDUs for isisISCircIfIndex: sysInst=0, circIdx=1 → ifIndex=3.
func isisCircPDUs() []gsnmp.SnmpPDU {
	return []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.138.1.4.1.1.3.0.1", Type: gsnmp.Integer, Value: 3},
	}
}

// ifDescrPDUs returns PDUs for ifDescr: ifIndex=3 → "GigabitEthernet0/0".
func ifDescrPDUs() []gsnmp.SnmpPDU {
	return []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.2.2.1.2.3", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/0")},
	}
}

// Walk: up adjacency with circuit and ifDescr tables present → SrcPort populated.
func TestWalkPopulatesSrcPort(t *testing.T) {
	// sysInst=0, circIdx=1, adjIdx=1, IP=192.0.2.1
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	pdus = append(pdus, isisCircPDUs()...)
	pdus = append(pdus, ifDescrPDUs()...)

	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].SrcPort != "GigabitEthernet0/0" {
		t.Errorf("SrcPort = %q, want GigabitEthernet0/0", edges[0].SrcPort)
	}
	if edges[0].SrcIfIndex != 3 {
		t.Errorf("SrcIfIndex = %d, want 3", edges[0].SrcIfIndex)
	}
}

// Walk: up adjacency without circuit table PDUs → SrcPort empty (graceful degradation).
func TestWalkSrcPortEmptyWhenCircuitMissing(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].SrcPort != "" {
		t.Errorf("SrcPort = %q, want empty string", edges[0].SrcPort)
	}
	if edges[0].SrcIfIndex != 0 {
		t.Errorf("SrcIfIndex = %d, want 0 (circuit table missing, nothing to resolve)", edges[0].SrcIfIndex)
	}
	if edges[0].Metadata == nil {
		t.Fatal("Metadata is nil, want degraded metadata")
	}
	if edges[0].Metadata[discovery.MetadataKeyDegraded] != "true" {
		t.Errorf("%s = %q, want true", discovery.MetadataKeyDegraded, edges[0].Metadata[discovery.MetadataKeyDegraded])
	}
	if edges[0].Metadata[discovery.MetadataKeyDegradedReason] != discovery.DegradedReasonMissingSrcPortMapping {
		t.Errorf("%s = %q, want %s", discovery.MetadataKeyDegradedReason, edges[0].Metadata[discovery.MetadataKeyDegradedReason], discovery.DegradedReasonMissingSrcPortMapping)
	}
}

// Walk: non-integer adjacency state PDU is a hard failure.
func TestWalkAdjStateDecodeFailureIsHardFail(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.OctetString, Value: []byte("bad")},
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	_, _, err := Walk(context.Background(), p, "router-a", nil)
	if err == nil {
		t.Fatal("expected hard-fail error for adjacency decode failure, got nil")
	}
}

// Walk: mixed valid+invalid adjacency state rows below threshold degrades but still emits edges.
func TestWalkAdjStatePartialDecodeDegraded(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + "0.1.1", Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjStateBase + "0.1.2", Type: gsnmp.OctetString, Value: []byte("bad")},
		{Name: adjIPBase + "0.1.1.1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}

	edges, _, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].Metadata == nil || edges[0].Metadata[discovery.MetadataKeyDegraded] != "true" {
		t.Fatalf("expected degraded metadata, got %+v", edges[0].Metadata)
	}
	if edges[0].Metadata[discovery.MetadataKeyDegradedReason] != discovery.DegradedReasonRequiredTablePartialDecode {
		t.Errorf("degraded_reason = %q, want %s", edges[0].Metadata[discovery.MetadataKeyDegradedReason], discovery.DegradedReasonRequiredTablePartialDecode)
	}
}

// Walk: required-table invalid ratio above threshold hard-fails.
func TestWalkAdjStateInvalidRatioExceededHardFail(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + "0.1.1", Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjStateBase + "0.1.2", Type: gsnmp.OctetString, Value: []byte("bad")},
		{Name: adjStateBase + "0.1.3", Type: gsnmp.OctetString, Value: []byte("bad")},
		{Name: adjIPBase + "0.1.1.1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}

	_, _, err := Walk(context.Background(), p, "router-a", nil)
	if err == nil {
		t.Fatal("expected hard-fail error for invalid ratio exceeded, got nil")
	}
}

// Walk: deterministic behavior across noisy cycles (degraded -> hard-fail -> recover).
func TestWalkAdjStateNoisyCyclesDeterministic(t *testing.T) {
	run := func(pdus []gsnmp.SnmpPDU) error {
		addr := snmptest.Start(t, "public", pdus)
		ip, port := snmptest.ParseAddr(addr)
		p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
		_, _, err := Walk(context.Background(), p, "router-a", nil)
		return err
	}

	// Cycle 1: partial decode anomaly below threshold => no hard-fail.
	err := run([]gsnmp.SnmpPDU{
		{Name: adjStateBase + "0.1.1", Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjStateBase + "0.1.2", Type: gsnmp.OctetString, Value: []byte("bad")},
		{Name: adjIPBase + "0.1.1.1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	})
	if err != nil {
		t.Fatalf("cycle1 unexpected error: %v", err)
	}

	// Cycle 2: invalid ratio exceeds threshold => hard-fail.
	err = run([]gsnmp.SnmpPDU{
		{Name: adjStateBase + "0.1.1", Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjStateBase + "0.1.2", Type: gsnmp.OctetString, Value: []byte("bad")},
		{Name: adjStateBase + "0.1.3", Type: gsnmp.OctetString, Value: []byte("bad")},
		{Name: adjIPBase + "0.1.1.1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	})
	if err == nil {
		t.Fatal("cycle2 expected hard-fail error, got nil")
	}

	// Cycle 3: clean data => no hard-fail.
	err = run([]gsnmp.SnmpPDU{
		{Name: adjStateBase + "0.1.1", Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + "0.1.1.1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
	})
	if err != nil {
		t.Fatalf("cycle3 unexpected recovery error: %v", err)
	}
}

// ---------- walkCircuitIfNames coverage tests ----------

// Walk: isisISCircIfIndex has a non-integer decode failure → walkCircuitIfNames
// returns a non-empty srcPortDegradedReason, exercising the
// "else if srcPortDegradedReason != """ branch in Walk.
func TestWalkCircuitIfNamesCircuitDecodeFailureDegrades(t *testing.T) {
	// Circuit PDU with OctetString value: decode failure → stats.InvalidRows > 0
	// → degradedReason = DegradedReasonMissingSrcPortMapping returned from walkCircuitIfNames.
	const circBase = ".1.3.6.1.2.1.138.1.4.1.1.3."
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
		// Non-integer circuit entry: decode failure triggers degradedReason in walkCircuitIfNames.
		{Name: circBase + "0.1", Type: gsnmp.OctetString, Value: []byte("bad")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge (degraded, not failed), got %d", len(edges))
	}
	if edges[0].Metadata == nil {
		t.Fatal("Metadata is nil, want degraded metadata")
	}
	if edges[0].Metadata[discovery.MetadataKeyDegraded] != "true" {
		t.Errorf("%s = %q, want true", discovery.MetadataKeyDegraded, edges[0].Metadata[discovery.MetadataKeyDegraded])
	}
}

// Walk: circuit table has matching ifIndex but ifDescr table is empty → no overlap,
// SrcPort stays empty, edge is degraded.
func TestWalkCircuitIfNamesNoIfDescrOverlap(t *testing.T) {
	const circBase = ".1.3.6.1.2.1.138.1.4.1.1.3."
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + adjKey + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
		// Circuit entry: sysInst=0, circIdx=1 → ifIndex=99, but no ifDescr for ifIndex 99.
		{Name: circBase + "0.1", Type: gsnmp.Integer, Value: 99},
		// No ifDescr PDUs: join yields nothing, SrcPort = "".
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].SrcPort != "" {
		t.Errorf("SrcPort = %q, want empty (no ifDescr overlap)", edges[0].SrcPort)
	}
}

// walkCircuitIfNames: WalkIfDescr fails (context cancelled after circuit BulkWalk
// succeeds) → function returns an error, exercising the "err != nil" branch after
// the WalkIfDescr call. Test calls walkCircuitIfNames directly to avoid the
// walkAdjIPAddrs BulkWalk also being cancelled.
func TestWalkCircuitIfNamesIfDescrFails(t *testing.T) {
	const circBase = ".1.3.6.1.2.1.138.1.4.1.1.3."
	pdus := []gsnmp.SnmpPDU{
		{Name: circBase + "0.1", Type: gsnmp.Integer, Value: 3},
		{Name: ".1.3.6.1.2.1.2.2.1.2.3", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/0")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := snmputil.Open(p)
	if err != nil {
		t.Fatalf("snmputil.Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	// isisLazyErrCtx: first ctx.Err() call (circuit BulkWalk) returns nil;
	// second call (WalkIfDescr BulkWalk) returns Canceled → WalkIfDescr fails.
	ctx := &isisLazyErrCtx{Context: context.Background(), failAfter: 1}

	result, _, circErr := walkCircuitIfNames(ctx, client)
	if circErr == nil {
		t.Fatal("expected error when WalkIfDescr fails, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result on error, got %v", result)
	}
}

// isisLazyErrCtx returns nil from Err() for the first failAfter calls, then
// returns context.Canceled. Used to fail BulkWalk on the Nth call while
// letting earlier calls succeed.
type isisLazyErrCtx struct {
	context.Context
	calls     int
	failAfter int
}

func (c *isisLazyErrCtx) Err() error {
	c.calls++
	if c.calls > c.failAfter {
		return context.Canceled
	}
	return nil
}

// Walk: malformed adjKey with only 1 dot-separated component → edge emitted with
// DegradedReasonMissingSrcPortMapping. This exercises the circKey == "" branch
// added in the fix to the degraded-reason condition.
//
// OID structure: the suffix after the adjIPBase prefix is "col.1.4.ip0.ip1.ip2.ip3"
// (7 parts). Stripping the last ipv4TailLen=6 parts yields adjKey="col" (1 component).
// SplitN on "." with limit 3 returns only 1 element, so circKey stays empty.
func TestWalkMalformedAdjKeyDegradedReason(t *testing.T) {
	// adjKey "2" has a single component: col=2, no sysInst/circIdx/adjIdx.
	// State OID: adjStateBase+"2"; IP OID: adjIPBase+"2.1.4.192.0.2.2".
	const malformedStateKey = "2"
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + malformedStateKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + malformedStateKey + ".1.4.192.0.2.2", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 2}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope, got %d", len(oos))
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge (malformed adjKey still emits), got %d", len(edges))
	}
	e := edges[0]
	if e.Metadata == nil {
		t.Fatal("Metadata is nil, want degraded metadata")
	}
	if e.Metadata[discovery.MetadataKeyDegraded] != "true" {
		t.Errorf("%s = %q, want true", discovery.MetadataKeyDegraded, e.Metadata[discovery.MetadataKeyDegraded])
	}
	if e.Metadata[discovery.MetadataKeyDegradedReason] != discovery.DegradedReasonMissingSrcPortMapping {
		t.Errorf("%s = %q, want %s", discovery.MetadataKeyDegradedReason, e.Metadata[discovery.MetadataKeyDegradedReason], discovery.DegradedReasonMissingSrcPortMapping)
	}
}

// Walk: Open fails when the connection cannot be established.
func TestWalkOpenFails(t *testing.T) {
	p := snmputil.Params{
		IP:        net.ParseIP("127.0.0.1"),
		Port:      161,
		Community: []byte("public"),
		Timeout:   time.Nanosecond,
	}
	_, _, err := Walk(context.Background(), p, "router-a", nil)
	if err == nil {
		t.Fatal("expected error when Open fails, got nil")
	}
}

// Walk: up adjacency with IP = 0.0.0.0 (IsUnspecified) → no edge, no OOS.
func TestWalkFiltersUnspecifiedAdjIP(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + adjKey + ".1.4.0.0.0.0", Type: gsnmp.OctetString, Value: []byte{0, 0, 0, 0}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for unspecified adj IP, got %d", len(edges))
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope for unspecified adj IP, got %d", len(oos))
	}
}

// Walk: up adjacency with link-local IPv4 address (169.254.1.1) → no edge, no OOS.
func TestWalkFiltersLinkLocalAdjIP(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + adjKey + ".1.4.169.254.1.1", Type: gsnmp.OctetString, Value: []byte{169, 254, 1, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for link-local adj IP, got %d", len(edges))
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope for link-local adj IP, got %d", len(oos))
	}
}

// Walk: IPv6 adjacency PDU (ipSubType=2, ipLen=16) is skipped without
// affecting valid IPv4 edges on the same device. The IPv4 peer still
// produces a normal edge with no degraded metadata.
func TestWalkIPv6AdjacencySkipped(t *testing.T) {
	// sysInst=0, circIdx=1, adjIdx=1: state up for both adjKeys.
	const adjKeyIPv4 = "0.1.1"
	const adjKeyIPv6 = "0.1.2"
	// IPv6 OID suffix: adjKey + ".2.16." + 16 zero octets.
	ipv6Suffix := adjKeyIPv6 + ".2.16.32.1.13.184.0.0.0.0.0.0.0.0.0.0.0.1"
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKeyIPv4, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjStateBase + adjKeyIPv6, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + adjKeyIPv4 + ".1.4.192.0.2.1", Type: gsnmp.OctetString, Value: []byte{192, 0, 2, 1}},
		// IPv6 PDU: 16-octet value for 2001:db8::1; should be skipped.
		{Name: adjIPBase + ipv6Suffix, Type: gsnmp.OctetString, Value: []byte{32, 1, 13, 184, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope, got %d", len(oos))
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge (IPv4 only), got %d", len(edges))
	}
	e := edges[0]
	if e.DstDevice != "192.0.2.1" {
		t.Errorf("DstDevice = %q, want 192.0.2.1", e.DstDevice)
	}
	// The IPv4 edge may legitimately carry other degraded reasons (e.g.
	// missing_srcport_mapping when no circuit table is walked), but it MUST
	// NOT carry unsupported_ip_version — that signal applies to the skipped
	// IPv6 row, not to this fully valid IPv4 observation.
	if reason := e.Metadata[discovery.MetadataKeyDegradedReason]; strings.Contains(reason, discovery.DegradedReasonUnsupportedIPVersion) {
		t.Errorf("IPv4 edge tainted with %s because of an unrelated IPv6 adjacency on the same device; degraded_reason=%q", discovery.DegradedReasonUnsupportedIPVersion, reason)
	}
}

// Walk: IPv6-only adjacencies (no IPv4 peers) → no edges, no OOS, no error.
func TestWalkIPv6OnlyAdjacencies(t *testing.T) {
	const adjKeyIPv6 = "0.1.1"
	ipv6Suffix := adjKeyIPv6 + ".2.16.32.1.13.184.0.0.0.0.0.0.0.0.0.0.0.1"
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKeyIPv6, Type: gsnmp.Integer, Value: isisAdjStateUp},
		{Name: adjIPBase + ipv6Suffix, Type: gsnmp.OctetString, Value: []byte{32, 1, 13, 184, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, oos, err := Walk(context.Background(), p, "router-a", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for IPv6-only adjacencies, got %d", len(edges))
	}
	if len(oos) != 0 {
		t.Errorf("expected 0 out-of-scope, got %d", len(oos))
	}
}

// Walk: cancelled context → error returned from walkAdjStates.
func TestWalkCancelledContext(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: adjStateBase + adjKey, Type: gsnmp.Integer, Value: isisAdjStateUp},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := Walk(ctx, p, "router-a", nil)
	if err == nil {
		t.Fatal("expected error when context is cancelled, got nil")
	}
}
