package fdb

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	gsnmp "github.com/gosnmp/gosnmp"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	snmputil "github.com/grafana/network-topology-exporter/internal/discovery/snmp"
	"github.com/grafana/network-topology-exporter/internal/snmptest"
)

// buildEdges: one MAC on a port → AdjacencyDirect, ConfidenceMedium.
func TestBuildEdgesDirectAdjacency(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.10.187.204.221.238": {mac: []byte{0, 10, 187, 204, 221, 238}, port: 1, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{1: 2}
	ifNames := map[int]string{2: "GigabitEthernet0/1"}

	edges, _ := buildEdges(context.Background(), "sw-01", entries, bridgePorts, ifNames, nil)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.SrcDevice != "sw-01" {
		t.Errorf("SrcDevice = %q, want sw-01", e.SrcDevice)
	}
	if e.SrcPort != "GigabitEthernet0/1" {
		t.Errorf("SrcPort = %q, want GigabitEthernet0/1", e.SrcPort)
	}
	if e.DstDevice != "00:0a:bb:cc:dd:ee" {
		t.Errorf("DstDevice = %q, want 00:0a:bb:cc:dd:ee", e.DstDevice)
	}
	if e.Adjacency != discovery.AdjacencyDirect {
		t.Errorf("Adjacency = %q, want direct", e.Adjacency)
	}
	if e.Confidence != discovery.ConfidenceMedium {
		t.Errorf("Confidence = %q, want medium", e.Confidence)
	}
	if e.DiscoveryProto != "fdb" {
		t.Errorf("DiscoveryProto = %q, want fdb", e.DiscoveryProto)
	}
	if e.PrecedenceRank != precedenceRank {
		t.Errorf("PrecedenceRank = %d, want %d", e.PrecedenceRank, precedenceRank)
	}
}

// buildEdges: multiple MACs on a port → AdjacencyIndirect, ConfidenceLow.
func TestBuildEdgesIndirectAdjacency(t *testing.T) {
	// Two MACs on the same port → AdjacencyIndirect. These are suppressed to
	// avoid a cardinality bomb: without L3 ARP correlation the MAC cannot be
	// mapped to a device identity, so no edges are emitted.
	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 1, status: fdbStatusLearned},
		"0.1.2.3.4.6": {mac: []byte{0, 1, 2, 3, 4, 6}, port: 1, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{1: 3}
	ifNames := map[int]string{3: "Ethernet1/1"}

	edges, _ := buildEdges(context.Background(), "sw-01", entries, bridgePorts, ifNames, nil)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges for indirect port, got %d", len(edges))
	}
}

// buildEdges: status=self(4) and status=mgmt(5) entries are filtered out.
func TestBuildEdgesFiltersNonLearned(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 1, status: 4}, // self
		"0.1.2.3.4.6": {mac: []byte{0, 1, 2, 3, 4, 6}, port: 1, status: 5}, // mgmt
		"0.1.2.3.4.7": {mac: []byte{0, 1, 2, 3, 4, 7}, port: 1, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{1: 2}
	ifNames := map[int]string{2: "Gi0/1"}

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge (only learned), got %d", len(edges))
	}
	if edges[0].DstDevice != "00:01:02:03:04:07" {
		t.Errorf("DstDevice = %q, want 00:01:02:03:04:07", edges[0].DstDevice)
	}
}

// buildEdges: MAC with wrong byte length is skipped.
func TestBuildEdgesSkipsInvalidMAC(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.1.2.3":     {mac: []byte{0, 1, 2, 3}, port: 1, status: fdbStatusLearned}, // 4 bytes, not 6
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 1, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{1: 2}
	ifNames := map[int]string{2: "Gi0/1"}

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge (invalid MAC skipped), got %d", len(edges))
	}
}

// buildEdges: FDB entry whose bridge port has no base-port mapping is skipped.
func TestBuildEdgesMissingBridgePort(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 99, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{} // port 99 not present
	ifNames := map[int]string{}

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges when bridge port unmapped, got %d", len(edges))
	}
}

// buildEdges: falls back to "if{ifIndex}" when ifName is missing.
func TestBuildEdgesFallbackPortName(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 1, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{1: 7}
	ifNames := map[int]string{} // ifIndex 7 has no name

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].SrcPort != "if7" {
		t.Errorf("SrcPort = %q, want \"if7\" (fallback to if{ifIndex})", edges[0].SrcPort)
	}
	if edges[0].SrcIfIndex != 7 {
		t.Errorf("SrcIfIndex = %d, want 7", edges[0].SrcIfIndex)
	}
}

// buildEdges: port in STP forwarding state produces an edge.
func TestBuildEdgesStpForwardingAllowed(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 2, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{2: 4}
	ifNames := map[int]string{4: "Ethernet0/2"}
	stpStates := map[int]int{2: stpStateForwarding}

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, stpStates)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge for forwarding STP port, got %d", len(edges))
	}
}

// buildEdges: port in STP blocking state produces no edges.
func TestBuildEdgesStpBlockingFiltered(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 3, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{3: 5}
	ifNames := map[int]string{5: "Ethernet0/3"}
	stpStates := map[int]int{3: 2} // blocking(2)

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, stpStates)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges for blocking STP port, got %d", len(edges))
	}
}

// buildEdges: port absent from stpStates produces an edge (treat as forwarding).
func TestBuildEdgesStpAbsentAllowed(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 4, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{4: 6}
	ifNames := map[int]string{6: "Ethernet0/4"}
	stpStates := map[int]int{} // port 4 not present

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, stpStates)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge when port absent from STP table, got %d", len(edges))
	}
}

// buildEdges: port in STP learning state (not yet forwarding) is filtered.
func TestBuildEdgesStpLearningFiltered(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 5, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{5: 7}
	ifNames := map[int]string{7: "Ethernet0/5"}
	stpStates := map[int]int{5: 4} // learning(4)

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, stpStates)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges for learning STP port, got %d", len(edges))
	}
}

// Walk: end-to-end with a fake agent. One learned MAC on one port → one direct edge.
func TestWalkEndToEnd(t *testing.T) {
	pdus := buildFdbAgentPDUs()
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}

	edges, oos, err := Walk(context.Background(), p, "sw-01", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(oos) != 0 {
		t.Errorf("unexpected out-of-scope entries: %v", oos)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d: %v", len(edges), edges)
	}
	e := edges[0]
	if e.SrcDevice != "sw-01" {
		t.Errorf("SrcDevice = %q, want sw-01", e.SrcDevice)
	}
	if e.SrcPort != "GigabitEthernet0/1" {
		t.Errorf("SrcPort = %q, want GigabitEthernet0/1", e.SrcPort)
	}
	if e.DstDevice != "00:0a:bb:cc:dd:ee" {
		t.Errorf("DstDevice = %q, want 00:0a:bb:cc:dd:ee", e.DstDevice)
	}
	if e.Adjacency != discovery.AdjacencyDirect {
		t.Errorf("Adjacency = %q, want direct", e.Adjacency)
	}
}

