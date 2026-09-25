package lldp

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	gsnmp "github.com/gosnmp/gosnmp"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	snmputil "github.com/grafana/network-topology-exporter/internal/discovery/snmp"
	"github.com/grafana/network-topology-exporter/internal/snmptest"
)

// decodePortID: MAC address subtype produces colon-hex notation.
func TestDecodePortIDMAC(t *testing.T) {
	raw := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	got := decodePortID(portSubtypeMACAddress, raw)
	if got != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("got %q, want aa:bb:cc:dd:ee:ff", got)
	}
}

// decodePortID: interface name subtype is a raw string, no transformation.
func TestDecodePortIDInterfaceName(t *testing.T) {
	raw := []byte("GigabitEthernet0/1")
	got := decodePortID(portSubtypeInterfaceName, raw)
	if got != "GigabitEthernet0/1" {
		t.Errorf("got %q, want GigabitEthernet0/1", got)
	}
}

// decodePortID: networkAddress subtype with IPv4 (family byte 1, then 4 octets).
func TestDecodePortIDNetworkAddressIPv4(t *testing.T) {
	raw := []byte{1, 192, 168, 1, 1}
	got := decodePortID(portSubtypeNetworkAddress, raw)
	if got != "192.168.1.1" {
		t.Errorf("got %q, want 192.168.1.1", got)
	}
}

// decodePortID: null bytes at end of string are trimmed.
func TestDecodePortIDNullTrim(t *testing.T) {
	raw := []byte{'e', 't', 'h', '0', 0, 0}
	got := decodePortID(portSubtypeLocal, raw)
	if got != "eth0" {
		t.Errorf("got %q, want eth0", got)
	}
}

// decodeChassisID: MAC chassis (most common) produces colon-hex notation.
func TestDecodeChassisIDMAC(t *testing.T) {
	raw := []byte{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e}
	got := decodeChassisID(chassisSubtypeMACAddress, raw)
	if got != "00:1a:2b:3c:4d:5e" {
		t.Errorf("got %q, want 00:1a:2b:3c:4d:5e", got)
	}
}

// extractChassisIP: non-networkAddress subtype returns nil.
func TestExtractChassisIPWrongSubtype(t *testing.T) {
	if ip := extractChassisIP(chassisSubtypeMACAddress, []byte{0, 0, 0, 0, 0, 0}); ip != nil {
		t.Errorf("expected nil for MAC subtype, got %v", ip)
	}
}

// extractChassisIP: networkAddress IPv4 returns parsed IP.
func TestExtractChassisIPv4(t *testing.T) {
	raw := []byte{1, 10, 0, 0, 1}
	ip := extractChassisIP(chassisSubtypeNetworkAddress, raw)
	if ip == nil || ip.String() != "10.0.0.1" {
		t.Errorf("got %v, want 10.0.0.1", ip)
	}
}

// buildEdges: LD-11 out-of-scope neighbor (network-address chassis outside allow-list).
func TestBuildEdgesOutOfScope(t *testing.T) {
	_, allowed, _ := net.ParseCIDR("10.0.0.0/24")

	locPorts := map[int]locPort{
		1: {idSubtype: portSubtypeInterfaceName, id: []byte("Gi0/1")},
	}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeNetworkAddress,
			chassisID:      []byte{1, 192, 168, 100, 1}, // 192.168.100.1 — outside 10.0.0.0/24
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Gi0/2"),
			sysName:        "remote-sw",
		},
	}

	edges, oos, _, err := buildEdges(context.Background(), "local-sw", locPorts, remEntries, []*net.IPNet{allowed})
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected no edges for out-of-scope neighbor, got %d", len(edges))
	}
	if len(oos) != 1 {
		t.Fatalf("expected 1 out-of-scope entry, got %d", len(oos))
	}
	if oos[0].NeighbourHint != "remote-sw" {
		t.Errorf("NeighbourHint = %q, want remote-sw", oos[0].NeighbourHint)
	}
}

// buildEdges: MAC chassis-ID neighbor is skipped when allowedNets is non-empty
// (cannot validate scope without an IP — mirrors CDP behaviour).
func TestBuildEdgesMACChassisSkippedUnderScopeFilter(t *testing.T) {
	_, allowed, _ := net.ParseCIDR("10.0.0.0/24")

	locPorts := map[int]locPort{
		1: {idSubtype: portSubtypeInterfaceName, id: []byte("Gi0/1")},
	}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Gi0/2"),
			sysName:        "mac-peer",
		},
	}

	edges, oos, _, err := buildEdges(context.Background(), "local-sw", locPorts, remEntries, []*net.IPNet{allowed})
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected no edges for MAC chassis ID under scope filter, got %d", len(edges))
	}
	// Non-IP neighbours are silently skipped, not recorded as OOS.
	if len(oos) != 0 {
		t.Errorf("expected no OOS entries for MAC chassis ID under scope filter, got %d", len(oos))
	}
}

// buildEdges: chassisSubtype=5 with IP 0.0.0.0 produces no edge and no OOS entry.
func TestBuildEdgesUnspecifiedChassisIP(t *testing.T) {
	locPorts := map[int]locPort{
		1: {idSubtype: portSubtypeInterfaceName, id: []byte("Gi0/1")},
	}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeNetworkAddress,
			chassisID:      []byte{1, 0, 0, 0, 0}, // IANA family 1 (IPv4) + 0.0.0.0
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Gi0/2"),
			sysName:        "unaddressed-peer",
		},
	}

	edges, oos, _, err := buildEdges(context.Background(), "local-sw", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected no edges for unspecified chassis IP, got %d", len(edges))
	}
	if len(oos) != 0 {
		t.Errorf("expected no OOS entries for unspecified chassis IP, got %d", len(oos))
	}
}

