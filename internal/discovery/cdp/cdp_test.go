package cdp

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	gsnmp "github.com/gosnmp/gosnmp"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	snmputil "github.com/grafana/network-topology-exporter/internal/discovery/snmp"
	"github.com/grafana/network-topology-exporter/internal/snmptest"
)

// buildEdges: in-scope neighbor with IPv4 address produces an edge.
func TestBuildEdgesInScope(t *testing.T) {
	_, allowed, _ := net.ParseCIDR("10.0.0.0/8")

	ifNames := map[int]string{3: "GigabitEthernet0/3"}
	entries := map[cacheKey]*cacheEntry{
		{3, 1}: {
			addrType: 1,
			addr:     []byte{10, 1, 2, 3},
			deviceID: "peer-sw",
			devPort:  "Gi0/1",
		},
	}

	edges, oos, _ := buildEdges(context.Background(), "local-sw", ifNames, entries, []*net.IPNet{allowed})
	if len(oos) != 0 {
		t.Errorf("unexpected out-of-scope: %v", oos)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.SrcDevice != "local-sw" || e.SrcPort != "GigabitEthernet0/3" {
		t.Errorf("src = %q/%q, want local-sw/GigabitEthernet0/3", e.SrcDevice, e.SrcPort)
	}
	if e.DstDevice != "peer-sw" || e.DstPort != "Gi0/1" {
		t.Errorf("dst = %q/%q, want peer-sw/Gi0/1", e.DstDevice, e.DstPort)
	}
	if e.DiscoveryProto != "cdp" {
		t.Errorf("proto = %q, want cdp", e.DiscoveryProto)
	}
	if e.Direction != discovery.DirectionUnidirectional {
		t.Errorf("direction = %q, want unidirectional", e.Direction)
	}
	if e.PrecedenceRank != precedenceRank {
		t.Errorf("rank = %d, want %d", e.PrecedenceRank, precedenceRank)
	}
}

// TestBuildEdgesIPv6Neighbor verifies that a CDP cache entry with addrType=20
// and a valid 16-byte IPv6 address produces an edge when the address falls
// within the allowed networks.
func TestBuildEdgesIPv6Neighbor(t *testing.T) {
	t.Parallel()

	ip6 := net.ParseIP("2001:db8::1").To16()
	_, allowed, _ := net.ParseCIDR("2001:db8::/32")

	ifNames := map[int]string{5: "GigabitEthernet0/5"}
	entries := map[cacheKey]*cacheEntry{
		{5, 1}: {
			addrType: 20,
			addr:     ip6,
			deviceID: "ipv6-peer",
			devPort:  "Gi0/1",
		},
	}

	edges, oos, _ := buildEdges(context.Background(), "local-sw", ifNames, entries, []*net.IPNet{allowed})
	if len(oos) != 0 {
		t.Errorf("unexpected out-of-scope entries: %v", oos)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.SrcDevice != "local-sw" || e.SrcPort != "GigabitEthernet0/5" {
		t.Errorf("src = %q/%q, want local-sw/GigabitEthernet0/5", e.SrcDevice, e.SrcPort)
	}
	if e.DstDevice != "ipv6-peer" || e.DstPort != "Gi0/1" {
		t.Errorf("dst = %q/%q, want ipv6-peer/Gi0/1", e.DstDevice, e.DstPort)
	}
	if e.DiscoveryProto != "cdp" {
		t.Errorf("proto = %q, want cdp", e.DiscoveryProto)
	}
}

// buildEdges: out-of-scope neighbor is recorded in oos and skipped as an edge.
func TestBuildEdgesOutOfScope(t *testing.T) {
	_, allowed, _ := net.ParseCIDR("10.0.0.0/8")

	ifNames := map[int]string{1: "Gi0/1"}
	entries := map[cacheKey]*cacheEntry{
		{1, 1}: {
			addrType: 1,
			addr:     []byte{192, 168, 1, 1},
			deviceID: "remote-sw",
			devPort:  "Gi0/2",
		},
	}

	edges, oos, _ := buildEdges(context.Background(), "local-sw", ifNames, entries, []*net.IPNet{allowed})
	if len(edges) != 0 {
		t.Errorf("expected no edges for out-of-scope, got %d", len(edges))
	}
	if len(oos) != 1 {
		t.Fatalf("expected 1 oos entry, got %d", len(oos))
	}
	if oos[0].NeighbourHint != "remote-sw" {
		t.Errorf("NeighbourHint = %q, want remote-sw", oos[0].NeighbourHint)
	}
}

// buildEdges: empty allowedNets means no scope filtering.
func TestBuildEdgesNoFilter(t *testing.T) {
	ifNames := map[int]string{1: "eth0"}
	entries := map[cacheKey]*cacheEntry{
		{1, 1}: {
			addrType: 1,
			addr:     []byte{1, 2, 3, 4},
			deviceID: "far-sw",
			devPort:  "eth1",
		},
	}

	edges, oos, _ := buildEdges(context.Background(), "me", ifNames, entries, nil)
	if len(oos) != 0 {
		t.Errorf("unexpected oos with nil allowedNets")
	}
	if len(edges) != 1 {
		t.Errorf("expected 1 edge, got %d", len(edges))
	}
}

// buildEdges: entries without deviceID or devPort are skipped.
func TestBuildEdgesSkipsIncomplete(t *testing.T) {
	ifNames := map[int]string{1: "eth0"}
	entries := map[cacheKey]*cacheEntry{
		{1, 1}: {deviceID: "", devPort: "Gi0/1"}, // no deviceID
		{1, 2}: {deviceID: "sw", devPort: ""},    // no devPort
	}

	edges, _, _ := buildEdges(context.Background(), "me", ifNames, entries, nil)
	if len(edges) != 0 {
		t.Errorf("expected no edges, got %d", len(edges))
	}
}

// buildEdges: unknown ifIndex falls back to "if{ifIndex}".
func TestBuildEdgesFallbackIfIndex(t *testing.T) {
	entries := map[cacheKey]*cacheEntry{
		{7, 1}: {deviceID: "peer", devPort: "eth0"},
	}

	edges, _, _ := buildEdges(context.Background(), "me", map[int]string{}, entries, nil)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].SrcPort != "if7" {
		t.Errorf("SrcPort = %q, want \"if7\"", edges[0].SrcPort)
	}
	if edges[0].SrcIfIndex != 7 {
		t.Errorf("SrcIfIndex = %d, want 7", edges[0].SrcIfIndex)
	}
}

// TestBuildEdgesIfNameFallback verifies that when ifNames is empty, the local
// port name is synthesised as "if{ifIndex}" rather than an empty string.
func TestBuildEdgesIfNameFallback(t *testing.T) {
	entries := map[cacheKey]*cacheEntry{
		{3, 1}: {deviceID: "peer-sw", devPort: "GigabitEthernet0/1"},
	}

	edges, _, _ := buildEdges(context.Background(), "local-sw", map[int]string{}, entries, nil)
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d: %v", len(edges), edges)
	}
	if edges[0].SrcPort != "if3" {
		t.Errorf("SrcPort = %q, want \"if3\"", edges[0].SrcPort)
	}
	if edges[0].SrcIfIndex != 3 {
		t.Errorf("SrcIfIndex = %d, want 3", edges[0].SrcIfIndex)
	}
}