func TestParseQBridgeIndex(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantKey string
		wantMAC []byte
		wantOK  bool
	}{
		{
			name:    "valid fdbId=1",
			input:   "1.0.10.187.204.221.238",
			wantKey: "0.10.187.204.221.238",
			wantMAC: []byte{0, 10, 187, 204, 221, 238},
			wantOK:  true,
		},
		{
			name:    "valid fdbId=100",
			input:   "100.0.1.2.3.4.5",
			wantKey: "0.1.2.3.4.5",
			wantMAC: []byte{0, 1, 2, 3, 4, 5},
			wantOK:  true,
		},
		{
			name:   "too short — only 6 components",
			input:  "1.2.3.4.5.6",
			wantOK: false,
		},
		{
			name:   "non-numeric component",
			input:  "1.x.1.2.3.4.5",
			wantOK: false,
		},
		{
			name:   "out of range octet",
			input:  "1.0.1.2.3.4.999",
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, mac, ok := parseQBridgeIndex(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if key != tc.wantKey {
				t.Errorf("key = %q, want %q", key, tc.wantKey)
			}
			if len(mac) != len(tc.wantMAC) {
				t.Fatalf("mac len = %d, want %d", len(mac), len(tc.wantMAC))
			}
			for i := range tc.wantMAC {
				if mac[i] != tc.wantMAC[i] {
					t.Errorf("mac[%d] = %d, want %d", i, mac[i], tc.wantMAC[i])
				}
			}
		})
	}
}

// Walk: end-to-end with B-MIB and Q-BRIDGE PDUs. B-MIB contributes one MAC,
// Q-BRIDGE contributes a second MAC on the same port → two direct edges.
func TestWalkEndToEndQBridge(t *testing.T) {
	pdus := buildQBridgeAgentPDUs()
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}

	edges, oos, err := Walk(context.Background(), p, "sw-01", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(oos) != 0 {
		t.Errorf("unexpected out-of-scope entries: %v", oos)
	}
	if len(edges) != 2 {
		t.Fatalf("expected 2 edges, got %d: %v", len(edges), edges)
	}

	seen := make(map[string]bool)
	for _, e := range edges {
		seen[e.DstDevice] = true
	}
	if !seen["00:0a:bb:cc:dd:ee"] {
		t.Errorf("expected DstDevice 00:0a:bb:cc:dd:ee in edges")
	}
	if !seen["00:aa:bb:cc:dd:01"] {
		t.Errorf("expected DstDevice 00:aa:bb:cc:dd:01 in edges")
	}
}

// TestDiscoverVlanIDs verifies that discoverVlanIDs parses dot1qVlanCurrentTable
// OID suffixes and returns a sorted, deduplicated slice of valid VLAN IDs.
func TestDiscoverVlanIDs(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		// .1.3.6.1.2.1.17.7.1.4.2.1.3.0.1 (col=3, timeMark=0, vlanId=1)
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.1.3.0.1", Type: gsnmp.Integer, Value: 1},
		// .1.3.6.1.2.1.17.7.1.4.2.1.3.0.10 (col=3, timeMark=0, vlanId=10)
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.1.3.0.10", Type: gsnmp.Integer, Value: 10},
		// .1.3.6.1.2.1.17.7.1.4.2.1.3.0.100 (col=3, timeMark=0, vlanId=100)
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.1.3.0.100", Type: gsnmp.Integer, Value: 100},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}
	client, err := snmputil.Open(p)
	if err != nil {
		t.Fatalf("snmputil.Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	ids := discoverVlanIDs(context.Background(), client)
	want := []int{1, 10, 100}
	if len(ids) != len(want) {
		t.Fatalf("discoverVlanIDs returned %v, want %v", ids, want)
	}
	for i, v := range want {
		if ids[i] != v {
			t.Errorf("ids[%d] = %d, want %d", i, ids[i], v)
		}
	}
}

// TestWalkVlanCommunityFdbDiscovery verifies that walkVlanCommunityFdbs discovers
// VLANs via dot1qVlanCurrentTable and then walks dot1dTpFdbTable for each VLAN
// using community-string indexing (community@vlanId).
func TestWalkVlanCommunityFdbDiscovery(t *testing.T) {
	vlanTablePDUs := []gsnmp.SnmpPDU{
		// dot1qVlanCurrentTable: VLAN 10 active
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.1.3.0.10", Type: gsnmp.Integer, Value: 10},
		// dot1dBasePortTable: bridge port 1 → ifIndex 2
		{Name: ".1.3.6.1.2.1.17.1.4.1.2.1", Type: gsnmp.Integer, Value: 2},
		// ifXTable.ifName: ifIndex 2 → "GigabitEthernet0/2"
		{Name: ".1.3.6.1.2.1.31.1.1.1.1.2", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/2")},
	}

	// B-MIB FDB PDUs for MAC 00:aa:bb:cc:dd:ee on bridge port 1 (learned)
	vlan10PDUs := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.4.3.1.1.0.170.187.204.221.238", Type: gsnmp.OctetString, Value: []byte{0, 170, 187, 204, 221, 238}},
		{Name: ".1.3.6.1.2.1.17.4.3.1.2.0.170.187.204.221.238", Type: gsnmp.Integer, Value: 1},
		{Name: ".1.3.6.1.2.1.17.4.3.1.3.0.170.187.204.221.238", Type: gsnmp.Integer, Value: fdbStatusLearned},
	}

	communities := map[string][]gsnmp.SnmpPDU{
		"public":    vlanTablePDUs,
		"public@10": vlan10PDUs,
	}
	addr := snmptest.StartMultiCommunity(t, communities)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}

	edges, _, err := Walk(context.Background(), p, "sw-01", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	found := false
	for _, e := range edges {
		if e.DstDevice == "00:aa:bb:cc:dd:ee" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected edge with DstDevice 00:aa:bb:cc:dd:ee, got %v", edges)
	}
}

// startLimitedAgent starts a UDP SNMPv2c agent that responds to at most
// maxResponses GetBulk/GetNext/Get requests with empty (EndOfMibView) results,
// then stops responding. This lets tests drive Walk() through its sequential
// BulkWalk calls, making the Nth+1 call fail by timeout.
//
// The Params returned point at the agent with a 50 ms timeout. At most
// one retry (the gosnmp default) is performed per BulkWalkAll call, so each
// failed call costs ~100 ms.  WalkAll fallback also adds ~100 ms, capping
// total per-Walk-call cost at ~200 ms.
//
// Count of BulkWalk calls Walk() makes (with community set, !V3):
//
//	#1  walkFdbTable        → oidFdbTable
//	#2  walkQBridgeFdbTable → oidQBridgeFdbTable   (error discarded)
//	#3  discoverVlanIDs     → oidVlanCurrentTable  (called by walkVlanCommunityFdbs)
//	#4  walkBasePortTable   → oidBasePortTable
//	#5  walkStpPortStates   → oidStpPortTable
//	#6  WalkIfNames         → oidIfNameTable
//
// Set maxResponses to the number of successful (empty) responses the agent
// should send before going dark. The Walk call will then fail on the next BulkWalk.
func startLimitedAgent(t *testing.T, community string, maxResponses int) (net.IP, uint16) {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startLimitedAgent: listen: %v", err)
	}

	done := make(chan struct{})
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})

	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		decoder := &gsnmp.GoSNMP{Version: gsnmp.Version2c, Community: community}
		responded := 0
		for responded < maxResponses {
			n, src, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			pkt, err := decoder.SnmpDecodePacket(buf[:n])
			if err != nil || pkt.Community != community {
				continue
			}
			// Return EndOfMibView for every requested variable.
			vars := make([]gsnmp.SnmpPDU, len(pkt.Variables))
			for i, v := range pkt.Variables {
				vars[i] = gsnmp.SnmpPDU{Name: v.Name, Type: gsnmp.EndOfMibView}
			}
			reply := &gsnmp.SnmpPacket{
				Version:   gsnmp.Version2c,
				Community: community,
				PDUType:   gsnmp.GetResponse,
				RequestID: pkt.RequestID,
				Variables: vars,
			}
			raw, err := reply.MarshalMsg()
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(raw, src)
			responded++
		}
		// Stop responding: close the connection so no further replies are sent.
		_ = conn.Close()
	}()

	host, portStr, _ := net.SplitHostPort(conn.LocalAddr().String())
	portVal, _ := net.LookupPort("udp", portStr)
	return net.ParseIP(host), uint16(portVal) //nolint:gosec // net.LookupPort always returns 0–65535
}