// buildEdges: in-scope neighbor produces an edge with correct fields.
func TestBuildEdgesNormal(t *testing.T) {
	locPorts := map[int]locPort{
		2: {idSubtype: portSubtypeInterfaceName, id: []byte("Ethernet1")},
	}
	remEntries := map[remKey]*remEntry{
		{2, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Ethernet2"),
			sysName:        "spine-01",
		},
	}

	// No scope filter: MAC chassis ID neighbours are allowed through unconditionally.
	edges, oos, _, err := buildEdges(context.Background(), "leaf-01", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(oos) != 0 {
		t.Errorf("unexpected out-of-scope entries: %v", oos)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.SrcDevice != "leaf-01" || e.SrcPort != "Ethernet1" {
		t.Errorf("src = %q/%q, want leaf-01/Ethernet1", e.SrcDevice, e.SrcPort)
	}
	if e.DstDevice != "spine-01" || e.DstPort != "Ethernet2" {
		t.Errorf("dst = %q/%q, want spine-01/Ethernet2", e.DstDevice, e.DstPort)
	}
	if e.DiscoveryProto != "lldp" {
		t.Errorf("proto = %q, want lldp", e.DiscoveryProto)
	}
	if e.Direction != discovery.DirectionUnidirectional {
		t.Errorf("direction = %q, want unidirectional", e.Direction)
	}
	if e.PrecedenceRank != precedenceRank {
		t.Errorf("rank = %d, want %d", e.PrecedenceRank, precedenceRank)
	}
}

// decodeChassisID: networkAddress subtype decodes to IP string.
func TestDecodeChassisIDNetworkAddress(t *testing.T) {
	raw := []byte{1, 10, 0, 0, 1} // IANA family 1 (IPv4) + 4 octets
	got := decodeChassisID(chassisSubtypeNetworkAddress, raw)
	if got != "10.0.0.1" {
		t.Errorf("got %q, want 10.0.0.1", got)
	}
}

// decodeChassisID: unknown subtype falls back to the raw string.
func TestDecodeChassisIDUnknown(t *testing.T) {
	raw := []byte("my-chassis")
	got := decodeChassisID(99, raw)
	if got != "my-chassis" {
		t.Errorf("got %q, want my-chassis", got)
	}
}

// fmtMAC: non-6-byte input falls back to hex encoding.
func TestFmtMACShort(t *testing.T) {
	got := fmtMAC([]byte{0xaa, 0xbb})
	if got != "aabb" {
		t.Errorf("got %q, want aabb", got)
	}
}

// TrimOIDPrefix: valid prefix strips correctly.
func TestTrimOIDPrefixOK(t *testing.T) {
	rest, ok := snmputil.TrimOIDPrefix(".1.2.3.4", ".1.2.")
	if !ok || rest != "3.4" {
		t.Errorf("TrimOIDPrefix = (%q, %v), want (3.4, true)", rest, ok)
	}
}

// TrimOIDPrefix: wrong prefix returns false.
func TestTrimOIDPrefixMismatch(t *testing.T) {
	_, ok := snmputil.TrimOIDPrefix(".1.2.3", ".1.9.")
	if ok {
		t.Error("expected false for mismatched prefix")
	}
}

// TrimOIDPrefix: exact prefix (no suffix) returns false.
func TestTrimOIDPrefixEmpty(t *testing.T) {
	_, ok := snmputil.TrimOIDPrefix(".1.2.", ".1.2.")
	if ok {
		t.Error("expected false for empty suffix after prefix")
	}
}

// buildEdges: entry with no portDesc falls back to portNum string.
func TestBuildEdgesFallbackPortNum(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{5, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    portSubtypeMACAddress,
			portID:         []byte{0, 1, 2, 3, 4, 6},
			portDesc:       "",
			sysName:        "peer",
		},
	}

	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	// port ID is a MAC address for portSubtypeMACAddress — should produce a non-empty port
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
}

// buildEdges: portID that decodes to "" (null-only bytes) falls back to portDesc.
func TestBuildEdgesUsesPortDesc(t *testing.T) {
	locPorts := map[int]locPort{
		1: {idSubtype: portSubtypeInterfaceName, id: []byte("eth0")},
	}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    portSubtypeLocal,
			portID:         []byte{0, 0}, // null bytes TrimRight → "" → use portDesc
			portDesc:       "GigabitEthernet0/1",
			sysName:        "peer",
		},
	}

	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].DstPort != "GigabitEthernet0/1" {
		t.Errorf("DstPort = %q, want GigabitEthernet0/1", edges[0].DstPort)
	}
	if edges[0].SrcIfIndex != 1 {
		t.Errorf("SrcIfIndex = %d, want 1 (local lldpLocPortNum)", edges[0].SrcIfIndex)
	}
	if edges[0].DstIfIndex != 0 {
		t.Errorf("DstIfIndex = %d, want 0 (LLDP never resolves the remote side's ifIndex)", edges[0].DstIfIndex)
	}
}