// cdpNeighborIP: addrType 1 with 4-byte addr returns the IPv4 address.
func TestCdpNeighborIP(t *testing.T) {
	e := &cacheEntry{addrType: 1, addr: []byte{10, 0, 0, 1}}
	ip := cdpNeighborIP(e)
	if ip == nil || ip.String() != "10.0.0.1" {
		t.Errorf("got %v, want 10.0.0.1", ip)
	}
}

// cdpNeighborIP: wrong addrType returns nil.
func TestCdpNeighborIPWrongType(t *testing.T) {
	e := &cacheEntry{addrType: 2, addr: []byte{10, 0, 0, 1}}
	if ip := cdpNeighborIP(e); ip != nil {
		t.Errorf("expected nil for addrType=2, got %v", ip)
	}
}

// cdpNeighborIP: addrType 1 but wrong length returns nil.
func TestCdpNeighborIPWrongLen(t *testing.T) {
	e := &cacheEntry{addrType: 1, addr: []byte{10, 0, 0}}
	if ip := cdpNeighborIP(e); ip != nil {
		t.Errorf("expected nil for 3-byte addr, got %v", ip)
	}
}

// Walk: end-to-end test with a fake SNMP agent serving ifName and cdpCacheTable.
func TestWalkEndToEnd(t *testing.T) {
	pdus := buildCDPAgentPDUs()
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}

	edges, oos, err := Walk(context.Background(), p, "local-sw", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(oos) != 0 {
		t.Errorf("unexpected oos: %v", oos)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d: %v", len(edges), edges)
	}
	e := edges[0]
	if e.SrcDevice != "local-sw" {
		t.Errorf("SrcDevice = %q, want local-sw", e.SrcDevice)
	}
	if e.SrcPort != "GigabitEthernet0/1" {
		t.Errorf("SrcPort = %q, want GigabitEthernet0/1", e.SrcPort)
	}
	if e.DstDevice != "remote-sw" {
		t.Errorf("DstDevice = %q, want remote-sw", e.DstDevice)
	}
	if e.DstPort != "GigabitEthernet0/2" {
		t.Errorf("DstPort = %q, want GigabitEthernet0/2", e.DstPort)
	}
}