// openDeadClient opens a GoSNMP session that will time out on any BulkWalk,
// for use in tests that call individual walk functions directly.
func openDeadClient(t *testing.T) *gsnmp.GoSNMP {
	t.Helper()
	// Nothing listens on port 1; UDP Connect succeeds but BulkWalkAll will time out.
	// We use a cancelled context in the actual test, so no real timeout is incurred.
	client := &gsnmp.GoSNMP{
		Target:    "127.0.0.1",
		Port:      1,
		Community: "public",
		Version:   gsnmp.Version2c,
		Timeout:   50 * time.Millisecond,
		Retries:   0,
		MaxOids:   gsnmp.MaxOids,
		Transport: "udp",
	}
	if err := client.Connect(); err != nil {
		t.Fatalf("openDeadClient: Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Conn.Close() })
	return client
}

// cancelledCtx returns a context that has already been cancelled.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// openClientToAgent starts a snmptest agent with the given PDUs and opens a
// GoSNMP client pointing at it. Returns the client; cleanup is handled by t.
func openClientToAgent(t *testing.T, community string, pdus []gsnmp.SnmpPDU) *gsnmp.GoSNMP {
	t.Helper()
	addr := snmptest.Start(t, community, pdus)
	ip, port := snmptest.ParseAddr(addr)
	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte(community),
		Timeout:   3 * time.Second,
	}
	client, err := snmputil.Open(p)
	if err != nil {
		t.Fatalf("openClientToAgent: Open: %v", err)
	}
	t.Cleanup(func() { _ = client.Conn.Close() })
	return client
}

// ---------------------------------------------------------------------------
// Walk() error paths
// ---------------------------------------------------------------------------

// Walk: nil IP causes snmputil.Open to fail → Walk returns error.
func TestWalkOpenFails(t *testing.T) {
	p := snmputil.Params{
		IP:        net.IP(nil), // "nil" resolves to "<nil>" which fails DNS lookup
		Port:      12345,
		Community: []byte("public"),
		Timeout:   50 * time.Millisecond,
	}
	_, _, err := Walk(context.Background(), p, "sw", nil)
	if err == nil {
		t.Fatal("expected error when IP is nil, got nil")
	}
	if !strings.Contains(err.Error(), "fdb") {
		t.Errorf("error %q does not mention 'fdb'", err)
	}
}

// Walk: walkFdbTable fails (agent gives no response) → Walk returns "fdb table" error.
func TestWalkFdbTableFails(t *testing.T) {
	ip, port := startLimitedAgent(t, "public", 0) // responds to 0 requests
	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   50 * time.Millisecond,
	}
	_, _, err := Walk(context.Background(), p, "sw", nil)
	if err == nil {
		t.Fatal("expected error when FDB walk times out, got nil")
	}
	if !strings.Contains(err.Error(), "fdb table") {
		t.Errorf("error %q does not mention 'fdb table'", err)
	}
}

// Walk: walkBasePortTable fails → Walk returns "fdb baseport" error.
// The agent responds to the first 3 BulkWalk calls (FDB, Q-BRIDGE, VLAN
// discovery) with empty results, then stops responding.
func TestWalkBasePortTableFails(t *testing.T) {
	ip, port := startLimitedAgent(t, "public", 3)
	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   50 * time.Millisecond,
	}
	_, _, err := Walk(context.Background(), p, "sw", nil)
	if err == nil {
		t.Fatal("expected error when basePort walk fails, got nil")
	}
	if !strings.Contains(err.Error(), "fdb baseport") {
		t.Errorf("error %q does not mention 'fdb baseport'", err)
	}
}

// Walk: walkStpPortStates fails → Walk returns "fdb stpport" error.
func TestWalkStpPortStatesFails(t *testing.T) {
	ip, port := startLimitedAgent(t, "public", 4)
	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   50 * time.Millisecond,
	}
	_, _, err := Walk(context.Background(), p, "sw", nil)
	if err == nil {
		t.Fatal("expected error when STP walk fails, got nil")
	}
	if !strings.Contains(err.Error(), "fdb stpport") {
		t.Errorf("error %q does not mention 'fdb stpport'", err)
	}
}

// Walk: WalkIfNames fails → Walk returns "fdb ifname" error.
func TestWalkIfNamesFails(t *testing.T) {
	ip, port := startLimitedAgent(t, "public", 5)
	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   50 * time.Millisecond,
	}
	_, _, err := Walk(context.Background(), p, "sw", nil)
	if err == nil {
		t.Fatal("expected error when ifName walk fails, got nil")
	}
	if !strings.Contains(err.Error(), "fdb ifname") {
		t.Errorf("error %q does not mention 'fdb ifname'", err)
	}
}

// ---------------------------------------------------------------------------
// walkFdbTableInto() coverage gaps
// ---------------------------------------------------------------------------

// walkFdbTableInto: BulkWalk fails → returns error immediately.
func TestWalkFdbTableIntoBulkWalkError(t *testing.T) {
	client := openDeadClient(t)
	entries := make(map[string]*fdbEntry)
	_, err := walkFdbTableInto(cancelledCtx(), client, entries)
	if err == nil {
		t.Fatal("expected error from walkFdbTableInto with cancelled context, got nil")
	}
}

// walkFdbTableInto: PDU whose name starts with oidFdbTable but not with the
// ".1." sub-table prefix is silently skipped (TrimOIDPrefix returns !ok).
// We inject a PDU under the fictitious ".2." sub-table of dot1dTpFdbTable.
func TestWalkFdbTableIntoTrimPrefixSkip(t *testing.T) {
	// ".1.3.6.1.2.1.17.4.3.2.1.0.1.2.3.4.5" is within the oidFdbTable subtree
	// (".1.3.6.1.2.1.17.4.3") but NOT under the ".1." entry sub-table that
	// walkFdbTableInto expects. BulkWalk returns it; TrimOIDPrefix rejects it.
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.4.3.2.1.0.1.2.3.4.5", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	_, err := walkFdbTableInto(context.Background(), client, entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries (PDU skipped), got %d", len(entries))
	}
}