// buildEdges: entries with empty chassisID or portID are skipped.
func TestBuildEdgesSkipsEmpty(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {chassisID: []byte{}, portID: []byte("eth0"), sysName: "peer"},
		{1, 2}: {chassisID: []byte{1}, portID: []byte{}, sysName: "peer"},
	}

	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges, got %d", len(edges))
	}
}

// Walk: end-to-end with a fake agent serving lldpLocPortTable and lldpRemTable.
func TestWalkEndToEnd(t *testing.T) {
	pdus := buildLLDPAgentPDUs()
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}

	edges, oos, err := Walk(context.Background(), p, "leaf-01", nil)
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
	if e.SrcDevice != "leaf-01" {
		t.Errorf("SrcDevice = %q, want leaf-01", e.SrcDevice)
	}
	if e.SrcPort != "Ethernet1" {
		t.Errorf("SrcPort = %q, want Ethernet1", e.SrcPort)
	}
	if e.DstDevice != "spine-01" {
		t.Errorf("DstDevice = %q, want spine-01", e.DstDevice)
	}
	if e.DstPort != "Ethernet2" {
		t.Errorf("DstPort = %q, want Ethernet2", e.DstPort)
	}
}

// buildLLDPAgentPDUs builds the minimal PDU set for one local port and one remote entry.
//
// lldpLocPortTable (1.0.8802.1.1.2.1.3.7.1.{col}.{portNum}):
//
//	col 2 (idSubtype) = 5 (interfaceName)
//	col 3 (id)        = "Ethernet1"
//
// lldpRemTable (1.0.8802.1.1.2.1.4.1.1.{col}.{timeMark}.{portNum}.{remIndex}):
//
//	col 4 (chassisSubtype) = 4 (MAC)
//	col 5 (chassisID)      = 00:01:02:03:04:05
//	col 6 (portSubtype)    = 5 (interfaceName)
//	col 7 (portID)         = "Ethernet2"
//	col 9 (sysName)        = "spine-01"
func buildLLDPAgentPDUs() []gsnmp.SnmpPDU {
	locBase := ".1.0.8802.1.1.2.1.3.7.1."
	remBase := ".1.0.8802.1.1.2.1.4.1.1."
	portNum := "1"
	timeMark := "0"
	remIdx := "1"
	remSuffix := timeMark + "." + portNum + "." + remIdx

	return []gsnmp.SnmpPDU{
		// lldpLocPortTable
		{Name: locBase + "2." + portNum, Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: locBase + "3." + portNum, Type: gsnmp.OctetString, Value: []byte("Ethernet1")},

		// lldpRemTable
		{Name: remBase + "4." + remSuffix, Type: gsnmp.Integer, Value: int(chassisSubtypeMACAddress)},
		{Name: remBase + "5." + remSuffix, Type: gsnmp.OctetString, Value: []byte{0, 1, 2, 3, 4, 5}},
		{Name: remBase + "6." + remSuffix, Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: remBase + "7." + remSuffix, Type: gsnmp.OctetString, Value: []byte("Ethernet2")},
		{Name: remBase + "9." + remSuffix, Type: gsnmp.OctetString, Value: []byte("spine-01")},
	}
}

// ---------- Walk error path tests ----------

// Walk: Open fails (V3 with empty username) → Walk returns error.
func TestWalkOpenFails(t *testing.T) {
	p := snmputil.Params{
		IP:      net.ParseIP("127.0.0.1"),
		Port:    161,
		Timeout: time.Second,
		V3:      true,
		// Username intentionally empty
	}
	_, _, err := Walk(context.Background(), p, "local", nil)
	if err == nil {
		t.Fatal("expected error when Open fails, got nil")
	}
}

// Walk: pre-cancelled context makes walkLocPorts BulkWalk return immediately,
// exercising the "lldp locport" error return in Walk.
func TestWalkLocPortsFails(t *testing.T) {
	pdus := buildLLDPAgentPDUs()
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	_, _, err := Walk(ctx, p, "local", nil)
	if err == nil {
		t.Fatal("expected error when walkLocPorts fails, got nil")
	}
}

// lazyErrCtx returns nil from Err() for the first failAfter calls, then
// returns context.Canceled. Used to fail BulkWalk on the Nth call while
// letting earlier calls succeed.
type lazyErrCtx struct {
	context.Context
	calls     int
	failAfter int
}

func (c *lazyErrCtx) Err() error {
	c.calls++
	if c.calls > c.failAfter {
		return context.Canceled
	}
	return nil
}

// Walk: walkLocPorts succeeds but walkRemEntries BulkWalk fails (ctx cancelled
// after first BulkWalk call), exercising the "lldp remtable" error return.
func TestWalkRemEntriesFails(t *testing.T) {
	pdus := buildLLDPAgentPDUs()
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}

	// failAfter=1: first ctx.Err() call (walkLocPorts BulkWalk entry) returns nil;
	// second call (walkRemEntries BulkWalk entry) returns Canceled.
	ctx := &lazyErrCtx{Context: context.Background(), failAfter: 1}

	_, _, err := Walk(ctx, p, "local", nil)
	if err == nil {
		t.Fatal("expected error when walkRemEntries fails, got nil")
	}
}

// ---------- walkLocPorts skip path tests ----------