// Walk: an oversized cdpCacheDevicePort is truncated at discovery time so that
// a spoke push of the resulting Edge.DstPort survives the hub's 256-byte cap
// (issue #13). Truncation must produce valid UTF-8.
func TestWalkOversizedDevicePortTruncated(t *testing.T) {
	ifNameOID := ".1.3.6.1.2.1.31.1.1.1.1.1"
	base := ".1.3.6.1.4.1.9.9.23.1.2.1.1."
	idx := "1.1"

	// 254 ASCII bytes + 3-byte UTF-8 rune = 257 bytes. Naive byte-slice at 255
	// would land mid-rune; SanitisePortName retreats to a rune boundary.
	huge := strings.Repeat("a", 254) + "€"

	pdus := []gsnmp.SnmpPDU{
		{Name: ifNameOID, Type: gsnmp.OctetString, Value: []byte("Gi0/1")},
		{Name: base + strconv.Itoa(colAddressType) + "." + idx, Type: gsnmp.Integer, Value: int(1)},
		{Name: base + strconv.Itoa(colAddress) + "." + idx, Type: gsnmp.OctetString, Value: []byte{10, 0, 0, 2}},
		{Name: base + strconv.Itoa(colDeviceID) + "." + idx, Type: gsnmp.OctetString, Value: []byte("remote-sw")},
		{Name: base + strconv.Itoa(colDevicePort) + "." + idx, Type: gsnmp.OctetString, Value: []byte(huge)},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "local-sw", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if got := len(edges[0].DstPort); got > 255 {
		t.Errorf("DstPort len = %d, want ≤255 (hub would reject this)", got)
	}
	if !utf8.ValidString(edges[0].DstPort) {
		t.Errorf("DstPort is not valid UTF-8 — slice landed mid-rune: %q", edges[0].DstPort)
	}
}

// buildCDPAgentPDUs builds the PDUs a fake agent needs to support one CDP neighbor.
//
// ifName: ifIndex 1 → "GigabitEthernet0/1"
// cdpCacheTable: ifIndex=1, neighIndex=1
//
//	col 3 (addrType) = 1 (IPv4)
//	col 4 (addr)     = 10.0.0.2
//	col 6 (deviceId) = "remote-sw"
//	col 7 (devPort)  = "GigabitEthernet0/2"
func buildCDPAgentPDUs() []gsnmp.SnmpPDU {
	ifNameOID := ".1.3.6.1.2.1.31.1.1.1.1.1"
	// cdpCacheTable: 1.3.6.1.4.1.9.9.23.1.2.1.1.{col}.{ifIndex}.{neighIndex}
	base := ".1.3.6.1.4.1.9.9.23.1.2.1.1."
	idx := "1.1"

	return []gsnmp.SnmpPDU{
		{Name: ifNameOID, Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/1")},
		{Name: base + strconv.Itoa(colAddressType) + "." + idx, Type: gsnmp.Integer, Value: int(1)},
		{Name: base + strconv.Itoa(colAddress) + "." + idx, Type: gsnmp.OctetString, Value: []byte{10, 0, 0, 2}},
		{Name: base + strconv.Itoa(colDeviceID) + "." + idx, Type: gsnmp.OctetString, Value: []byte("remote-sw")},
		{Name: base + strconv.Itoa(colDevicePort) + "." + idx, Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/2")},
	}
}

// Walk: Open fails when the connection cannot be established.
func TestWalkOpenFails(t *testing.T) {
	// Nanosecond timeout forces Connect to fail immediately.
	p := snmputil.Params{
		IP:        net.ParseIP("127.0.0.1"),
		Port:      161,
		Community: []byte("public"),
		Timeout:   time.Nanosecond,
	}
	_, _, err := Walk(context.Background(), p, "local-sw", nil)
	if err == nil {
		t.Fatal("expected error when Open fails, got nil")
	}
}

// Walk: WalkIfNames fails when the context is already cancelled.
func TestWalkWalkIfNamesFails(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.31.1.1.1.1.1", Type: gsnmp.OctetString, Value: []byte("eth0")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so WalkIfNames' BulkWalk fails immediately

	_, _, err := Walk(ctx, p, "local-sw", nil)
	if err == nil {
		t.Fatal("expected error when WalkIfNames fails, got nil")
	}
}

// startCDPSilentAgent starts an SNMP agent that serves ifName OIDs normally but
// silently drops all requests for the CDP cache table OID subtree. This forces
// the CDP-specific BulkWalk to time out, exercising the walkCacheTable error path.
func startCDPSilentAgent(t *testing.T) string {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startCDPSilentAgent: listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ifNamePDUs := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.31.1.1.1.1.1", Type: gsnmp.OctetString, Value: []byte("eth0")},
	}
	sort.Slice(ifNamePDUs, func(i, j int) bool {
		return ifNamePDUs[i].Name < ifNamePDUs[j].Name
	})

	decoder := &gsnmp.GoSNMP{Version: gsnmp.Version2c, Community: "public"}

	go func() {
		buf := make([]byte, 65535)
		for {
			n, src, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			pkt, err := decoder.SnmpDecodePacket(buf[:n])
			if err != nil {
				continue
			}
			if pkt.Community != "public" {
				continue
			}
			// Drop CDP OID requests silently (prefix 1.3.6.1.4.1.9 = Cisco enterprise).
			for _, v := range pkt.Variables {
				if len(v.Name) >= 10 && v.Name[1:10] == "1.3.6.1.4" {
					return // stop the goroutine; subsequent reads close via conn.Close
				}
			}
			// Serve ifName responses.
			resp := agentHandleBulk(ifNamePDUs, pkt)
			if resp == nil {
				continue
			}
			reply := &gsnmp.SnmpPacket{
				Version:   gsnmp.Version2c,
				Community: "public",
				PDUType:   gsnmp.GetResponse,
				RequestID: pkt.RequestID,
				Variables: resp,
			}
			raw, err := reply.MarshalMsg()
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(raw, src)
		}
	}()

	return conn.LocalAddr().String()
}

// agentHandleBulk responds to GetBulkRequest and GetNextRequest PDUs using the
// given sorted PDU list. It mirrors the snmptest agent's handleBulk logic.
func agentHandleBulk(pdus []gsnmp.SnmpPDU, pkt *gsnmp.SnmpPacket) []gsnmp.SnmpPDU {
	if pkt.PDUType != gsnmp.GetBulkRequest && pkt.PDUType != gsnmp.GetNextRequest {
		return nil
	}
	maxReps := int(pkt.MaxRepetitions)
	if maxReps == 0 {
		maxReps = 50
	}
	var resp []gsnmp.SnmpPDU
	for _, v := range pkt.Variables {
		cur := v.Name
		for i := 0; i < maxReps; i++ {
			found := false
			for _, p := range pdus {
				if p.Name > cur {
					resp = append(resp, p)
					cur = p.Name
					found = true
					break
				}
			}
			if !found {
				resp = append(resp, gsnmp.SnmpPDU{Name: cur, Type: gsnmp.EndOfMibView})
				break
			}
		}
	}
	return resp
}

// Walk: walkCacheTable fails when the SNMP agent stops responding to CDP OID
// requests; the error propagates through Walk's third error return.
// This also covers walkCacheTable's internal BulkWalk-error return.
func TestWalkCacheTableFails(t *testing.T) {
	addr := startCDPSilentAgent(t)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   50 * time.Millisecond, // short so the CDP walk times out quickly
	}

	_, _, err := Walk(context.Background(), p, "local-sw", nil)
	if err == nil {
		t.Fatal("expected error when walkCacheTable fails, got nil")
	}
}

// walkCacheTable: a cache entry with ifIdx=0 in the OID suffix is silently
// skipped and produces no edge. Covers the ifIdx <= 0 guard.
func TestWalkCacheTableSkipsZeroIfIdx(t *testing.T) {
	// OID format: prefix + col.ifIndex.neighIndex
	// Using col=6 (deviceID), ifIndex=0, neighIndex=1.
	const zeroIfIdxOID = ".1.3.6.1.4.1.9.9.23.1.2.1.1.6.0.1"
	pdus := []gsnmp.SnmpPDU{
		{Name: zeroIfIdxOID, Type: gsnmp.OctetString, Value: []byte("peer-sw")},
	}

	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := snmputil.Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	entries, _, err := walkCacheTable(context.Background(), client)
	if err != nil {
		t.Fatalf("walkCacheTable: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for ifIdx=0, got %d", len(entries))
	}
}

// walkCacheTable: PDUs with too-short or wrong-prefix OID suffixes are silently
// skipped. Covers the three skip-continue paths inside the parse loop.
func TestWalkCacheTableSkipsShortOIDs(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		// Continue A: within the CDP table BulkWalk root (.1.3.6.1.4.1.9.9.23.1.2.1)
		// but the OID has ".2." not ".1." for the row-entry subtree — TrimOIDPrefix fails.
		{Name: ".1.3.6.1.4.1.9.9.23.1.2.1.2.3.1.1", Type: gsnmp.Integer, Value: int(1)},
		// Continue C: suffix = "3" (col only, no ifIndex component) —
		// second SplitOIDComponent("")  returns !ok.
		{Name: ".1.3.6.1.4.1.9.9.23.1.2.1.1.3", Type: gsnmp.Integer, Value: int(1)},
		// Continue D: suffix = "3.1" (col + ifIndex, no neighIndex) —
		// Atoi("") on the empty neighStr fails.
		{Name: ".1.3.6.1.4.1.9.9.23.1.2.1.1.3.1", Type: gsnmp.Integer, Value: int(1)},
		// Valid PDU to confirm normal processing still works.
		{Name: ".1.3.6.1.4.1.9.9.23.1.2.1.1.6.1.1", Type: gsnmp.OctetString, Value: []byte("device-a")},
	}

	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := snmputil.Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	entries, _, err := walkCacheTable(context.Background(), client)
	if err != nil {
		t.Fatalf("walkCacheTable: %v", err)
	}
	// Only the valid PDU should produce an entry (col 6 = deviceID; no addr/port PDUs
	// provided, so the entry exists but is incomplete).
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}
}