// walkFdbTableInto: PDU with column-only OID (no MAC instance suffix) →
// SplitOIDComponent returns macKey=="" → entry silently skipped.
func TestWalkFdbTableIntoMacKeyEmpty(t *testing.T) {
	// ".1.3.6.1.2.1.17.4.3.1.3" has suffix "3" after the prefix;
	// SplitOIDComponent("3") returns (3, "", true) → macKey=="" → skip.
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.4.3.1.3", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	_, err := walkFdbTableInto(context.Background(), client, entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries (column-only OID skipped), got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// walkQBridgeFdbTable() coverage gaps
// ---------------------------------------------------------------------------

// walkQBridgeFdbTable: BulkWalk fails → returns error.
func TestWalkQBridgeFdbTableBulkWalkError(t *testing.T) {
	client := openDeadClient(t)
	err := walkQBridgeFdbTable(cancelledCtx(), client, make(map[string]*fdbEntry))
	if err == nil {
		t.Fatal("expected error from walkQBridgeFdbTable with cancelled context, got nil")
	}
}

// walkQBridgeFdbTable: PDU whose name equals the table OID root (without the
// trailing ".") is returned by gosnmp in its GetRequest fallback path and has a
// name that does NOT start with the expected ".1.3.6.1.2.1.17.7.1.2.2." prefix.
// TrimOIDPrefix returns !ok → entry silently skipped.
//
// To trigger gosnmp's GetRequest fallback we include one PDU outside the
// table subtree so that the first GetBulk response is out-of-range, causing
// gosnmp to retry with GetRequest.  The GetRequest returns the exact table-OID
// PDU (no trailing "."), which fails the prefix check.
func TestWalkQBridgeFdbTableTrimPrefixSkip(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		// Exact table OID: returned by gosnmp's GetRequest fallback as a leaf.
		{Name: ".1.3.6.1.2.1.17.7.1.2.2", Type: gsnmp.Integer, Value: 0},
		// Out-of-range OID: causes gosnmp to treat the table as a leaf and
		// retry with GetRequest on the first walk iteration.
		{Name: ".1.3.6.1.2.1.17.7.1.2.9", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	err := walkQBridgeFdbTable(context.Background(), client, entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries (prefix-skip PDU), got %d", len(entries))
	}
}

// walkQBridgeFdbTable: PDU with column-only OID (no VLAN+MAC instance) →
// SplitOIDComponent returns rest=="" → entry silently skipped.
func TestWalkQBridgeFdbTableRestEmpty(t *testing.T) {
	// Suffix "2" after the prefix → SplitOIDComponent("2") → (2,"",true) → rest=="" → skip.
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.7.1.2.2.2", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	err := walkQBridgeFdbTable(context.Background(), client, entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries (rest-empty skip), got %d", len(entries))
	}
}

// walkQBridgeFdbTable: PDU with column number other than colQBridgePort(2) or
// colQBridgeStatus(3) → entry silently skipped.
func TestWalkQBridgeFdbTableColumnSkip(t *testing.T) {
	// Column 1 is not colQBridgePort (2) or colQBridgeStatus (3) → skip.
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.7.1.2.2.1.10.0.1.2.3.4.5", Type: gsnmp.Integer, Value: 1},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	err := walkQBridgeFdbTable(context.Background(), client, entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries (column skip), got %d", len(entries))
	}
}

// walkQBridgeFdbTable: PDU instance with fewer than 7 components → parseQBridgeIndex
// returns !ok → entry silently skipped.
func TestWalkQBridgeFdbTableInvalidInstance(t *testing.T) {
	// Suffix after the prefix: "2.10.0.1.2" — only 5 components, needs ≥7.
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.7.1.2.2.2.10.0.1.2", Type: gsnmp.Integer, Value: 1},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	err := walkQBridgeFdbTable(context.Background(), client, entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries (invalid instance skip), got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// discoverVlanIDs() coverage gaps
// ---------------------------------------------------------------------------

// discoverVlanIDs: BulkWalk fails → returns nil (not a panic).
func TestDiscoverVlanIDsBulkWalkError(t *testing.T) {
	client := openDeadClient(t)
	ids := discoverVlanIDs(cancelledCtx(), client)
	if ids != nil {
		t.Errorf("expected nil on BulkWalk error, got %v", ids)
	}
}

// discoverVlanIDs: PDU with the exact table OID (no trailing ".") is returned
// via gosnmp's GetRequest fallback and fails TrimOIDPrefix → entry skipped.
func TestDiscoverVlanIDsTrimPrefixSkip(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.7.1.4.2", Type: gsnmp.Integer, Value: 0},
		{Name: ".1.3.6.1.2.1.17.7.1.4.9", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	ids := discoverVlanIDs(context.Background(), client)
	if ids != nil {
		t.Errorf("expected nil (PDU skipped, no valid VLANs), got %v", ids)
	}
}

// discoverVlanIDs: PDU with column-only OID (no timeMark or vlanID) →
// SplitOIDComponent returns rest=="" → entry skipped.
func TestDiscoverVlanIDsRestEmpty(t *testing.T) {
	// ".1.3.6.1.2.1.17.7.1.4.2.3" → suffix "3" → SplitOIDComponent → (3,"",true) → skip.
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.3", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	ids := discoverVlanIDs(context.Background(), client)
	if ids != nil {
		t.Errorf("expected nil (column-only OID skipped), got %v", ids)
	}
}

// discoverVlanIDs: PDU with col.timeMark only (no vlanID suffix) →
// second SplitOIDComponent returns vlanStr=="" → entry skipped.
func TestDiscoverVlanIDsVlanStrEmpty(t *testing.T) {
	// ".1.3.6.1.2.1.17.7.1.4.2.3.0" → suffix "3.0" →
	// first split: (3,"0",true) → second split of "0": (0,"",true) → vlanStr="" → skip.
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.3.0", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	ids := discoverVlanIDs(context.Background(), client)
	if ids != nil {
		t.Errorf("expected nil (no-vlanID OID skipped), got %v", ids)
	}
}

// discoverVlanIDs: VLAN ID out of range (0 and 5000) → entries skipped.
func TestDiscoverVlanIDsOutOfRange(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		// vlanID = 0: below minimum (1)
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.3.0.0", Type: gsnmp.Integer, Value: 0},
		// vlanID = 5000: above maximum (4094)
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.3.0.5000", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	ids := discoverVlanIDs(context.Background(), client)
	if ids != nil {
		t.Errorf("expected nil (all VLAN IDs out of range), got %v", ids)
	}
}

// ---------------------------------------------------------------------------
// walkVlanCommunityFdbs() coverage gaps
// ---------------------------------------------------------------------------

// TestWalkVlanCommunityFdbsMaxVlans verifies that when MaxVlans is 2 and 3 VLANs
// are discovered, only the first 2 are walked.
func TestWalkVlanCommunityFdbsMaxVlans(t *testing.T) {
	// Three VLANs in dot1qVlanCurrentTable.
	vlanTablePDUs := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.1.3.0.10", Type: gsnmp.Integer, Value: 10},
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.1.3.0.20", Type: gsnmp.Integer, Value: 20},
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.1.3.0.30", Type: gsnmp.Integer, Value: 30},
	}

	// FDB entries for each per-VLAN community.
	mac10 := []byte{0, 0xAA, 0xBB, 0xCC, 0xDD, 0x10}
	mac20 := []byte{0, 0xAA, 0xBB, 0xCC, 0xDD, 0x20}
	mac30 := []byte{0, 0xAA, 0xBB, 0xCC, 0xDD, 0x30}

	vlan10PDUs := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.4.3.1.1.0.170.187.204.221.16", Type: gsnmp.OctetString, Value: mac10},
		{Name: ".1.3.6.1.2.1.17.4.3.1.2.0.170.187.204.221.16", Type: gsnmp.Integer, Value: 1},
		{Name: ".1.3.6.1.2.1.17.4.3.1.3.0.170.187.204.221.16", Type: gsnmp.Integer, Value: fdbStatusLearned},
	}
	vlan20PDUs := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.4.3.1.1.0.170.187.204.221.32", Type: gsnmp.OctetString, Value: mac20},
		{Name: ".1.3.6.1.2.1.17.4.3.1.2.0.170.187.204.221.32", Type: gsnmp.Integer, Value: 1},
		{Name: ".1.3.6.1.2.1.17.4.3.1.3.0.170.187.204.221.32", Type: gsnmp.Integer, Value: fdbStatusLearned},
	}
	// VLAN 30 PDUs — these must NOT be collected when MaxVlans=2.
	vlan30PDUs := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.4.3.1.1.0.170.187.204.221.48", Type: gsnmp.OctetString, Value: mac30},
		{Name: ".1.3.6.1.2.1.17.4.3.1.2.0.170.187.204.221.48", Type: gsnmp.Integer, Value: 1},
		{Name: ".1.3.6.1.2.1.17.4.3.1.3.0.170.187.204.221.48", Type: gsnmp.Integer, Value: fdbStatusLearned},
	}

	communities := map[string][]gsnmp.SnmpPDU{
		"public":    vlanTablePDUs,
		"public@10": vlan10PDUs,
		"public@20": vlan20PDUs,
		"public@30": vlan30PDUs,
	}
	addr := snmptest.StartMultiCommunity(t, communities)
	ip, port := snmptest.ParseAddr(addr)

	// Open a client pointing at the agent.
	mainClient, err := snmputil.Open(snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = mainClient.Conn.Close() }()

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}
	entries := make(map[string]*fdbEntry)
	// MaxVlans=2: only VLANs 10 and 20 should be walked; VLAN 30 must be skipped.
	walkVlanCommunityFdbs(context.Background(), &p, mainClient, entries, 2)

	// VLAN 10 and 20 entries should be present.
	key10 := "0.170.187.204.221.16"
	key20 := "0.170.187.204.221.32"
	key30 := "0.170.187.204.221.48"

	if _, ok := entries[key10]; !ok {
		t.Errorf("expected VLAN 10 entry (key %q) to be present", key10)
	}
	if _, ok := entries[key20]; !ok {
		t.Errorf("expected VLAN 20 entry (key %q) to be present", key20)
	}
	if _, ok := entries[key30]; ok {
		t.Errorf("expected VLAN 30 entry (key %q) to be absent (MaxVlans=2)", key30)
	}
}