// walkLocPorts: PDU outside the strict locPortTable subtree
// (prefix check fails) is silently skipped.
// OID ".1.0.8802.1.1.2.1.3.7.2.1" starts with ".1.0.8802.1.1.2.1.3.7." (gosnmp
// includes it) but NOT ".1.0.8802.1.1.2.1.3.7.1." (our prefix — skipped).
func TestWalkLocPortsTrimOIDSkip(t *testing.T) {
	locBase := ".1.0.8802.1.1.2.1.3.7.1."
	pdus := []gsnmp.SnmpPDU{
		{Name: locBase + "2.1", Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: locBase + "3.1", Type: gsnmp.OctetString, Value: []byte("Eth1")},
		// Out-of-range PDU: passes gosnmp prefix ".1.0.8802.1.1.2.1.3.7." but
		// fails our prefix ".1.0.8802.1.1.2.1.3.7.1." → triggers TrimOIDPrefix !ok.
		{Name: ".1.0.8802.1.1.2.1.3.7.2.1", Type: gsnmp.OctetString, Value: []byte("skip")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	_, _, err := Walk(context.Background(), p, "local", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
}

// walkLocPorts: PDU with a multi-component suffix (col.portNum.extra) causes
// Atoi to fail on "portNum.extra" and the entry is silently skipped.
func TestWalkLocPortsAtoiSkip(t *testing.T) {
	locBase := ".1.0.8802.1.1.2.1.3.7.1."
	remBase := ".1.0.8802.1.1.2.1.4.1.1."
	timeMark := "0"
	portNum := "1"
	remIdx := "1"
	remSuffix := timeMark + "." + portNum + "." + remIdx

	pdus := []gsnmp.SnmpPDU{
		{Name: locBase + "2.1", Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: locBase + "3.1", Type: gsnmp.OctetString, Value: []byte("Eth1")},
		// col=4 (colLocPortDesc), portNum suffix="2.5" → Atoi("2.5") fails → skip.
		{Name: locBase + "4.2.5", Type: gsnmp.OctetString, Value: []byte("skip-this")},
		// Includes colLocPortDesc (col=4) for portNum=1 to cover that branch too.
		{Name: locBase + "4.1", Type: gsnmp.OctetString, Value: []byte("Eth1-desc")},

		// lldpRemTable entries for a complete Walk
		{Name: remBase + "4." + remSuffix, Type: gsnmp.Integer, Value: int(chassisSubtypeMACAddress)},
		{Name: remBase + "5." + remSuffix, Type: gsnmp.OctetString, Value: []byte{0, 1, 2, 3, 4, 5}},
		{Name: remBase + "6." + remSuffix, Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: remBase + "7." + remSuffix, Type: gsnmp.OctetString, Value: []byte("Eth2")},
		{Name: remBase + "9." + remSuffix, Type: gsnmp.OctetString, Value: []byte("spine-01")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "local", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// Should still produce an edge from the valid entries.
	if len(edges) == 0 {
		t.Error("expected at least one edge, got none")
	}
}

// ---------- walkRemEntries skip path tests ----------

// walkRemEntries: PDU outside the strict remTable subtree
// (prefix check fails) is silently skipped.
func TestWalkRemEntriesTrimOIDSkip(t *testing.T) {
	locBase := ".1.0.8802.1.1.2.1.3.7.1."
	remBase := ".1.0.8802.1.1.2.1.4.1.1."
	timeMark := "0"
	portNum := "1"
	remIdx := "1"
	remSuffix := timeMark + "." + portNum + "." + remIdx

	pdus := []gsnmp.SnmpPDU{
		{Name: locBase + "2.1", Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: locBase + "3.1", Type: gsnmp.OctetString, Value: []byte("Eth1")},

		{Name: remBase + "4." + remSuffix, Type: gsnmp.Integer, Value: int(chassisSubtypeMACAddress)},
		{Name: remBase + "5." + remSuffix, Type: gsnmp.OctetString, Value: []byte{0, 1, 2, 3, 4, 5}},
		{Name: remBase + "6." + remSuffix, Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: remBase + "7." + remSuffix, Type: gsnmp.OctetString, Value: []byte("Eth2")},
		{Name: remBase + "9." + remSuffix, Type: gsnmp.OctetString, Value: []byte("spine-01")},
		// Out-of-range: passes gosnmp prefix ".1.0.8802.1.1.2.1.4.1." but
		// fails our prefix ".1.0.8802.1.1.2.1.4.1.1." → TrimOIDPrefix !ok.
		{Name: ".1.0.8802.1.1.2.1.4.1.2.4.0.1.1", Type: gsnmp.OctetString, Value: []byte("skip")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "local", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) == 0 {
		t.Error("expected at least one edge, got none")
	}
}

// walkRemEntries: PDU with a multi-component remIndex (portNum.remIndex.extra)
// causes Atoi to fail on "remIndex.extra" and the entry is silently skipped.
func TestWalkRemEntriesAtoiSkip(t *testing.T) {
	locBase := ".1.0.8802.1.1.2.1.3.7.1."
	remBase := ".1.0.8802.1.1.2.1.4.1.1."
	timeMark := "0"
	portNum := "1"
	remIdx := "1"
	remSuffix := timeMark + "." + portNum + "." + remIdx

	pdus := []gsnmp.SnmpPDU{
		{Name: locBase + "2.1", Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: locBase + "3.1", Type: gsnmp.OctetString, Value: []byte("Eth1")},

		// Valid entry
		{Name: remBase + "4." + remSuffix, Type: gsnmp.Integer, Value: int(chassisSubtypeMACAddress)},
		{Name: remBase + "5." + remSuffix, Type: gsnmp.OctetString, Value: []byte{0, 1, 2, 3, 4, 5}},
		{Name: remBase + "6." + remSuffix, Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: remBase + "7." + remSuffix, Type: gsnmp.OctetString, Value: []byte("Eth2")},
		{Name: remBase + "9." + remSuffix, Type: gsnmp.OctetString, Value: []byte("spine-01")},
		// col=4, timeMark=0, portNum=1, remStr="2.5" → Atoi("2.5") fails → skip.
		{Name: remBase + "4.0.1.2.5", Type: gsnmp.Integer, Value: int(chassisSubtypeMACAddress)},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "local", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) == 0 {
		t.Error("expected at least one edge, got none")
	}
}

// walkRemEntries: colRemPortDesc (col=8) is populated.
func TestWalkRemEntriesPortDesc(t *testing.T) {
	locBase := ".1.0.8802.1.1.2.1.3.7.1."
	remBase := ".1.0.8802.1.1.2.1.4.1.1."
	timeMark := "0"
	portNum := "1"
	remIdx := "1"
	remSuffix := timeMark + "." + portNum + "." + remIdx

	pdus := []gsnmp.SnmpPDU{
		{Name: locBase + "2.1", Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: locBase + "3.1", Type: gsnmp.OctetString, Value: []byte("Eth1")},

		{Name: remBase + "4." + remSuffix, Type: gsnmp.Integer, Value: int(chassisSubtypeMACAddress)},
		{Name: remBase + "5." + remSuffix, Type: gsnmp.OctetString, Value: []byte{0, 1, 2, 3, 4, 5}},
		{Name: remBase + "6." + remSuffix, Type: gsnmp.Integer, Value: int(portSubtypeLocal)},
		// portID with all null bytes → decodePortID returns "" → falls back to portDesc
		{Name: remBase + "7." + remSuffix, Type: gsnmp.OctetString, Value: []byte{0, 0}},
		// col=8 (colRemPortDesc) — this branch is otherwise uncovered
		{Name: remBase + "8." + remSuffix, Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/1")},
		{Name: remBase + "9." + remSuffix, Type: gsnmp.OctetString, Value: []byte("spine-01")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "local", nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(edges) == 0 {
		t.Errorf("expected an edge using portDesc fallback, got none")
	}
	if len(edges) > 0 && edges[0].DstPort != "GigabitEthernet0/1" {
		t.Errorf("DstPort = %q, want GigabitEthernet0/1", edges[0].DstPort)
	}
}

// Walk: a remote portDesc exceeding 255 bytes is truncated at discovery time
// (issue #13). A spoke pushing the resulting Edge.DstPort to the hub would
// otherwise fail validateSpokePayload's 256-byte cap and lose the entire push.
// The truncation must produce valid UTF-8 (no mid-rune slice).
func TestWalkRemEntriesPortDescOversizedTruncated(t *testing.T) {
	locBase := ".1.0.8802.1.1.2.1.3.7.1."
	remBase := ".1.0.8802.1.1.2.1.4.1.1."
	timeMark := "0"
	portNum := "1"
	remIdx := "1"
	remSuffix := timeMark + "." + portNum + "." + remIdx

	// 254 ASCII bytes followed by a 3-byte UTF-8 rune. Naive byte-slicing at
	// 255 would land mid-rune and produce invalid UTF-8; SanitisePortName must
	// retreat to a rune boundary.
	huge := strings.Repeat("a", 254) + "€" // 257 bytes total

	pdus := []gsnmp.SnmpPDU{
		{Name: locBase + "2.1", Type: gsnmp.Integer, Value: int(portSubtypeInterfaceName)},
		{Name: locBase + "3.1", Type: gsnmp.OctetString, Value: []byte("Eth1")},

		{Name: remBase + "4." + remSuffix, Type: gsnmp.Integer, Value: int(chassisSubtypeMACAddress)},
		{Name: remBase + "5." + remSuffix, Type: gsnmp.OctetString, Value: []byte{0, 1, 2, 3, 4, 5}},
		{Name: remBase + "6." + remSuffix, Type: gsnmp.Integer, Value: int(portSubtypeLocal)},
		{Name: remBase + "7." + remSuffix, Type: gsnmp.OctetString, Value: []byte{0, 0}}, // null portID → falls back to portDesc
		{Name: remBase + "8." + remSuffix, Type: gsnmp.OctetString, Value: []byte(huge)},
		{Name: remBase + "9." + remSuffix, Type: gsnmp.OctetString, Value: []byte("spine-01")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := snmputil.Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	edges, _, err := Walk(context.Background(), p, "local", nil)
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

// ---------- buildEdges skip test ----------

// buildEdges: entry where both portID decodes to "" and portDesc is ""
// is skipped (empty remDevice or remPort check).
func TestBuildEdgesSkipEmptyRemPort(t *testing.T) {
	locPorts := map[int]locPort{
		1: {idSubtype: portSubtypeInterfaceName, id: []byte("eth0")},
	}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    portSubtypeLocal,
			portID:         []byte{0, 0}, // null bytes → decodePortID = "" → portDesc = "" → skip
			portDesc:       "",
			sysName:        "peer",
		},
	}
	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for empty remPort, got %d", len(edges))
	}
}

// ---------- resolveLocalPort tests ----------

// resolveLocalPort: portNum not found in locPorts map returns strconv.Itoa fallback.
func TestResolveLocalPortFallback(t *testing.T) {
	got := resolveLocalPort(42, map[int]locPort{})
	if got != "42" {
		t.Errorf("resolveLocalPort(42, {}) = %q, want 42", got)
	}
}

// resolveLocalPort: decodePortID returns "" and desc is non-empty → desc is returned.
func TestResolveLocalPortDescFallback(t *testing.T) {
	lp := locPort{
		idSubtype: portSubtypeLocal,
		id:        []byte{0, 0}, // null bytes → decodePortID returns ""
		desc:      "GigabitEthernet0/0",
	}
	got := resolveLocalPort(3, map[int]locPort{3: lp})
	if got != "GigabitEthernet0/0" {
		t.Errorf("resolveLocalPort with desc fallback = %q, want GigabitEthernet0/0", got)
	}
}

// resolveLocalPort: decodePortID returns "" and desc is also "" → portNum string.
func TestResolveLocalPortNumericFallback(t *testing.T) {
	lp := locPort{
		idSubtype: portSubtypeLocal,
		id:        []byte{0, 0}, // null bytes → ""
		desc:      "",
	}
	got := resolveLocalPort(7, map[int]locPort{7: lp})
	if got != "7" {
		t.Errorf("resolveLocalPort with numeric fallback = %q, want 7", got)
	}
}

// ---------- resolveRemDevice tests ----------

// resolveRemDevice: empty sysName falls back to decodeChassisID.
func TestResolveRemDeviceFallback(t *testing.T) {
	rem := &remEntry{
		chassisSubtype: chassisSubtypeMACAddress,
		chassisID:      []byte{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e},
		sysName:        "", // empty → use chassis ID
	}
	got := resolveRemDevice(rem)
	if got != "00:1a:2b:3c:4d:5e" {
		t.Errorf("resolveRemDevice fallback = %q, want 00:1a:2b:3c:4d:5e", got)
	}
}

// ---------- decodePortID tests ----------

// decodePortID: empty raw bytes returns "".
func TestDecodePortIDEmpty(t *testing.T) {
	got := decodePortID(portSubtypeInterfaceName, []byte{})
	if got != "" {
		t.Errorf("decodePortID empty = %q, want empty string", got)
	}
}

// decodePortID: portSubtypeLocal subtype returns the string value.
func TestDecodePortIDLocal(t *testing.T) {
	got := decodePortID(portSubtypeLocal, []byte("lo"))
	if got != "lo" {
		t.Errorf("decodePortID local = %q, want lo", got)
	}
}

// ---------- decodeChassisID tests ----------

// decodeChassisID: empty raw bytes returns "".
func TestDecodeChassisIDEmpty(t *testing.T) {
	got := decodeChassisID(chassisSubtypeMACAddress, []byte{})
	if got != "" {
		t.Errorf("decodeChassisID empty = %q, want empty string", got)
	}
}

// ---------- extractChassisIP tests ----------

// extractChassisIP: networkAddress subtype with IPv6 (IANA family 2, 16 octets).
func TestExtractChassisIPv6(t *testing.T) {
	raw := make([]byte, 17)
	raw[0] = 2 // IANA IPv6 family
	copy(raw[1:], net.ParseIP("fe80::1").To16())
	ip := extractChassisIP(chassisSubtypeNetworkAddress, raw)
	if ip == nil {
		t.Fatal("expected non-nil IPv6 address")
	}
	if ip.String() != "fe80::1" {
		t.Errorf("got %v, want fe80::1", ip)
	}
}

// extractChassisIP: networkAddress subtype but IPv4 raw length != 5 → nil.
func TestExtractChassisIPv4WrongLength(t *testing.T) {
	raw := []byte{1, 10, 0, 0} // IANA IPv4 family but only 3 IP octets (len=4, not 5)
	ip := extractChassisIP(chassisSubtypeNetworkAddress, raw)
	if ip != nil {
		t.Errorf("expected nil for wrong-length IPv4, got %v", ip)
	}
}

// extractChassisIP: networkAddress subtype but IPv6 raw length != 17 → nil.
func TestExtractChassisIPv6WrongLength(t *testing.T) {
	raw := []byte{2, 0xfe, 0x80, 0, 0} // IANA IPv6 family but only 4 IP octets (len=5, not 17)
	ip := extractChassisIP(chassisSubtypeNetworkAddress, raw)
	if ip != nil {
		t.Errorf("expected nil for wrong-length IPv6, got %v", ip)
	}
}

// ---------- buildEdges IEEE 802.1AB validation rejection tests ----------

// buildEdges: chassis subtype 0 (below valid range 1–7) → entry dropped.
func TestBuildEdgesInvalidChassisSubtypeLow(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: 0, // invalid: below range 1–7
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Eth0"),
			sysName:        "peer",
		},
	}
	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for invalid chassis subtype 0, got %d", len(edges))
	}
}

// buildEdges: chassis subtype 8 (above valid range 1–7) → entry dropped.
func TestBuildEdgesInvalidChassisSubtypeHigh(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: 8, // invalid: above range 1–7
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Eth0"),
			sysName:        "peer",
		},
	}
	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for invalid chassis subtype 8, got %d", len(edges))
	}
}

// buildEdges: port subtype 0 (below valid range 1–7) → entry dropped.
func TestBuildEdgesInvalidPortSubtypeLow(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    0, // invalid: below range 1–7
			portID:         []byte("Eth0"),
			sysName:        "peer",
		},
	}
	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for invalid port subtype 0, got %d", len(edges))
	}
}