// ---------- decode-issue reporter tests (issue #99) ----------

// buildEdges: a cache row with an empty deviceID reports a decode issue to the
// reporter installed on ctx with reason empty_device_id and the cdpCacheTable
// root OID.
func TestBuildEdgesReportsEmptyDeviceID(t *testing.T) {
	var issues []snmputil.DecodeIssue
	ctx := snmputil.ContextWithDecodeIssueReporter(context.Background(), func(i snmputil.DecodeIssue) {
		issues = append(issues, i)
	})

	entries := map[cacheKey]*cacheEntry{
		{1, 1}: {deviceID: "", devPort: "Gi0/1"}, // empty deviceID → rejected
	}

	edges, _, _ := buildEdges(ctx, "me", map[int]string{1: "eth0"}, entries, nil)
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges, got %d", len(edges))
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 decode issue, got %d: %v", len(issues), issues)
	}
	if issues[0].Module != walkerCDP || issues[0].Reason != "empty_device_id" {
		t.Errorf("issue = {%q,%q}, want {cdp,empty_device_id}", issues[0].Module, issues[0].Reason)
	}
	if string(issues[0].OID) != oidCDPCacheTable {
		t.Errorf("issue OID = %q, want %q", issues[0].OID, oidCDPCacheTable)
	}
}