// walkVlanCommunityFdbs: V3 session → early return (community-string indexing
// is SNMPv2c-only).
func TestWalkVlanCommunityFdbsV3Skip(t *testing.T) {
	client := openDeadClient(t)
	entries := make(map[string]*fdbEntry)
	p := snmputil.Params{V3: true, Community: []byte("public")}
	// Should return without calling anything on client (which is dead).
	walkVlanCommunityFdbs(context.Background(), &p, client, entries, 100)
}

// walkVlanCommunityFdbs: per-VLAN snmputil.Open fails (nil IP) → continue
// without panicking or aborting.
//
// discoverVlanIDs is called with a real client (pointing at the agent), so it
// discovers one VLAN. The per-VLAN Open then uses p.IP=nil, which resolves to
// the invalid host "<nil>", causing Connect to fail and the iteration to be
// skipped via continue.
func TestWalkVlanCommunityFdbsOpenFails(t *testing.T) {
	vlanTablePDUs := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.7.1.4.2.1.3.0.10", Type: gsnmp.Integer, Value: 10},
	}
	addr := snmptest.Start(t, "public", vlanTablePDUs)
	ip, port := snmptest.ParseAddr(addr)

	// Open a real client to the agent for discoverVlanIDs to use.
	realClient, err := snmputil.Open(snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = realClient.Conn.Close() }()

	// p.IP = nil means vp.IP is also nil; snmputil.Open(vp) will fail DNS lookup
	// ("<nil>" is not a resolvable host), hitting the `continue` branch.
	p := snmputil.Params{
		IP:        net.IP(nil),
		Port:      port,
		Community: []byte("public"),
		Timeout:   50 * time.Millisecond,
	}
	entries := make(map[string]*fdbEntry)
	// Should not panic and should not add any entries.
	walkVlanCommunityFdbs(context.Background(), &p, realClient, entries, 100)
}

// TestWalkVlanCommunityFdbsParallel verifies that walkVlanCommunityFdbs merges
// results from all VLANs correctly when walks run concurrently, and that entries
// from an earlier VLAN are not overwritten by a later one (no-overwrite semantics).
func TestWalkVlanCommunityFdbsParallel(t *testing.T) {
	// Four VLANs, each with one unique MAC entry.
	vlanIDs := []int{10, 20, 30, 40}

	// MAC key format used by walkFdbTableInto: decimal octets joined by "."
	macs := map[int][]byte{
		10: {0, 0xAA, 0xBB, 0xCC, 0xDD, 0x10},
		20: {0, 0xAA, 0xBB, 0xCC, 0xDD, 0x20},
		30: {0, 0xAA, 0xBB, 0xCC, 0xDD, 0x30},
		40: {0, 0xAA, 0xBB, 0xCC, 0xDD, 0x40},
	}
	macKeys := map[int]string{
		10: "0.170.187.204.221.16",
		20: "0.170.187.204.221.32",
		30: "0.170.187.204.221.48",
		40: "0.170.187.204.221.64",
	}

	buildFdbPDUs := func(mac []byte, key string) []gsnmp.SnmpPDU {
		base := ".1.3.6.1.2.1.17.4.3.1."
		return []gsnmp.SnmpPDU{
			{Name: base + "1." + key, Type: gsnmp.OctetString, Value: mac},
			{Name: base + "2." + key, Type: gsnmp.Integer, Value: 1},
			{Name: base + "3." + key, Type: gsnmp.Integer, Value: fdbStatusLearned},
		}
	}

	// dot1qVlanCurrentTable PDUs for the "public" community.
	vlanTablePDUs := make([]gsnmp.SnmpPDU, 0, len(vlanIDs))
	for _, id := range vlanIDs {
		vlanTablePDUs = append(vlanTablePDUs, gsnmp.SnmpPDU{
			Name:  fmt.Sprintf(".1.3.6.1.2.1.17.7.1.4.2.1.3.0.%d", id),
			Type:  gsnmp.Integer,
			Value: id,
		})
	}

	communities := map[string][]gsnmp.SnmpPDU{
		"public": vlanTablePDUs,
	}
	for _, id := range vlanIDs {
		comm := fmt.Sprintf("public@%d", id)
		communities[comm] = buildFdbPDUs(macs[id], macKeys[id])
	}

	addr := snmptest.StartMultiCommunity(t, communities)
	ip, port := snmptest.ParseAddr(addr)

	mainClient, err := snmputil.Open(snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = mainClient.Conn.Close() }()

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}

	// Seed entries with VLAN 10's MAC already present; it must not be overwritten.
	preExistingEntry := &fdbEntry{mac: macs[10], port: 99, status: fdbStatusLearned}
	entries := map[string]*fdbEntry{
		macKeys[10]: preExistingEntry,
	}

	walkVlanCommunityFdbs(context.Background(), &p, mainClient, entries, 100)

	// All four VLAN entries must be present.
	for _, id := range vlanIDs {
		if _, ok := entries[macKeys[id]]; !ok {
			t.Errorf("expected entry for VLAN %d (key %q) to be present", id, macKeys[id])
		}
	}

	// The pre-existing VLAN 10 entry must not have been overwritten.
	if got := entries[macKeys[10]]; got != preExistingEntry {
		t.Errorf("VLAN 10 entry was overwritten; want original pointer, got different entry")
	}

	// No duplicate keys: total entry count must equal 4 (10, 20, 30, 40).
	if len(entries) != 4 {
		t.Errorf("expected 4 entries, got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// walkBasePortTable() coverage gaps
// ---------------------------------------------------------------------------

// walkBasePortTable: BulkWalk fails → returns error.
func TestWalkBasePortTableBulkWalkError(t *testing.T) {
	client := openDeadClient(t)
	_, err := walkBasePortTable(cancelledCtx(), client)
	if err == nil {
		t.Fatal("expected error from walkBasePortTable with cancelled context, got nil")
	}
}

// walkBasePortTable: PDU under the ".2." sub-table of dot1dBasePortTable (not
// ".1.") fails TrimOIDPrefix → entry silently skipped.
func TestWalkBasePortTableTrimPrefixSkip(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.1.4.2.2.1", Type: gsnmp.Integer, Value: 3},
	}
	client := openClientToAgent(t, "public", pdus)
	ports, err := walkBasePortTable(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ports) != 0 {
		t.Errorf("expected 0 ports (prefix-skip PDU), got %d", len(ports))
	}
}

// walkBasePortTable: PDU with a column number other than colBasePortIfIndex(2)
// → col != colBasePortIfIndex → entry silently skipped.
func TestWalkBasePortTableColSkip(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		// column 1, not column 2 (colBasePortIfIndex)
		{Name: ".1.3.6.1.2.1.17.1.4.1.1.5", Type: gsnmp.Integer, Value: 7},
	}
	client := openClientToAgent(t, "public", pdus)
	ports, err := walkBasePortTable(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ports) != 0 {
		t.Errorf("expected 0 ports (col != colBasePortIfIndex), got %d", len(ports))
	}
}