// buildEdges: port subtype 8 (above valid range 1–7) → entry dropped.
func TestBuildEdgesInvalidPortSubtypeHigh(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    8, // invalid: above range 1–7
			portID:         []byte("Eth0"),
			sysName:        "peer",
		},
	}
	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for invalid port subtype 8, got %d", len(edges))
	}
}

// buildEdges: chassis subtype MAC (4) but chassisID length != 6 → entry dropped.
func TestBuildEdgesMACChassisIDWrongLength(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0, 1, 2, 3}, // only 4 bytes, not 6
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Eth0"),
			sysName:        "peer",
		},
	}
	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for MAC chassis ID with wrong length, got %d", len(edges))
	}
}

// buildEdges: chassis subtype networkAddress (5) with unknown IANA family byte → entry dropped.
func TestBuildEdgesNetworkAddressChassisIDUnknownFamily(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeNetworkAddress,
			chassisID:      []byte{3, 10, 0, 0, 1}, // family byte 3: not IPv4 (1) or IPv6 (2)
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Eth0"),
			sysName:        "peer",
		},
	}
	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for network-address chassis ID with unknown family, got %d", len(edges))
	}
}

// buildEdges: chassis subtype networkAddress (5) with IPv4 family but wrong total length → entry dropped.
func TestBuildEdgesNetworkAddressChassisIDIPv4WrongLength(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeNetworkAddress,
			chassisID:      []byte{1, 10, 0, 0}, // family=IPv4 but only 3 IP octets (len=4, not 5)
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Eth0"),
			sysName:        "peer",
		},
	}
	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for network-address chassis ID with IPv4 wrong length, got %d", len(edges))
	}
}