// walkCacheTable: a PDU whose OID index carries ifIndex=0 reports a decode
// issue with reason index_unparseable and the cdpCacheTable root OID.
func TestWalkCacheTableReportsIndexUnparseable(t *testing.T) {
	const zeroIfIdxOID = ".1.3.6.1.4.1.9.9.23.1.2.1.1.6.0.1"
	pdus := []gsnmp.SnmpPDU{
		{Name: zeroIfIdxOID, Type: gsnmp.OctetString, Value: []byte("peer-sw")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := snmputil.Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	var issues []snmputil.DecodeIssue
	ctx := snmputil.ContextWithDecodeIssueReporter(context.Background(), func(i snmputil.DecodeIssue) {
		issues = append(issues, i)
	})

	if _, _, err := walkCacheTable(ctx, client); err != nil {
		t.Fatalf("walkCacheTable: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 decode issue, got %d: %v", len(issues), issues)
	}
	if issues[0].Module != walkerCDP || issues[0].Reason != "index_unparseable" {
		t.Errorf("issue = {%q,%q}, want {cdp,index_unparseable}", issues[0].Module, issues[0].Reason)
	}
	if string(issues[0].OID) != oidCDPCacheTable {
		t.Errorf("issue OID = %q, want %q", issues[0].OID, oidCDPCacheTable)
	}
}