// walkBasePortTable: PDU OID ".1.3.6.1.2.1.17.1.4.1.2" — column 2 with no
// port number suffix → portStr="" → strconv.Atoi("") fails → entry skipped.
func TestWalkBasePortTablePortStrEmpty(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.1.4.1.2", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	ports, err := walkBasePortTable(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ports) != 0 {
		t.Errorf("expected 0 ports (portStr empty), got %d", len(ports))
	}
}

// walkBasePortTable: bridge port index 0 is invalid per RFC 4188 and must be
// skipped. Port index 1 in the same response is valid and must be stored.
func TestWalkBasePortTablePortZeroSkipped(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		// port 0 → ifIndex 99: should be skipped (invalid RFC 4188 index)
		{Name: ".1.3.6.1.2.1.17.1.4.1.2.0", Type: gsnmp.Integer, Value: 99},
		// port 1 → ifIndex 2: should be stored
		{Name: ".1.3.6.1.2.1.17.1.4.1.2.1", Type: gsnmp.Integer, Value: 2},
	}
	client := openClientToAgent(t, "public", pdus)
	ports, err := walkBasePortTable(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ports[0]; ok {
		t.Errorf("port 0 must not appear in ports map (RFC 4188: indices are 1-based)")
	}
	if ifIdx, ok := ports[1]; !ok || ifIdx != 2 {
		t.Errorf("expected ports[1] = 2, got %d (ok=%v)", ifIdx, ok)
	}
}

// ---------------------------------------------------------------------------
// buildEdges: special MAC filtering
// ---------------------------------------------------------------------------

// buildEdges: broadcast MAC (ff:ff:ff:ff:ff:ff) produces no edge.
func TestBuildEdgesBroadcastMACFiltered(t *testing.T) {
	entries := map[string]*fdbEntry{
		"255.255.255.255.255.255": {mac: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, port: 1, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{1: 2}
	ifNames := map[int]string{2: "Gi0/1"}

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges for broadcast MAC, got %d: %v", len(edges), edges)
	}
}

// buildEdges: all-zeros MAC (00:00:00:00:00:00) produces no edge.
func TestBuildEdgesAllZerosMACFiltered(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.0.0.0.0.0": {mac: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, port: 1, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{1: 2}
	ifNames := map[int]string{2: "Gi0/1"}

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges for all-zeros MAC, got %d: %v", len(edges), edges)
	}
}

// buildEdges: multicast MAC (I/G bit set on first byte) produces no edge.
// 01:00:5e:00:00:01 is a well-known IPv4 multicast MAC.
func TestBuildEdgesMulticastMACFiltered(t *testing.T) {
	entries := map[string]*fdbEntry{
		"1.0.94.0.0.1": {mac: []byte{0x01, 0x00, 0x5e, 0x00, 0x00, 0x01}, port: 1, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{1: 2}
	ifNames := map[int]string{2: "Gi0/1"}

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges for multicast MAC, got %d: %v", len(edges), edges)
	}
}

// buildEdges: fdbEntry with valid MAC, learned status, but port=0 produces no edge.
// This covers the case where an SNMP walk returned a MAC+status row but no port
// row (partial table response), leaving e.port at its zero value.
func TestBuildEdgesSkipsZeroPort(t *testing.T) {
	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 0, status: fdbStatusLearned},
	}
	bridgePorts := map[int]int{1: 2}
	ifNames := map[int]string{2: "Gi0/1"}

	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges for port=0 entry, got %d: %v", len(edges), edges)
	}
}

// ---------------------------------------------------------------------------
// walkStpPortStates() coverage gaps
// ---------------------------------------------------------------------------

// walkStpPortStates: BulkWalk fails → returns error.
func TestWalkStpPortStatesBulkWalkError(t *testing.T) {
	client := openDeadClient(t)
	_, err := walkStpPortStates(cancelledCtx(), client)
	if err == nil {
		t.Fatal("expected error from walkStpPortStates with cancelled context, got nil")
	}
}

// walkStpPortStates: PDU under the ".2." sub-table of dot1dStpPortTable fails
// TrimOIDPrefix → entry silently skipped.
func TestWalkStpPortStatesTrimPrefixSkip(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.2.15.2.3.1", Type: gsnmp.Integer, Value: 5},
	}
	client := openClientToAgent(t, "public", pdus)
	states, err := walkStpPortStates(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 states (prefix-skip PDU), got %d", len(states))
	}
}

// walkStpPortStates: PDU with column number other than colStpPortState(3)
// → col != colStpPortState → entry silently skipped.
func TestWalkStpPortStatesColSkip(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		// column 1, not column 3 (colStpPortState)
		{Name: ".1.3.6.1.2.1.17.2.15.1.1.5", Type: gsnmp.Integer, Value: 5},
	}
	client := openClientToAgent(t, "public", pdus)
	states, err := walkStpPortStates(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 states (col != colStpPortState), got %d", len(states))
	}
}

// walkStpPortStates: PDU OID ".1.3.6.1.2.1.17.2.15.1.3" — column 3 with no
// port number suffix → portStr="" → strconv.Atoi("") fails → entry skipped.
func TestWalkStpPortStatesPortStrEmpty(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.17.2.15.1.3", Type: gsnmp.Integer, Value: 0},
	}
	client := openClientToAgent(t, "public", pdus)
	states, err := walkStpPortStates(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected 0 states (portStr empty), got %d", len(states))
	}
}