// buildEdges: port subtype MAC (3) but portID length != 6 → entry dropped.
func TestBuildEdgesMACPortIDWrongLength(t *testing.T) {
	locPorts := map[int]locPort{}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    portSubtypeMACAddress,
			portID:         []byte{0, 1, 2, 3}, // only 4 bytes, not 6
			sysName:        "peer",
		},
	}
	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for MAC port ID with wrong length, got %d", len(edges))
	}
}

// buildEdges: peer with MAC chassis ID and sysName populates peer_chassis_mac in Metadata.
func TestBuildEdgesPeerChassisMACMetadata(t *testing.T) {
	locPorts := map[int]locPort{
		1: {idSubtype: portSubtypeInterfaceName, id: []byte("Ethernet0")},
	}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e},
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Ethernet1"),
			sysName:        "some-peer",
		},
	}

	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.Metadata == nil {
		t.Fatal("expected Metadata to be non-nil")
	}
	mac, ok := e.Metadata[discovery.MetadataKeyPeerChassisMac]
	if !ok {
		t.Fatal("expected peer_chassis_mac key in Metadata")
	}
	if mac != "00:1a:2b:3c:4d:5e" {
		t.Errorf("peer_chassis_mac = %q, want 00:1a:2b:3c:4d:5e", mac)
	}
}