// ---------------------------------------------------------------------------
// RFC 4188 port=0 rejection in walkFdbTableInto and walkQBridgeFdbTable
// ---------------------------------------------------------------------------

// walkFdbTableInto: a PDU reporting port=0 must not set e.port; the entry's
// port field stays 0 and buildEdges maps it to portMACs[0], which has no
// entry in bridgePorts, so no edge is emitted.
func TestWalkFdbTableIntoPortZeroRejected(t *testing.T) {
	macSuffix := "0.1.2.3.4.5"
	fdbBase := ".1.3.6.1.2.1.17.4.3.1."
	pdus := []gsnmp.SnmpPDU{
		{Name: fdbBase + "1." + macSuffix, Type: gsnmp.OctetString, Value: []byte{0, 1, 2, 3, 4, 5}},
		{Name: fdbBase + "2." + macSuffix, Type: gsnmp.Integer, Value: 0}, // port=0 → must be rejected
		{Name: fdbBase + "3." + macSuffix, Type: gsnmp.Integer, Value: fdbStatusLearned},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	if _, err := walkFdbTableInto(context.Background(), client, entries); err != nil {
		t.Fatalf("walkFdbTableInto: %v", err)
	}
	e, ok := entries[macSuffix]
	if !ok {
		t.Fatalf("expected entry for key %q", macSuffix)
	}
	if e.port != 0 {
		t.Errorf("port = %d, want 0 (port=0 PDU must not update e.port)", e.port)
	}

	// buildEdges with no bridgePorts mapping for port 0 → no edges.
	bridgePorts := map[int]int{1: 2} // port 1 mapped; port 0 intentionally absent
	ifNames := map[int]string{2: "Gi0/1"}
	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for port=0 entry, got %d: %v", len(edges), edges)
	}
}

// walkFdbTableInto: a PDU reporting port=1 must set e.port to 1 and
// buildEdges must produce one direct edge (existing behaviour unchanged).
func TestWalkFdbTableIntoPortOneAccepted(t *testing.T) {
	macSuffix := "0.1.2.3.4.5"
	fdbBase := ".1.3.6.1.2.1.17.4.3.1."
	pdus := []gsnmp.SnmpPDU{
		{Name: fdbBase + "1." + macSuffix, Type: gsnmp.OctetString, Value: []byte{0, 1, 2, 3, 4, 5}},
		{Name: fdbBase + "2." + macSuffix, Type: gsnmp.Integer, Value: 1}, // port=1 → must be accepted
		{Name: fdbBase + "3." + macSuffix, Type: gsnmp.Integer, Value: fdbStatusLearned},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	if _, err := walkFdbTableInto(context.Background(), client, entries); err != nil {
		t.Fatalf("walkFdbTableInto: %v", err)
	}
	e, ok := entries[macSuffix]
	if !ok {
		t.Fatalf("expected entry for key %q", macSuffix)
	}
	if e.port != 1 {
		t.Errorf("port = %d, want 1", e.port)
	}

	bridgePorts := map[int]int{1: 2}
	ifNames := map[int]string{2: "Gi0/1"}
	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge for port=1 entry, got %d: %v", len(edges), edges)
	}
}

// walkQBridgeFdbTable: a PDU reporting port=0 must not set e.port; the entry's
// port field stays 0 and buildEdges produces no edge.
func TestWalkQBridgeFdbTablePortZeroRejected(t *testing.T) {
	// Q-BRIDGE index: fdbId=1, MAC=00:01:02:03:04:05
	qSuffix := "1.0.1.2.3.4.5"
	macKey := "0.1.2.3.4.5"
	qBridgeBase := ".1.3.6.1.2.1.17.7.1.2.2.1."
	pdus := []gsnmp.SnmpPDU{
		{Name: qBridgeBase + "2." + qSuffix, Type: gsnmp.Integer, Value: 0}, // port=0 → must be rejected
		{Name: qBridgeBase + "3." + qSuffix, Type: gsnmp.Integer, Value: fdbStatusLearned},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	if err := walkQBridgeFdbTable(context.Background(), client, entries); err != nil {
		t.Fatalf("walkQBridgeFdbTable: %v", err)
	}
	e, ok := entries[macKey]
	if !ok {
		t.Fatalf("expected entry for key %q", macKey)
	}
	if e.port != 0 {
		t.Errorf("port = %d, want 0 (port=0 PDU must not update e.port)", e.port)
	}

	// buildEdges with no bridgePorts mapping for port 0 → no edges.
	bridgePorts := map[int]int{1: 2} // port 1 mapped; port 0 intentionally absent
	ifNames := map[int]string{2: "Gi0/1"}
	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for port=0 Q-BRIDGE entry, got %d: %v", len(edges), edges)
	}
}

// walkQBridgeFdbTable: a PDU reporting port=1 must set e.port to 1 and
// buildEdges must produce one direct edge (existing behaviour unchanged).
func TestWalkQBridgeFdbTablePortOneAccepted(t *testing.T) {
	// Q-BRIDGE index: fdbId=1, MAC=00:01:02:03:04:05
	qSuffix := "1.0.1.2.3.4.5"
	macKey := "0.1.2.3.4.5"
	qBridgeBase := ".1.3.6.1.2.1.17.7.1.2.2.1."
	pdus := []gsnmp.SnmpPDU{
		{Name: qBridgeBase + "2." + qSuffix, Type: gsnmp.Integer, Value: 1}, // port=1 → must be accepted
		{Name: qBridgeBase + "3." + qSuffix, Type: gsnmp.Integer, Value: fdbStatusLearned},
	}
	client := openClientToAgent(t, "public", pdus)
	entries := make(map[string]*fdbEntry)
	if err := walkQBridgeFdbTable(context.Background(), client, entries); err != nil {
		t.Fatalf("walkQBridgeFdbTable: %v", err)
	}
	e, ok := entries[macKey]
	if !ok {
		t.Fatalf("expected entry for key %q", macKey)
	}
	if e.port != 1 {
		t.Errorf("port = %d, want 1", e.port)
	}

	bridgePorts := map[int]int{1: 2}
	ifNames := map[int]string{2: "Gi0/1"}
	edges, _ := buildEdges(context.Background(), "sw", entries, bridgePorts, ifNames, nil)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge for port=1 Q-BRIDGE entry, got %d: %v", len(edges), edges)
	}
}