// buildEdges: peer with MAC chassis ID but no sysName does NOT populate peer_chassis_mac.
func TestBuildEdgesNoPeerChassisMACWithoutSysName(t *testing.T) {
	locPorts := map[int]locPort{
		1: {idSubtype: portSubtypeInterfaceName, id: []byte("Ethernet0")},
	}
	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: chassisSubtypeMACAddress,
			chassisID:      []byte{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e},
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Ethernet1"),
			sysName:        "", // no sysName → no peer_chassis_mac
		},
	}

	edges, _, _, err := buildEdges(context.Background(), "me", locPorts, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.Metadata != nil {
		if _, ok := e.Metadata[discovery.MetadataKeyPeerChassisMac]; ok {
			t.Error("expected peer_chassis_mac to be absent when sysName is empty")
		}
	}
}

// ---------- fmtNetAddr tests ----------

// fmtNetAddr: when extractChassisIP returns nil (wrong-length raw), falls back to hex.
func TestFmtNetAddrHexFallback(t *testing.T) {
	// raw[0]=1 (IPv4 family) but len=3 (not 5) → extractChassisIP returns nil → hex
	raw := []byte{1, 192, 168}
	got := fmtNetAddr(raw)
	if got != "01c0a8" {
		t.Errorf("fmtNetAddr hex fallback = %q, want 01c0a8", got)
	}
}

// ---------- IEEE 802.1AB compliance tests ----------

// TestIEEE802_1ABCompliance covers binary/UTF-8 handling for subtype 7 (local)
// and confirms well-known subtype decoding remains correct.
func TestIEEE802_1ABCompliance(t *testing.T) {
	tests := []struct {
		name    string
		fn      func(int, []byte) string
		subtype int
		raw     []byte
		want    string
	}{
		// 1. decodeChassisID: binary subtype 7 → hex-encoded (not mojibake).
		{
			name:    "decodeChassisID binary subtype 7 → hex",
			fn:      decodeChassisID,
			subtype: portSubtypeLocal, // 7 — same value for both chassis and port
			raw:     []byte{0x01, 0x02, 0xfe, 0xff},
			want:    "0102feff",
		},
		// 2. decodeChassisID: subtype 7 with valid UTF-8 → string preserved.
		{
			name:    "decodeChassisID subtype 7 valid UTF-8 → preserved",
			fn:      decodeChassisID,
			subtype: portSubtypeLocal,
			raw:     []byte("arista-sw-1"),
			want:    "arista-sw-1",
		},
		// 3. decodePortID: binary subtype 7 → hex-encoded.
		{
			name:    "decodePortID binary subtype 7 → hex",
			fn:      decodePortID,
			subtype: portSubtypeLocal,
			raw:     []byte{0x01, 0x02, 0xfe, 0xff},
			want:    "0102feff",
		},
		// 4. decodePortID: subtype 7 with valid UTF-8 → string preserved.
		{
			name:    "decodePortID subtype 7 valid UTF-8 → preserved",
			fn:      decodePortID,
			subtype: portSubtypeLocal,
			raw:     []byte("arista-sw-1"),
			want:    "arista-sw-1",
		},
		// 5. decodeChassisID: MAC subtype (4) with zero bytes → empty string
		//    (len check fires first).
		{
			name:    "decodeChassisID empty raw → empty string",
			fn:      decodeChassisID,
			subtype: chassisSubtypeMACAddress,
			raw:     []byte{},
			want:    "",
		},
		// 6. decodePortID: null-terminated ASCII with interface-name subtype (5) → trimmed.
		{
			name:    "decodePortID null-terminated ASCII → trimmed",
			fn:      decodePortID,
			subtype: portSubtypeInterfaceName,
			raw:     []byte("Gi0/1\x00"),
			want:    "Gi0/1",
		},
		// 7. decodeChassisID: MAC subtype (4) → colon-hex notation.
		{
			name:    "decodeChassisID MAC subtype → formatted MAC",
			fn:      decodeChassisID,
			subtype: chassisSubtypeMACAddress,
			raw:     []byte{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e},
			want:    "00:1a:2b:3c:4d:5e",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.fn(tc.subtype, tc.raw)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------- decode-issue reporter tests (issue #99) ----------

// buildEdges: a row rejected for an out-of-range chassis subtype reports a
// decode issue to the reporter installed on ctx with reason
// chassis_subtype_invalid and the lldpRemTable root OID.
func TestBuildEdgesReportsDecodeIssue(t *testing.T) {
	var issues []snmputil.DecodeIssue
	ctx := snmputil.ContextWithDecodeIssueReporter(context.Background(), func(i snmputil.DecodeIssue) {
		issues = append(issues, i)
	})

	remEntries := map[remKey]*remEntry{
		{1, 1}: {
			chassisSubtype: 0, // invalid: below IEEE 802.1AB range 1–7
			chassisID:      []byte{0, 1, 2, 3, 4, 5},
			portSubtype:    portSubtypeInterfaceName,
			portID:         []byte("Eth0"),
			sysName:        "peer",
		},
	}

	edges, _, _, err := buildEdges(ctx, "me", map[int]locPort{}, remEntries, nil)
	if err != nil {
		t.Fatalf("buildEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("expected 0 edges, got %d", len(edges))
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 decode issue, got %d: %v", len(issues), issues)
	}
	if issues[0].Module != walkerLLDP || issues[0].Reason != "chassis_subtype_invalid" {
		t.Errorf("issue = {%q,%q}, want {lldp,chassis_subtype_invalid}", issues[0].Module, issues[0].Reason)
	}
	if string(issues[0].OID) != oidRemTable {
		t.Errorf("issue OID = %q, want %q", issues[0].OID, oidRemTable)
	}
}

// ---------- TestMandatoryTLVValidation ----------

// TestMandatoryTLVValidation verifies that buildEdges drops entries whose
// chassisSubtype or portSubtype fall outside the IEEE 802.1AB-defined range
// of 1–7. The validation is already implemented in buildEdges; the four
// subtests below confirm all four boundary conditions (chassis low/high,
// port low/high).
func TestMandatoryTLVValidation(t *testing.T) {
	tests := []struct {
		name  string
		entry remEntry
	}{
		{
			name: "chassisSubtype below range (0)",
			entry: remEntry{
				chassisSubtype: 0,
				chassisID:      []byte{0, 1, 2, 3, 4, 5},
				portSubtype:    portSubtypeInterfaceName,
				portID:         []byte("Eth0"),
				sysName:        "peer",
			},
		},
		{
			name: "chassisSubtype above range (8)",
			entry: remEntry{
				chassisSubtype: 8,
				chassisID:      []byte{0, 1, 2, 3, 4, 5},
				portSubtype:    portSubtypeInterfaceName,
				portID:         []byte("Eth0"),
				sysName:        "peer",
			},
		},
		{
			name: "portSubtype below range (0)",
			entry: remEntry{
				chassisSubtype: chassisSubtypeMACAddress,
				chassisID:      []byte{0, 1, 2, 3, 4, 5},
				portSubtype:    0,
				portID:         []byte("Eth0"),
				sysName:        "peer",
			},
		},
		{
			name: "portSubtype above range (8)",
			entry: remEntry{
				chassisSubtype: chassisSubtypeMACAddress,
				chassisID:      []byte{0, 1, 2, 3, 4, 5},
				portSubtype:    8,
				portID:         []byte("Eth0"),
				sysName:        "peer",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := tc.entry
			remEntries := map[remKey]*remEntry{{1, 1}: &entry}
			edges, _, _, err := buildEdges(context.Background(), "me", map[int]locPort{}, remEntries, nil)
			if err != nil {
				t.Fatalf("buildEdges: %v", err)
			}
			if len(edges) != 0 {
				t.Errorf("expected entry to be dropped, got %d edge(s)", len(edges))
			}
		})
	}
}