// buildQBridgeAgentPDUs builds PDUs with one B-MIB entry and one Q-BRIDGE entry
// on distinct bridge ports so that each port carries exactly one MAC
// (AdjacencyDirect) and both produce an edge.
//
// dot1dTpFdbTable: MAC 00:0a:bb:cc:dd:ee on bridge port 1 (learned)
// dot1qTpFdbTable: MAC 00:aa:bb:cc:dd:01 on bridge port 2, VLAN FDB ID 10 (learned)
// dot1dBasePortTable: bridge port 1 → ifIndex 2, bridge port 2 → ifIndex 3
// dot1dStpPortTable: bridge port 1 → forwarding(5), bridge port 2 → forwarding(5)
// ifXTable.ifName: ifIndex 2 → "GigabitEthernet0/1", ifIndex 3 → "GigabitEthernet0/2"
func buildQBridgeAgentPDUs() []gsnmp.SnmpPDU {
	fdbBase := ".1.3.6.1.2.1.17.4.3.1."
	qBridgeBase := ".1.3.6.1.2.1.17.7.1.2.2.1."
	basePortBase := ".1.3.6.1.2.1.17.1.4.1."
	stpPortBase := ".1.3.6.1.2.1.17.2.15.1."
	ifNameBase := ".1.3.6.1.2.1.31.1.1.1.1."

	bmibSuffix := "0.10.187.204.221.238"
	// Q-BRIDGE index: fdbId=10, MAC=00:aa:bb:cc:dd:01
	qSuffix := "10.0.170.187.204.221.1"

	return []gsnmp.SnmpPDU{
		// dot1dTpFdbTable: MAC 00:0a:bb:cc:dd:ee on bridge port 1
		{Name: fdbBase + "1." + bmibSuffix, Type: gsnmp.OctetString, Value: []byte{0, 10, 187, 204, 221, 238}},
		{Name: fdbBase + "2." + bmibSuffix, Type: gsnmp.Integer, Value: 1},
		{Name: fdbBase + "3." + bmibSuffix, Type: gsnmp.Integer, Value: fdbStatusLearned},

		// dot1qTpFdbTable: MAC 00:aa:bb:cc:dd:01 on bridge port 2, VLAN FDB ID 10
		{Name: qBridgeBase + "2." + qSuffix, Type: gsnmp.Integer, Value: 2},
		{Name: qBridgeBase + "3." + qSuffix, Type: gsnmp.Integer, Value: fdbStatusLearned},

		// dot1dBasePortTable: bridge port 1 → ifIndex 2, bridge port 2 → ifIndex 3
		{Name: basePortBase + "2.1", Type: gsnmp.Integer, Value: 2},
		{Name: basePortBase + "2.2", Type: gsnmp.Integer, Value: 3},

		// dot1dStpPortTable: bridge ports 1 and 2 → forwarding(5)
		{Name: stpPortBase + "3.1", Type: gsnmp.Integer, Value: stpStateForwarding},
		{Name: stpPortBase + "3.2", Type: gsnmp.Integer, Value: stpStateForwarding},

		// ifXTable.ifName: ifIndex 2 → "GigabitEthernet0/1", ifIndex 3 → "GigabitEthernet0/2"
		{Name: ifNameBase + "2", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/1")},
		{Name: ifNameBase + "3", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/2")},
	}
}

// buildFdbAgentPDUs builds the minimal PDU set for one learned FDB entry.
//
// dot1dTpFdbTable (1.3.6.1.2.1.17.4.3.1.{col}.{mac_octets}):
//
//	col 1 (address) = 00:0a:bb:cc:dd:ee  → OID suffix 0.10.187.204.221.238
//	col 2 (port)    = 1 (bridge port 1)
//	col 3 (status)  = 3 (learned)
//
// dot1dBasePortTable (1.3.6.1.2.1.17.1.4.1.2.{portNum}):
//
//	port 1 → ifIndex 2
//
// dot1dStpPortTable (1.3.6.1.2.1.17.2.15.1.3.{portNum}):
//
//	port 1 → forwarding(5)
//
// ifXTable.ifName (1.3.6.1.2.1.31.1.1.1.1.{ifIndex}):
//
//	ifIndex 2 → "GigabitEthernet0/1"
func buildFdbAgentPDUs() []gsnmp.SnmpPDU {
	macSuffix := "0.10.187.204.221.238"
	fdbBase := ".1.3.6.1.2.1.17.4.3.1."
	basePortBase := ".1.3.6.1.2.1.17.1.4.1."
	stpPortBase := ".1.3.6.1.2.1.17.2.15.1."
	ifNameBase := ".1.3.6.1.2.1.31.1.1.1.1."

	return []gsnmp.SnmpPDU{
		// dot1dTpFdbTable
		{Name: fdbBase + "1." + macSuffix, Type: gsnmp.OctetString, Value: []byte{0, 10, 187, 204, 221, 238}},
		{Name: fdbBase + "2." + macSuffix, Type: gsnmp.Integer, Value: 1},
		{Name: fdbBase + "3." + macSuffix, Type: gsnmp.Integer, Value: fdbStatusLearned},

		// dot1dBasePortTable: bridge port 1 → ifIndex 2
		{Name: basePortBase + "2.1", Type: gsnmp.Integer, Value: 2},

		// dot1dStpPortTable: bridge port 1 → forwarding(5)
		{Name: stpPortBase + "3.1", Type: gsnmp.Integer, Value: stpStateForwarding},

		// ifXTable.ifName: ifIndex 2 → "GigabitEthernet0/1"
		{Name: ifNameBase + "2", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/1")},
	}
}

// ---------- decode-issue reporter tests (issue #99) ----------

// walkBasePortTable: a bridge port index of 0 (invalid per RFC 4188) reports a
// decode issue to the reporter installed on ctx with reason
// bridge_port_index_invalid and the dot1dBasePortTable root OID.
func TestWalkBasePortTableReportsInvalidIndex(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		// port 0 → ifIndex 99: invalid RFC 4188 index → rejected.
		{Name: ".1.3.6.1.2.1.17.1.4.1.2.0", Type: gsnmp.Integer, Value: 99},
	}
	client := openClientToAgent(t, "public", pdus)

	var issues []snmputil.DecodeIssue
	ctx := snmputil.ContextWithDecodeIssueReporter(context.Background(), func(i snmputil.DecodeIssue) {
		issues = append(issues, i)
	})

	if _, err := walkBasePortTable(ctx, client); err != nil {
		t.Fatalf("walkBasePortTable: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 decode issue, got %d: %v", len(issues), issues)
	}
	if issues[0].Module != walkerFDB || issues[0].Reason != "bridge_port_index_invalid" {
		t.Errorf("issue = {%q,%q}, want {fdb,bridge_port_index_invalid}", issues[0].Module, issues[0].Reason)
	}
	if string(issues[0].OID) != oidBasePortTable {
		t.Errorf("issue OID = %q, want %q", issues[0].OID, oidBasePortTable)
	}
}

// buildEdges: an FDB entry on a bridge port with no ifIndex mapping reports a
// decode issue to the reporter installed on ctx with reason ifindex_unmapped
// and the dot1dBasePortTable root OID.
func TestBuildEdgesReportsIfIndexUnmapped(t *testing.T) {
	var issues []snmputil.DecodeIssue
	ctx := snmputil.ContextWithDecodeIssueReporter(context.Background(), func(i snmputil.DecodeIssue) {
		issues = append(issues, i)
	})

	entries := map[string]*fdbEntry{
		"0.1.2.3.4.5": {mac: []byte{0, 1, 2, 3, 4, 5}, port: 7, status: fdbStatusLearned},
	}
	// bridgePorts has no mapping for port 7 → ifindex_unmapped.
	edges, _ := buildEdges(ctx, "sw", entries, map[int]int{}, map[int]string{}, nil)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges, got %d", len(edges))
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 decode issue, got %d: %v", len(issues), issues)
	}
	if issues[0].Module != walkerFDB || issues[0].Reason != "ifindex_unmapped" {
		t.Errorf("issue = {%q,%q}, want {fdb,ifindex_unmapped}", issues[0].Module, issues[0].Reason)
	}
	if string(issues[0].OID) != oidBasePortTable {
		t.Errorf("issue OID = %q, want %q", issues[0].OID, oidBasePortTable)
	}
}
