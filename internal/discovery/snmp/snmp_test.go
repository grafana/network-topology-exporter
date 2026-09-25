package snmp

import (
	"context"
	"math"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	gsnmp "github.com/gosnmp/gosnmp"
	"golang.org/x/time/rate"

	"github.com/grafana/network-topology-exporter/internal/snmptest"
)

// PDUString: handles both string and []byte values.
func TestPDUString(t *testing.T) {
	cases := []struct {
		val  any
		want string
	}{
		{"hello", "hello"},
		{[]byte("world"), "world"},
		{42, ""},
	}
	for _, c := range cases {
		got := PDUString(gsnmp.SnmpPDU{Value: c.val})
		if got != c.want {
			t.Errorf("PDUString(%v) = %q, want %q", c.val, got, c.want)
		}
	}
}

// PDUString: trailing NUL bytes are stripped from both string and []byte values.
// Some SNMP devices terminate OctetString values with \x00 (e.g. sysName =
// "router1\x00"). Without stripping, "router1\x00" and "router1" would be
// treated as different graph keys.
func TestPDUStringStripsTrailingNUL(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want string
	}{
		// string path: trailing NUL stripped.
		{"string trailing NUL", "router1\x00", "router1"},
		// []byte path: trailing NUL stripped.
		{"[]byte trailing NUL", []byte{'r', 'o', 'u', 't', 'e', 'r', '1', 0x00}, "router1"},
		// Multiple trailing NULs are all stripped.
		{"string multiple trailing NULs", "sw-01\x00\x00", "sw-01"},
		{"[]byte multiple trailing NULs", []byte{'s', 'w', 0x00, 0x00}, "sw"},
		// NUL in the middle is NOT stripped (TrimRight only removes from the right).
		{"string NUL in middle preserved", "ro\x00ter", "ro\x00ter"},
		{"[]byte NUL in middle preserved", []byte{'r', 'o', 0x00, 't', 'e', 'r'}, "ro\x00ter"},
		// No NUL: value is returned unchanged.
		{"string no NUL", "spine-01", "spine-01"},
		{"[]byte no NUL", []byte("spine-01"), "spine-01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PDUString(gsnmp.SnmpPDU{Value: c.val})
			if got != c.want {
				t.Errorf("PDUString(%q) = %q, want %q", c.val, got, c.want)
			}
		})
	}
}

// PDUInt: handles int, int32, uint, uint32, returns 0 for other types.
func TestPDUInt(t *testing.T) {
	cases := []struct {
		val  any
		want int
	}{
		{int(5), 5},
		{int32(3), 3},
		{uint(9), 9},
		{uint32(7), 7},
		{float64(1.0), 0},
	}
	for _, c := range cases {
		if got := PDUInt(gsnmp.SnmpPDU{Value: c.val}); got != c.want {
			t.Errorf("PDUInt(%v) = %d, want %d", c.val, got, c.want)
		}
	}
}

// PDUIntStrict: returns ok=false for unsupported value types.
func TestPDUIntStrict(t *testing.T) {
	cases := []struct {
		val    any
		want   int
		wantOK bool
	}{
		{int(5), 5, true},
		{int32(3), 3, true},
		{uint(9), 9, true},
		{uint32(7), 7, true},
		{int64(11), 11, true},
		{uint64(13), 13, true},
		{uint64(64512), 64512, true},
		{uint32(4294967295), 4294967295, true}, // Gauge32 max — fits int on 64-bit, must not be rejected
		{int64(math.MaxInt64), math.MaxInt64, true},
		{uint64(math.MaxInt64) + 1, 0, false},
		{uint64(math.MaxUint64), 0, false},
		{float64(1.0), 0, false},
		{"7", 0, false},
	}
	for _, c := range cases {
		got, ok := PDUIntStrict(gsnmp.SnmpPDU{Value: c.val})
		if got != c.want || ok != c.wantOK {
			t.Errorf("PDUIntStrict(%v) = (%d,%v), want (%d,%v)", c.val, got, ok, c.want, c.wantOK)
		}
	}
}

// PDUBytes: handles []byte and string, returns nil for other types.
func TestPDUBytes(t *testing.T) {
	if got := PDUBytes(gsnmp.SnmpPDU{Value: []byte{1, 2}}); string(got) != string([]byte{1, 2}) {
		t.Errorf("[]byte: got %v, want [1 2]", got)
	}
	if got := PDUBytes(gsnmp.SnmpPDU{Value: "abc"}); string(got) != "abc" {
		t.Errorf("string: got %v, want abc", got)
	}
	if got := PDUBytes(gsnmp.SnmpPDU{Value: 42}); got != nil {
		t.Errorf("int: got %v, want nil", got)
	}
}

// PDUIPv4: handles dotted-decimal string, raw 4-byte slice, and invalid inputs.
func TestPDUIPv4(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want string // empty string means nil expected
	}{
		{"string dotted decimal", "192.0.2.1", "192.0.2.1"},
		{"raw 4-byte slice", []byte{10, 0, 0, 1}, "10.0.0.1"},
		{"invalid string", "not-an-ip", ""},
		{"3-byte slice", []byte{1, 2, 3}, ""},
		{"integer", 42, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PDUIPv4(gsnmp.SnmpPDU{Value: c.val})
			if c.want == "" {
				if got != nil {
					t.Errorf("PDUIPv4(%v) = %v, want nil", c.val, got)
				}
			} else {
				if got == nil || got.String() != c.want {
					t.Errorf("PDUIPv4(%v) = %v, want %s", c.val, got, c.want)
				}
			}
		})
	}
}

// SplitOIDComponent: normal case with a dot.
func TestSplitOIDComponent(t *testing.T) {
	col, rest, ok := SplitOIDComponent("6.1.2")
	if !ok || col != 6 || rest != "1.2" {
		t.Errorf("SplitOIDComponent(6.1.2) = (%d, %q, %v), want (6, 1.2, true)", col, rest, ok)
	}
}

// SplitOIDComponent: last component (no dot) returns value with empty rest.
func TestSplitOIDComponentLast(t *testing.T) {
	col, rest, ok := SplitOIDComponent("42")
	if !ok || col != 42 || rest != "" {
		t.Errorf("SplitOIDComponent(42) = (%d, %q, %v), want (42, '', true)", col, rest, ok)
	}
}

// SplitOIDComponent: non-numeric prefix fails.
func TestSplitOIDComponentInvalid(t *testing.T) {
	_, _, ok := SplitOIDComponent("abc.1")
	if ok {
		t.Error("SplitOIDComponent(abc.1) should return ok=false")
	}
}

// IPInNets: IP inside a network returns true.
func TestIPInNets(t *testing.T) {
	_, net1, _ := net.ParseCIDR("10.0.0.0/8")
	_, net2, _ := net.ParseCIDR("192.168.0.0/16")

	if !IPInNets(net.ParseIP("10.1.2.3"), []*net.IPNet{net1, net2}) {
		t.Error("expected 10.1.2.3 to be in 10.0.0.0/8")
	}
	if !IPInNets(net.ParseIP("192.168.5.1"), []*net.IPNet{net1, net2}) {
		t.Error("expected 192.168.5.1 to be in 192.168.0.0/16")
	}
	if IPInNets(net.ParseIP("172.16.0.1"), []*net.IPNet{net1, net2}) {
		t.Error("expected 172.16.0.1 to NOT be in either network")
	}
	if IPInNets(net.ParseIP("1.2.3.4"), nil) {
		t.Error("expected false for nil nets slice")
	}
}

// WalkToIntMap: returns a map from OID suffix to integer value.
func TestWalkToIntMap(t *testing.T) {
	const oid = "1.3.6.1.2.1.138.1.6.1.1.2"
	pdus := []gsnmp.SnmpPDU{
		{Name: "." + oid + ".0.1.1", Type: gsnmp.Integer, Value: 3},
		{Name: "." + oid + ".0.1.2", Type: gsnmp.Integer, Value: 1},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	client, err := Open(Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()
	m, err := WalkToIntMap(context.Background(), client, oid)
	if err != nil {
		t.Fatalf("WalkToIntMap: %v", err)
	}
	if m["0.1.1"] != 3 || m["0.1.2"] != 1 {
		t.Errorf("WalkToIntMap = %v, want {0.1.1:3, 0.1.2:1}", m)
	}
}

// WalkToIntMapStrict: invalid types are excluded and reported.
func TestWalkToIntMapStrictDecodeFailures(t *testing.T) {
	const oid = "1.3.6.1.2.1.138.1.6.1.1.2"
	pdus := []gsnmp.SnmpPDU{
		{Name: "." + oid + ".0.1.1", Type: gsnmp.Integer, Value: 3},
		{Name: "." + oid + ".0.1.2", Type: gsnmp.OctetString, Value: []byte("bad")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	client, err := Open(Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	var gotIssue DecodeIssue
	ctx := ContextWithDecodeIssueReporter(context.Background(), func(issue DecodeIssue) { gotIssue = issue })
	m, stats, err := WalkToIntMapStrict(ctx, client, "isis", oid)
	if err != nil {
		t.Fatalf("WalkToIntMapStrict: %v", err)
	}
	if m["0.1.1"] != 3 {
		t.Errorf("WalkToIntMapStrict map = %v, want key 0.1.1=3", m)
	}
	if _, ok := m["0.1.2"]; ok {
		t.Errorf("WalkToIntMapStrict map unexpectedly included decode-failed key: %v", m)
	}
	if stats.DecodeFailures != 1 {
		t.Errorf("DecodeFailures = %d, want 1", stats.DecodeFailures)
	}
	if stats.TotalRows != 2 || stats.ValidRows != 1 || stats.InvalidRows != 1 {
		t.Errorf("stats rows = %+v, want total=2 valid=1 invalid=1", stats)
	}
	if stats.InvalidRatio <= 0 || stats.InvalidRatio >= 1 {
		t.Errorf("InvalidRatio = %f, want in (0,1)", stats.InvalidRatio)
	}
	if gotIssue.Module != "isis" || gotIssue.OID != oid || gotIssue.Reason != "invalid_type" || gotIssue.Count != 1 {
		t.Errorf("DecodeIssue = %+v, want module=isis oid=%s reason=invalid_type count=1", gotIssue, oid)
	}
}

func TestEvaluateRequiredTablePolicy(t *testing.T) {
	policy := RequiredTablePolicy{MinValidRows: 1, MaxInvalidRatio: 0.5}
	cases := []struct {
		name        string
		stats       IntMapDecodeStats
		wantOutcome TableOutcome
		wantReason  string
	}{
		{
			name:        "clean table",
			stats:       IntMapDecodeStats{TotalRows: 10, ValidRows: 10, InvalidRows: 0, InvalidRatio: 0},
			wantOutcome: TableOK,
			wantReason:  "",
		},
		{
			name:        "partial anomalies below threshold",
			stats:       IntMapDecodeStats{TotalRows: 10, ValidRows: 8, InvalidRows: 2, InvalidRatio: 0.2},
			wantOutcome: TableDegraded,
			wantReason:  "",
		},
		{
			name:        "no valid rows",
			stats:       IntMapDecodeStats{TotalRows: 2, ValidRows: 0, InvalidRows: 2, InvalidRatio: 1.0},
			wantOutcome: TableHardFail,
			wantReason:  "required_table_no_valid_rows",
		},
		{
			name:        "invalid ratio exceeded",
			stats:       IntMapDecodeStats{TotalRows: 3, ValidRows: 1, InvalidRows: 2, InvalidRatio: 0.666},
			wantOutcome: TableHardFail,
			wantReason:  "required_table_invalid_ratio_exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := EvaluateRequiredTablePolicy(tc.stats, policy)
			if verdict.Outcome != tc.wantOutcome || verdict.Reason != tc.wantReason {
				t.Errorf("EvaluateRequiredTablePolicy(%+v) = {Outcome:%v Reason:%q}, want {Outcome:%v Reason:%q}", tc.stats, verdict.Outcome, verdict.Reason, tc.wantOutcome, tc.wantReason)
			}
		})
	}
}

// WalkIfDescr: returns a map from ifIndex to ifDescr string.
func TestWalkIfDescr(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: "." + OIDIfDescr + ".1", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/0")},
		{Name: "." + OIDIfDescr + ".2", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/1")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	client, err := Open(Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()
	m, err := WalkIfDescr(context.Background(), client)
	if err != nil {
		t.Fatalf("WalkIfDescr: %v", err)
	}
	if m[1] != "GigabitEthernet0/0" || m[2] != "GigabitEthernet0/1" {
		t.Errorf("WalkIfDescr = %v, want {1:GigabitEthernet0/0, 2:GigabitEthernet0/1}", m)
	}
}

// WalkIfDescr: a PDU with the wrong type (Integer32) yields an empty string
// for that index. Valid OctetString rows are unaffected.
func TestWalkIfDescrWrongType(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: "." + OIDIfDescr + ".1", Type: gsnmp.OctetString, Value: []byte("eth0")},
		{Name: "." + OIDIfDescr + ".2", Type: gsnmp.Integer, Value: 42},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	client, err := Open(Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()
	m, err := WalkIfDescr(context.Background(), client)
	if err != nil {
		t.Fatalf("WalkIfDescr: %v", err)
	}
	if m[1] != "eth0" {
		t.Errorf("m[1] = %q, want eth0", m[1])
	}
	// PDUString returns "" for Integer values; the key is still present.
	if got, ok := m[2]; !ok || got != "" {
		t.Errorf("m[2] = %q (present=%v), want \"\" (present=true)", got, ok)
	}
}

// Walk: sysName, sysDescr, sysObjectID, and sysUpTime round-trip through a
// fake agent and come back in the Device record.
func TestWalkSystemGroup(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("Cisco IOS 15.2")},
		{Name: ".1.3.6.1.2.1.1.2.0", Type: gsnmp.ObjectIdentifier, Value: ".1.3.6.1.4.1.9.1.1"},
		{Name: ".1.3.6.1.2.1.1.3.0", Type: gsnmp.TimeTicks, Value: uint32(100000)},
		{Name: ".1.3.6.1.2.1.1.5.0", Type: gsnmp.OctetString, Value: []byte("core-sw-01")},
	}

	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"),
		Timeout:   3 * time.Second,
	}

	dev, err := Walk(context.Background(), p)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if dev == nil {
		t.Fatal("Walk returned nil device")
		return
	}
	if dev.ID != "core-sw-01" {
		t.Errorf("ID = %q, want core-sw-01", dev.ID)
	}
	if dev.Vendor != "cisco" {
		t.Errorf("Vendor = %q, want cisco", dev.Vendor)
	}
	// normalizeSysDescr extracts the first version token from the sysDescr.
	// "Cisco IOS 15.2" → "15.2"
	if dev.OSVersion != "15.2" {
		t.Errorf("OSVersion = %q, want 15.2 (normalized from sysDescr)", dev.OSVersion)
	}
	if dev.Uptime != time.Duration(100000)*10*time.Millisecond {
		t.Errorf("Uptime = %v, want 1000s", dev.Uptime)
	}
}

// Walk: sysUpTime PDU with wrong type (OctetString instead of TimeTicks)
// is silently ignored; the Device.Uptime field remains zero.
func TestWalkSystemGroupSysUpTimeWrongType(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("Cisco IOS 15.2")},
		{Name: ".1.3.6.1.2.1.1.2.0", Type: gsnmp.ObjectIdentifier, Value: ".1.3.6.1.4.1.9.1.1"},
		{Name: ".1.3.6.1.2.1.1.3.0", Type: gsnmp.OctetString, Value: []byte("not-a-timeticks")},
		{Name: ".1.3.6.1.2.1.1.5.0", Type: gsnmp.OctetString, Value: []byte("core-sw-01")},
	}

	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	dev, err := Walk(context.Background(), Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if dev == nil {
		t.Fatal("Walk returned nil device")
		return
	}
	if dev.ID != "core-sw-01" {
		t.Errorf("ID = %q, want core-sw-01", dev.ID)
	}
	// sysUpTime PDU had wrong type; PDUInt returns 0, so Uptime should be zero.
	if dev.Uptime != 0 {
		t.Errorf("Uptime = %v, want 0 (wrong-type sysUpTime silently ignored)", dev.Uptime)
	}
}

// Walk: sysUpTime PDU with a negative integer value must not set dev.Uptime.
// A malformed SNMP response could return a negative value for sysUpTime (e.g.
// via a signed Integer type); the ticks >= 0 guard prevents a negative
// time.Duration from being stored. We use gsnmp.Integer (signed) with a
// negative value so the PDU survives wire encoding through the test agent.
func TestWalkSystemRejectsNegativeSysUpTime(t *testing.T) {
	t.Run("negative int leaves Uptime zero", func(t *testing.T) {
		pdus := []gsnmp.SnmpPDU{
			{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("Cisco IOS 15.2")},
			{Name: ".1.3.6.1.2.1.1.2.0", Type: gsnmp.ObjectIdentifier, Value: ".1.3.6.1.4.1.9.1.1"},
			// Malformed: negative signed Integer for sysUpTime OID.
			{Name: ".1.3.6.1.2.1.1.3.0", Type: gsnmp.Integer, Value: int(-1)},
			{Name: ".1.3.6.1.2.1.1.5.0", Type: gsnmp.OctetString, Value: []byte("core-sw-01")},
		}

		addr := snmptest.Start(t, "public", pdus)
		ip, port := snmptest.ParseAddr(addr)

		dev, err := Walk(context.Background(), Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if dev == nil {
			t.Fatal("Walk returned nil device")
			return
		}
		if dev.Uptime != 0 {
			t.Errorf("Uptime = %v, want 0 (negative sysUpTime must be rejected)", dev.Uptime)
		}
	})

	t.Run("positive int sets Uptime correctly", func(t *testing.T) {
		pdus := []gsnmp.SnmpPDU{
			{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("Cisco IOS 15.2")},
			{Name: ".1.3.6.1.2.1.1.2.0", Type: gsnmp.ObjectIdentifier, Value: ".1.3.6.1.4.1.9.1.1"},
			// Valid positive signed Integer for sysUpTime (50000 ticks = 500s).
			{Name: ".1.3.6.1.2.1.1.3.0", Type: gsnmp.Integer, Value: int(50000)},
			{Name: ".1.3.6.1.2.1.1.5.0", Type: gsnmp.OctetString, Value: []byte("core-sw-01")},
		}

		addr := snmptest.Start(t, "public", pdus)
		ip, port := snmptest.ParseAddr(addr)

		dev, err := Walk(context.Background(), Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if dev == nil {
			t.Fatal("Walk returned nil device")
			return
		}
		want := time.Duration(50000) * 10 * time.Millisecond
		if dev.Uptime != want {
			t.Errorf("Uptime = %v, want %v", dev.Uptime, want)
		}
	})
}

// Walk: devices with leading/trailing whitespace in sysName are normalised.
func TestWalkNormalisesSysName(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("")},
		{Name: ".1.3.6.1.2.1.1.2.0", Type: gsnmp.ObjectIdentifier, Value: ".1.3.6.1.4.1.30065.1"},
		{Name: ".1.3.6.1.2.1.1.3.0", Type: gsnmp.TimeTicks, Value: uint32(0)},
		{Name: ".1.3.6.1.2.1.1.5.0", Type: gsnmp.OctetString, Value: []byte("  Spine-01  ")},
	}

	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	dev, err := Walk(context.Background(), Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if dev.ID != "spine-01" {
		t.Errorf("ID = %q, want spine-01 (lower-cased, trimmed)", dev.ID)
	}
	if dev.SysName != "Spine-01" {
		t.Errorf("SysName = %q, want Spine-01 (trimmed, case preserved)", dev.SysName)
	}
	if dev.Vendor != "arista" {
		t.Errorf("Vendor = %q, want arista", dev.Vendor)
	}
}

// VendorFromObjectID: known enterprise prefixes map to the right vendor.
func TestVendorFromObjectID(t *testing.T) {
	cases := []struct {
		oid    string
		vendor string
	}{
		{".1.3.6.1.4.1.9.1.1", "cisco"},
		{"1.3.6.1.4.1.2636.3.1", "juniper"},
		{".1.3.6.1.4.1.30065.1.3011", "arista"},
		{"1.3.6.1.4.1.14988.1", "mikrotik"},
		{"1.3.6.1.4.1.9999.99", "unknown"},
		{"", "unknown"},
	}
	for _, c := range cases {
		if got := VendorFromObjectID(c.oid); got != c.vendor {
			t.Errorf("VendorFromObjectID(%q) = %q, want %q", c.oid, got, c.vendor)
		}
	}
}

// parseAuthProto: string values map to the right gosnmp constants.
func TestParseAuthProto(t *testing.T) {
	cases := []struct {
		s    string
		want gsnmp.SnmpV3AuthProtocol
	}{
		{"MD5", gsnmp.MD5},
		{"SHA-256", gsnmp.SHA256},
		{"SHA-384", gsnmp.SHA384},
		{"SHA-512", gsnmp.SHA512},
		{"SHA", gsnmp.SHA},
		{"", gsnmp.NoAuth},
		{"UNKNOWN", gsnmp.SHA},
	}
	for _, c := range cases {
		if got := parseAuthProto(c.s); got != c.want {
			t.Errorf("parseAuthProto(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// parsePrivProto: string values map to the right gosnmp constants.
func TestParsePrivProto(t *testing.T) {
	cases := []struct {
		s    string
		want gsnmp.SnmpV3PrivProtocol
	}{
		{"DES", gsnmp.DES},
		{"AES-192", gsnmp.AES192},
		{"AES-256", gsnmp.AES256},
		{"AES", gsnmp.AES},
		{"", gsnmp.NoPriv},
	}
	for _, c := range cases {
		if got := parsePrivProto(c.s); got != c.want {
			t.Errorf("parsePrivProto(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// pduOID: only string values return a non-empty result.
func TestPduOID(t *testing.T) {
	if got := pduOID(gsnmp.SnmpPDU{Value: ".1.2.3"}); got != ".1.2.3" {
		t.Errorf("pduOID string = %q, want .1.2.3", got)
	}
	if got := pduOID(gsnmp.SnmpPDU{Value: []byte(".1.2.3")}); got != "" {
		t.Errorf("pduOID []byte = %q, want empty", got)
	}
}

// buildClient: v2c community is set correctly.
func TestBuildClientV2c(t *testing.T) {
	p := Params{IP: net.ParseIP("192.0.2.1"), Port: 161, Community: []byte("myCommunity")}
	c := buildClient(p)
	if c.Community != "myCommunity" {
		t.Errorf("Community = %q, want myCommunity", c.Community)
	}
	if c.Version != gsnmp.Version2c {
		t.Errorf("Version = %v, want Version2c", c.Version)
	}
}

// buildClient: default port is 161, default timeout is 10s.
func TestBuildClientDefaults(t *testing.T) {
	p := Params{IP: net.ParseIP("10.0.0.1")}
	c := buildClient(p)
	if c.Port != 161 {
		t.Errorf("Port = %d, want 161", c.Port)
	}
	if c.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", c.Timeout)
	}
}

// buildClient: v3 with auth+priv sets AuthPriv MsgFlags.
func TestBuildClientV3AuthPriv(t *testing.T) {
	p := Params{
		IP:        net.ParseIP("10.0.0.1"),
		V3:        true,
		Username:  "admin",
		AuthProto: "SHA",
		AuthKey:   []byte("authpass"),
		PrivProto: "AES",
		PrivKey:   []byte("privpass"),
	}
	c := buildClient(p)
	if c.Version != gsnmp.Version3 {
		t.Errorf("Version = %v, want Version3", c.Version)
	}
	if c.MsgFlags != gsnmp.AuthPriv {
		t.Errorf("MsgFlags = %v, want AuthPriv", c.MsgFlags)
	}
}

// buildClient: v3 without auth/priv produces NoAuthNoPriv MsgFlags.
func TestBuildClientV3NoAuth(t *testing.T) {
	p := Params{IP: net.ParseIP("10.0.0.1"), V3: true, Username: "u"}
	c := buildClient(p)
	if c.Version != gsnmp.Version3 {
		t.Errorf("Version = %v, want Version3", c.Version)
	}
	if c.MsgFlags != gsnmp.NoAuthNoPriv {
		t.Errorf("MsgFlags = %v, want NoAuthNoPriv", c.MsgFlags)
	}
}

// buildClient: ContextName is forwarded when set.
func TestBuildClientV3ContextName(t *testing.T) {
	p := Params{
		IP:          net.ParseIP("10.0.0.1"),
		V3:          true,
		Username:    "u",
		ContextName: "myctx",
	}
	c := buildClient(p)
	if c.ContextName != "myctx" {
		t.Errorf("ContextName = %q, want myctx", c.ContextName)
	}
}

// authPrivMsgFlags: NoAuth → NoAuthNoPriv; auth-only → AuthNoPriv; auth+priv → AuthPriv.
func TestAuthPrivMsgFlags(t *testing.T) {
	cases := []struct {
		auth gsnmp.SnmpV3AuthProtocol
		priv gsnmp.SnmpV3PrivProtocol
		want gsnmp.SnmpV3MsgFlags
	}{
		{gsnmp.NoAuth, gsnmp.NoPriv, gsnmp.NoAuthNoPriv},
		{gsnmp.SHA, gsnmp.NoPriv, gsnmp.AuthNoPriv},
		{gsnmp.SHA, gsnmp.AES, gsnmp.AuthPriv},
	}
	for _, c := range cases {
		got := authPrivMsgFlags(c.auth, c.priv)
		if got != c.want {
			t.Errorf("authPrivMsgFlags(%v, %v) = %v, want %v", c.auth, c.priv, got, c.want)
		}
	}
}

// Open: creates a connected session against the fake agent.
func TestOpenConnect(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.5.0", Type: gsnmp.OctetString, Value: []byte("test-device")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	if client.Target != ip.String() {
		t.Errorf("Target = %q, want %s", client.Target, ip.String())
	}
}

// BulkWalk: cancelled context returns immediately without hitting the agent.
func TestBulkWalkCancelledCtx(t *testing.T) {
	addr := snmptest.Start(t, "public", nil)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = BulkWalk(ctx, client, "1.3.6.1.2.1.1")
	if err == nil {
		t.Error("expected error for already-cancelled context, got nil")
	}
}

// TrimOIDPrefix: valid prefix strips correctly.
func TestTrimOIDPrefix(t *testing.T) {
	rest, ok := TrimOIDPrefix(".1.2.3.4", ".1.2.")
	if !ok || rest != "3.4" {
		t.Errorf("TrimOIDPrefix = (%q, %v), want (3.4, true)", rest, ok)
	}
}

// TrimOIDPrefix: wrong prefix returns false.
func TestTrimOIDPrefixMismatch(t *testing.T) {
	_, ok := TrimOIDPrefix(".1.2.3", ".1.9.")
	if ok {
		t.Error("expected false for mismatched prefix")
	}
}

// TrimOIDPrefix: exact prefix with no suffix returns false.
func TestTrimOIDPrefixEmpty(t *testing.T) {
	_, ok := TrimOIDPrefix(".1.2.", ".1.2.")
	if ok {
		t.Error("expected false for empty suffix after prefix")
	}
}

// BulkWalk: returns PDUs matching the rootOID subtree.
func TestBulkWalk(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("desc")},
		{Name: ".1.3.6.1.2.1.1.5.0", Type: gsnmp.OctetString, Value: []byte("name")},
		{Name: ".1.3.6.1.2.1.2.1.0", Type: gsnmp.Integer, Value: int(5)}, // outside subtree
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	results, err := BulkWalk(context.Background(), client, "1.3.6.1.2.1.1")
	if err != nil {
		t.Fatalf("BulkWalk: %v", err)
	}
	if len(results) < 2 {
		t.Errorf("BulkWalk returned %d PDUs, want at least 2", len(results))
	}
}

// Open: V3 with empty username causes validateParametersV3 to fail immediately,
// exercising the connect-error return path in Open.
func TestOpenConnectError(t *testing.T) {
	p := Params{
		IP:      net.ParseIP("127.0.0.1"),
		Port:    161,
		Timeout: time.Second,
		V3:      true,
		// Username intentionally empty — gosnmp rejects it in validateParametersV3.
	}
	_, err := Open(p)
	if err == nil {
		t.Fatal("expected error for V3 with empty username, got nil")
	}
}

// BulkWalk: fallback to WalkAll when BulkWalkAll fails.
// gosnmp's BulkWalkAll fails when the connection is closed. We then fall
// through to WalkAll on the same closed connection, which also fails — but
// the important thing is the WalkAll fallback branch is reached.
func TestBulkWalkFallbackToWalkAll(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("desc")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Close the connection so that BulkWalkAll fails, exercising the fallback branch.
	_ = client.Conn.Close()

	ctx := context.Background()
	// Both BulkWalkAll and WalkAll will fail on the closed conn; we just need
	// the fallback branch to be reached (i.e., the WalkAll call happens).
	_, err = BulkWalk(ctx, client, "1.3.6.1.2.1.1")
	// An error is expected since the connection is closed.
	if err == nil {
		t.Log("BulkWalk unexpectedly succeeded on closed connection (platform may re-open)")
	}
}

// lazyErrCtx is a context.Context whose Err() returns nil for the first call
// and errCancelled for all subsequent calls. This lets us exercise the
// second ctx.Err() check in BulkWalk (after BulkWalkAll has already failed)
// without relying on goroutine scheduling.
type lazyErrCtx struct {
	context.Context
	called int
}

func (c *lazyErrCtx) Err() error {
	c.called++
	if c.called < 2 {
		return nil
	}
	return context.Canceled
}

// BulkWalk: when BulkWalkAll fails AND the context has since been cancelled,
// the second ctx.Err() check returns the context error (lines 111–113).
func TestBulkWalkCtxCancelledAfterBulkFail(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("desc")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Close the connection so BulkWalkAll fails immediately.
	_ = client.Conn.Close()

	// lazyErrCtx returns nil on the first Err() call (passes the entry guard),
	// then returns Canceled on the second call (hits the post-BulkWalkAll check).
	ctx := &lazyErrCtx{Context: context.Background()}
	_, err = BulkWalk(ctx, client, "1.3.6.1.2.1.1")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// Walk: Open failure (V3 with empty username) returns an error from Walk directly.
func TestWalkOpenFails(t *testing.T) {
	p := Params{
		IP:      net.ParseIP("127.0.0.1"),
		Port:    161,
		Timeout: time.Second,
		V3:      true,
		// Username intentionally empty — Open returns an error immediately.
	}
	dev, err := Walk(context.Background(), p)
	if err == nil {
		t.Fatal("expected error when Open fails, got nil")
	}
	if dev != nil {
		t.Errorf("expected nil device when Open fails, got %+v", dev)
	}
}

// Walk: context cancelled before the Get goroutine finishes exercises the
// ctx.Done select branch. We start an agent that won't respond (nil pdus +
// wrong community = drops all packets) and use a long SNMP timeout so the
// goroutine won't return from Get for a long time. The pre-cancelled context
// guarantees the select picks <-ctx.Done() over <-done.
func TestWalkCtxCancelled(t *testing.T) {
	// Start an agent with no PDUs using a community that we won't match,
	// so every packet is dropped and client.Get() blocks until the SNMP timeout.
	addr := snmptest.Start(t, "nomatch", nil)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{
		IP:        ip,
		Port:      port,
		Community: []byte("public"), // wrong community — agent drops all packets
		Timeout:   200 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — select will always pick ctx.Done()

	_, err := Walk(ctx, p)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}
}

// Walk: no sysName PDU returns the IP address as fallback device ID.
func TestWalkNoSysName(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("some desc")},
		{Name: ".1.3.6.1.2.1.1.2.0", Type: gsnmp.ObjectIdentifier, Value: ".1.3.6.1.4.1.9.1.1"},
		{Name: ".1.3.6.1.2.1.1.3.0", Type: gsnmp.TimeTicks, Value: uint32(500)},
		// note: no sysName (.1.3.6.1.2.1.1.5.0) PDU
	}

	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	dev, err := Walk(context.Background(), p)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if dev == nil {
		t.Fatal("Walk returned nil device")
		return
	}
	// ID should fall back to IP string when sysName is absent.
	if dev.ID != ip.String() {
		t.Errorf("ID = %q, want %s (IP fallback)", dev.ID, ip.String())
	}
	if dev.SysName != "" {
		t.Errorf("SysName = %q, want empty (no sysName PDU, no fallback for this field)", dev.SysName)
	}
}

// WalkIfNames: returns a correct ifIndex→ifName map from the agent.
func TestWalkIfNames(t *testing.T) {
	// ifXTable.ifName = 1.3.6.1.2.1.31.1.1.1.1
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.31.1.1.1.1.1", Type: gsnmp.OctetString, Value: []byte("eth0")},
		{Name: ".1.3.6.1.2.1.31.1.1.1.1.2", Type: gsnmp.OctetString, Value: []byte("eth1")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	names, err := WalkIfNames(context.Background(), client)
	if err != nil {
		t.Fatalf("WalkIfNames: %v", err)
	}
	if names[1] != "eth0" {
		t.Errorf("names[1] = %q, want eth0", names[1])
	}
	if names[2] != "eth1" {
		t.Errorf("names[2] = %q, want eth1", names[2])
	}
}

// WalkIfNames: BulkWalk error (closed connection) propagates as an error.
func TestWalkIfNamesBulkWalkError(t *testing.T) {
	addr := snmptest.Start(t, "public", nil)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: time.Millisecond}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = client.Conn.Close() // force failure

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled context guarantees BulkWalk returns immediately with error

	_, err = WalkIfNames(ctx, client)
	if err == nil {
		t.Error("expected error from WalkIfNames with cancelled context, got nil")
	}
}

// WalkIfNames: OID suffix that is not a plain integer (e.g. "1.2") is silently
// skipped, exercising the strconv.Atoi error path.
// We include a PDU whose name has a two-component suffix after the ifName
// prefix (e.g. .1.3.6.1.2.1.31.1.1.1.1.1.2 → suffix "1.2") so that Atoi
// fails. gosnmp handles this OID correctly; only our parsing skips it.
func TestWalkIfNamesNonNumericSuffix(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.31.1.1.1.1.3", Type: gsnmp.OctetString, Value: []byte("eth2")},
		// Two-component suffix "3.99" → Atoi("3.99") fails → entry skipped.
		{Name: ".1.3.6.1.2.1.31.1.1.1.1.3.99", Type: gsnmp.OctetString, Value: []byte("skip")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	names, err := WalkIfNames(context.Background(), client)
	if err != nil {
		t.Fatalf("WalkIfNames: %v", err)
	}
	// The valid entry is present; the one with non-Atoi-able suffix is absent.
	if names[3] != "eth2" {
		t.Errorf("names[3] = %q, want eth2", names[3])
	}
	// Confirm the two-component-suffix entry was skipped (no spurious keys).
	if len(names) != 1 {
		t.Errorf("expected 1 entry, got %d: %v", len(names), names)
	}
}

// WalkIfNames: same tolerance as WalkIfDescr — wrong-type PDUs yield "" for
// that index. Valid OctetString entries are unaffected.
func TestWalkIfNamesWrongType(t *testing.T) {
	// ifXTable.ifName = 1.3.6.1.2.1.31.1.1.1.1 (oidIfNameTable)
	const ifNameOID = "1.3.6.1.2.1.31.1.1.1.1"
	pdus := []gsnmp.SnmpPDU{
		{Name: "." + ifNameOID + ".1", Type: gsnmp.OctetString, Value: []byte("eth0")},
		{Name: "." + ifNameOID + ".2", Type: gsnmp.Integer, Value: 42},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	names, err := WalkIfNames(context.Background(), client)
	if err != nil {
		t.Fatalf("WalkIfNames: %v", err)
	}
	if names[1] != "eth0" {
		t.Errorf("names[1] = %q, want eth0", names[1])
	}
	// PDUString returns "" for Integer values; the key is still present.
	if got, ok := names[2]; !ok || got != "" {
		t.Errorf("names[2] = %q (present=%v), want \"\" (present=true)", got, ok)
	}
}

// WalkIfNamesWithFallback: when ifXTable returns empty and ifDescr returns
// results, the fallback ifDescr result is returned.
func TestWalkIfNamesWithFallback(t *testing.T) {
	// Serve only ifDescr PDUs — no ifXTable (ifName) PDUs. WalkIfNames will
	// succeed but return an empty map (no OIDs match), so the fallback fires.
	pdus := []gsnmp.SnmpPDU{
		{Name: "." + OIDIfDescr + ".1", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/0")},
		{Name: "." + OIDIfDescr + ".2", Type: gsnmp.OctetString, Value: []byte("GigabitEthernet0/1")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	client, err := Open(Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	names, err := WalkIfNamesWithFallback(context.Background(), client)
	if err != nil {
		t.Fatalf("WalkIfNamesWithFallback: %v", err)
	}
	if names[1] != "GigabitEthernet0/0" {
		t.Errorf("names[1] = %q, want GigabitEthernet0/0", names[1])
	}
	if names[2] != "GigabitEthernet0/1" {
		t.Errorf("names[2] = %q, want GigabitEthernet0/1", names[2])
	}
}

// WalkIfNamesWithFallback: when ifXTable has entries, they are returned without
// falling back to ifDescr.
func TestWalkIfNamesWithFallbackPrefersIfName(t *testing.T) {
	const ifNameOID = "1.3.6.1.2.1.31.1.1.1.1"
	pdus := []gsnmp.SnmpPDU{
		{Name: "." + ifNameOID + ".1", Type: gsnmp.OctetString, Value: []byte("eth0")},
		// ifDescr entry at a different index — must not appear in results.
		{Name: "." + OIDIfDescr + ".2", Type: gsnmp.OctetString, Value: []byte("should-not-appear")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	client, err := Open(Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	names, err := WalkIfNamesWithFallback(context.Background(), client)
	if err != nil {
		t.Fatalf("WalkIfNamesWithFallback: %v", err)
	}
	if names[1] != "eth0" {
		t.Errorf("names[1] = %q, want eth0", names[1])
	}
	// The ifDescr entry at index 2 must not appear — we returned early from ifName.
	if _, ok := names[2]; ok {
		t.Errorf("names[2] unexpectedly present; ifDescr fallback should not have been used")
	}
}

// ParseCIDRs: valid CIDRs are all returned.
func TestParseCIDRs(t *testing.T) {
	nets := ParseCIDRs([]string{"10.0.0.0/8", "192.168.0.0/16"})
	if len(nets) != 2 {
		t.Errorf("ParseCIDRs returned %d nets, want 2", len(nets))
	}
}

// ParseCIDRs: invalid entries are silently skipped.
func TestParseCIDRsSkipsInvalid(t *testing.T) {
	nets := ParseCIDRs([]string{"10.0.0.0/8", "not-a-cidr"})
	if len(nets) != 1 {
		t.Errorf("ParseCIDRs returned %d nets, want 1", len(nets))
	}
	if nets[0].String() != "10.0.0.0/8" {
		t.Errorf("nets[0] = %q, want 10.0.0.0/8", nets[0].String())
	}
}

// TestNormalizeSysDescr verifies that normalizeSysDescr extracts the first
// version-like token from a sysDescr string.
func TestNormalizeSysDescr(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "Cisco IOS full string extracts first version",
			input: "Cisco IOS Software, Version 15.2.4, RELEASE SOFTWARE (fc4)",
			want:  "15.2.4",
		},
		{
			name:  "empty string returns empty",
			input: "",
			want:  "",
		},
		{
			name:  "string with no version truncated at 64 chars",
			input: "Cisco Internetwork Operating System Software IOS (tm) C2950 Software",
			want:  "Cisco Internetwork Operating System Software IOS (tm) C2950 Soft",
		},
		{
			name:  "short string with no version returned as-is",
			input: "no version here",
			want:  "no version here",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeSysDescr(c.input)
			if got != c.want {
				t.Errorf("normalizeSysDescr(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// TestNormalizeSysDescrRuneBoundary verifies that normalizeSysDescr retreats to
// a rune boundary when truncating, so the result is always valid UTF-8.
func TestNormalizeSysDescrRuneBoundary(t *testing.T) {
	// Case 1: pure ASCII 65-byte string truncates cleanly to 64 bytes.
	ascii65 := strings.Repeat("a", 65)
	got := normalizeSysDescr(ascii65)
	if len(got) != 64 {
		t.Errorf("ASCII case: len = %d, want 64", len(got))
	}
	if got != ascii65[:64] {
		t.Errorf("ASCII case: got %q, want first 64 bytes unchanged", got)
	}

	// Case 2: 62 ASCII bytes followed by one 3-byte UTF-8 rune ("€" = 0xE2 0x82 0xAC)
	// = 65 bytes total. Byte 64 (index 63) is the first continuation byte 0x82, so
	// the function must retreat to byte 62, producing the 62-byte ASCII prefix.
	threeByteRune := "€" // U+20AC: 0xE2 0x82 0xAC
	input := strings.Repeat("b", 62) + threeByteRune
	if len(input) != 65 {
		t.Fatalf("test setup error: expected 65-byte string, got %d", len(input))
	}
	got = normalizeSysDescr(input)
	want := strings.Repeat("b", 62)
	if got != want {
		t.Errorf("rune-boundary case: got %q (len %d), want %q (len %d)", got, len(got), want, len(want))
	}
}

// TestBuildClientRetries verifies that Retries from Params propagates to GoSNMP.
func TestBuildClientRetries(t *testing.T) {
	p := Params{IP: net.ParseIP("10.0.0.1"), Retries: 3}
	c := buildClient(p)
	if c.Retries != 3 {
		t.Errorf("Retries = %d, want 3", c.Retries)
	}
}

// TestBuildClientRetriesDefault verifies that Retries=0 in Params defaults to 1.
func TestBuildClientRetriesDefault(t *testing.T) {
	p := Params{IP: net.ParseIP("10.0.0.1"), Retries: 0}
	c := buildClient(p)
	if c.Retries != 1 {
		t.Errorf("Retries = %d, want 1 (default)", c.Retries)
	}
}

// appendDecodeSample: adding to an under-capacity slice appends the sample.
func TestAppendDecodeSampleBasic(t *testing.T) {
	s := appendDecodeSample([]string{"a", "b"}, "c")
	if len(s) != 3 || s[2] != "c" {
		t.Errorf("appendDecodeSample = %v, want [a b c]", s)
	}
}

// appendDecodeSample: slice already at capacity (5) is returned unchanged.
func TestAppendDecodeSampleFull(t *testing.T) {
	full := []string{"a", "b", "c", "d", "e"}
	got := appendDecodeSample(full, "f")
	if len(got) != 5 {
		t.Errorf("appendDecodeSample on full slice: len = %d, want 5", len(got))
	}
	// Must be the exact same slice — no reallocation.
	if &got[0] != &full[0] {
		t.Error("appendDecodeSample on full slice returned a different slice")
	}
}

// ContextWithDecodeIssueReporter: nil fn returns the original context unchanged.
func TestContextWithDecodeIssueReporterNilFn(t *testing.T) {
	ctx := context.Background()
	got := ContextWithDecodeIssueReporter(ctx, nil)
	if got != ctx {
		t.Error("ContextWithDecodeIssueReporter(nil) should return the original context")
	}
}

// ContextWithDecodeIssueReporter: non-nil fn is stored and called via reportDecodeIssue.
func TestContextWithDecodeIssueReporterCallsReporter(t *testing.T) {
	var called DecodeIssue
	ctx := ContextWithDecodeIssueReporter(context.Background(), func(issue DecodeIssue) {
		called = issue
	})
	want := DecodeIssue{Module: "m", OID: "1.2.3", Reason: "invalid_type", Count: 2}
	reportDecodeIssue(ctx, want)
	if called != want {
		t.Errorf("reporter called with %+v, want %+v", called, want)
	}
}

// WalkToIntMapStrict: PDU whose OID name does not share the walked prefix
// increments TrimFailures and is excluded from the result map.
//
// Strategy: register a scalar PDU at the root OID (e.g. ".1.2.3") and a
// sibling PDU at ".1.2.4.1" to push the first BulkGet response out of the
// walked subtree. gosnmp then falls back to a GetRequest for ".1.2.3" itself
// and includes the returned PDU in the walk results. TrimOIDPrefix(".1.2.3",
// ".1.2.3.") returns false, incrementing TrimFailures.
func TestWalkToIntMapStrictTrimFailure(t *testing.T) {
	const oid = "1.2.3"
	pdus := []gsnmp.SnmpPDU{
		// Scalar leaf at the root OID — GetRequest fallback will return this.
		{Name: "." + oid, Type: gsnmp.Integer, Value: 99},
		// Sibling outside the subtree — causes the first BulkGet response to be
		// out of range, triggering gosnmp's GetRequest fallback for the root.
		{Name: ".1.2.4.1", Type: gsnmp.Integer, Value: 1},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	client, err := Open(Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	m, stats, err := WalkToIntMapStrict(context.Background(), client, "test", oid)
	if err != nil {
		t.Fatalf("WalkToIntMapStrict: %v", err)
	}
	if stats.TrimFailures == 0 {
		t.Errorf("TrimFailures = 0, want > 0; stats = %+v", stats)
	}
	// The mismatched PDU must not appear in the result map.
	if _, ok := m[oid]; ok {
		t.Errorf("result map unexpectedly contains OID key %q: %v", oid, m)
	}
}

// WalkToIntMap: cancelled context propagates the error from WalkToIntMapStrict.
func TestWalkToIntMapError(t *testing.T) {
	addr := snmptest.Start(t, "public", nil)
	ip, port := snmptest.ParseAddr(addr)
	client, err := Open(Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so BulkWalk returns immediately with an error

	m, err := WalkToIntMap(ctx, client, "1.3.6.1.2.1.138.1.6.1.1.2")
	if err == nil {
		t.Error("expected error from WalkToIntMap with cancelled context, got nil")
	}
	if m != nil {
		t.Errorf("expected nil map on error, got %v", m)
	}
}

// Walk: wrong community causes a timeout and returns a non-nil error.
// Real devices silently drop packets from unknown communities; our test agent
// does the same. The client exhausts its retries and Walk returns an error.
func TestWalkWrongCommunity(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.5.0", Type: gsnmp.OctetString, Value: []byte("sw-01")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{
		IP:        ip,
		Port:      port,
		Community: []byte("wrong"),
		Timeout:   200 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	dev, err := Walk(ctx, p)
	if err == nil {
		t.Fatalf("expected error for wrong community, got device %+v", dev)
	}
	if dev != nil {
		t.Errorf("expected nil device on auth failure, got %+v", dev)
	}
}

// NormaliseName: a 256-byte ASCII-only string is truncated to exactly 255 bytes.
func TestNormaliseNameCap(t *testing.T) {
	input := strings.Repeat("a", 256)
	got := NormaliseName(input)
	if len(got) != 255 {
		t.Errorf("NormaliseName(256-byte ASCII string) len = %d, want 255", len(got))
	}
}

// NormaliseName: truncation retreats to a rune boundary so the result is
// always valid UTF-8.
//
// Case 1: 254 ASCII bytes + one 3-byte UTF-8 rune = 257 bytes total.
// Byte 255 would be the second byte of the rune (a continuation byte), so the
// result must be 254 bytes, not 255.
//
// Case 2: 256 ASCII bytes truncates cleanly to 255 (no retreat needed).
func TestNormaliseNameRuneBoundary(t *testing.T) {
	// "é" in UTF-8 is 0xC3 0xA9 — 2 bytes; "€" is 0xE2 0x82 0xAC — 3 bytes.
	// Build a string of 254 ASCII 'a' bytes followed by the 3-byte euro sign.
	// Total = 257 bytes. Byte index 255 is 0x82 (continuation byte of '€').
	threeByteRune := "€"                               // U+20AC, encoded as 0xE2 0x82 0xAC
	input1 := strings.Repeat("a", 254) + threeByteRune // 254 + 3 = 257 bytes
	got1 := NormaliseName(input1)
	if len(got1) != 254 {
		t.Errorf("rune-boundary case: len = %d, want 254", len(got1))
	}

	// Case 2: 256 ASCII bytes → 255 (no continuation bytes involved).
	input2 := strings.Repeat("b", 256)
	got2 := NormaliseName(input2)
	if len(got2) != 255 {
		t.Errorf("ascii-only case: len = %d, want 255", len(got2))
	}
}

// NormaliseName: embedded control characters are removed to ensure device ID
// stability across polling cycles. Some SNMP devices embed \r, \x01, or other
// control characters in sysName responses; these must not survive into device IDs.
func TestNormaliseNameStripsControlChars(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		// \r embedded in sysName (e.g. "router1\rgarbage" from a buggy device).
		{"embedded CR", "router1\rgarbage", "router1garbage"},
		// SOH control character (\x01).
		{"embedded SOH", "spine\x01-01", "spine-01"},
		// NUL in the middle (PDUString only strips trailing NULs; NormaliseName
		// must strip interior ones too).
		{"embedded NUL in middle", "sw\x00-core", "sw-core"},
		// Normal names are unaffected.
		{"normal lowercase", "core-sw-01", "core-sw-01"},
		{"normal with spaces trimmed", "  Leaf-02  ", "leaf-02"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormaliseName(c.input)
			if got != c.want {
				t.Errorf("NormaliseName(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// NormaliseNamePreserveCase: same cleanup as NormaliseName (control-char
// strip, trim, 255-byte truncation) but case is preserved — this is the
// specific difference the join against snmp_exporter's own sysName label
// depends on (docs/proposals/snmp-exporter-label-alignment.md §5.2).
func TestNormaliseNamePreserveCase(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"mixed case preserved", "Sw-Core-01", "Sw-Core-01"},
		{"embedded CR stripped, case preserved", "Router1\rGarbage", "Router1Garbage"},
		{"trimmed, case preserved", "  Leaf-02  ", "Leaf-02"},
		{"already lowercase", "core-sw-01", "core-sw-01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormaliseNamePreserveCase(c.input)
			if got != c.want {
				t.Errorf("NormaliseNamePreserveCase(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// NormaliseNamePreserveCase: truncation behaves identically to NormaliseName
// (255-byte cap, retreating to a UTF-8 rune boundary) — case is the only
// difference between the two functions.
func TestNormaliseNamePreserveCaseCap(t *testing.T) {
	input := strings.Repeat("A", 256)
	got := NormaliseNamePreserveCase(input)
	if len(got) != 255 {
		t.Errorf("NormaliseNamePreserveCase(256-byte ASCII string) len = %d, want 255", len(got))
	}
	if got != strings.Repeat("A", 255) {
		t.Error("NormaliseNamePreserveCase lowercased or altered the truncated result")
	}
}

// SanitisePortName: 256-byte ASCII string is capped to 255.
func TestSanitisePortNameCap(t *testing.T) {
	input := strings.Repeat("a", 256)
	got := SanitisePortName(input)
	if len(got) != 255 {
		t.Errorf("SanitisePortName(256-byte ASCII) len = %d, want 255", len(got))
	}
}

// SanitisePortName: truncation retreats to a rune boundary so the result is
// always valid UTF-8 even when a multi-byte rune straddles the 255-byte mark.
func TestSanitisePortNameRuneBoundary(t *testing.T) {
	threeByteRune := "€"                              // U+20AC, encoded as 0xE2 0x82 0xAC
	input := strings.Repeat("a", 254) + threeByteRune // 254 + 3 = 257 bytes
	got := SanitisePortName(input)
	if len(got) != 254 {
		t.Errorf("SanitisePortName(rune-boundary input) len = %d, want 254", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("SanitisePortName produced invalid UTF-8: %q", got)
	}
}

// SanitisePortName: control characters are stripped (mirrors NormaliseName)
// because they otherwise cause edge-key instability across polling cycles.
func TestSanitisePortNameStripsControlChars(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"embedded CR", "Gi0/1\rgarbage", "Gi0/1garbage"},
		{"embedded SOH", "Eth\x011/1", "Eth1/1"},
		{"embedded NUL middle", "xe-\x000/0/0", "xe-0/0/0"},
		// Unlike NormaliseName, SanitisePortName must NOT lowercase or trim:
		// port names are case-sensitive in some operator expectations and
		// surrounding whitespace is graph.NormalizePortName's job, not ours.
		{"case preserved", "GigabitEthernet0/1", "GigabitEthernet0/1"},
		{"whitespace preserved", "  Gi0/1  ", "  Gi0/1  "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitisePortName(c.input)
			if got != c.want {
				t.Errorf("SanitisePortName(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// WalkARPTable: two ipNetToMediaPhysAddress PDUs are decoded to MAC→IP entries.
func TestWalkARPTable(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{
			Name:  "." + OIDIPNetToMediaPhysAddr + ".1.10.0.0.1",
			Type:  gsnmp.OctetString,
			Value: []byte{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e},
		},
		{
			Name:  "." + OIDIPNetToMediaPhysAddr + ".2.10.0.0.2",
			Type:  gsnmp.OctetString,
			Value: []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)
	client, err := Open(Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	m, err := WalkARPTable(context.Background(), client)
	if err != nil {
		t.Fatalf("WalkARPTable: %v", err)
	}
	if got := m["00:1a:2b:3c:4d:5e"]; got != "10.0.0.1" {
		t.Errorf("m[00:1a:2b:3c:4d:5e] = %q, want 10.0.0.1", got)
	}
	if got := m["aa:bb:cc:dd:ee:ff"]; got != "10.0.0.2" {
		t.Errorf("m[aa:bb:cc:dd:ee:ff] = %q, want 10.0.0.2", got)
	}
}

// ParseCIDRsStrict: a malformed CIDR entry returns a non-nil error.
func TestParseStrictRejectsInvalid(t *testing.T) {
	_, err := ParseCIDRsStrict([]string{"not-a-cidr"})
	if err == nil {
		t.Error("ParseCIDRsStrict(invalid) expected error, got nil")
	}
}

// ParseCIDRsStrict: all valid CIDRs return no error and the correct count.
func TestParseStrictAcceptsValid(t *testing.T) {
	nets, err := ParseCIDRsStrict([]string{"10.0.0.0/8", "192.168.1.0/24"})
	if err != nil {
		t.Fatalf("ParseCIDRsStrict(valid) unexpected error: %v", err)
	}
	if len(nets) != 2 {
		t.Errorf("ParseCIDRsStrict returned %d nets, want 2", len(nets))
	}
}

// ContextWithRateLimiter: a nil limiter returns the original context unchanged
// (the default / unlimited path).
func TestContextWithRateLimiterNil(t *testing.T) {
	ctx := context.Background()
	if got := ContextWithRateLimiter(ctx, nil); got != ctx {
		t.Error("ContextWithRateLimiter(nil) should return the original context")
	}
	if rateLimiterFrom(ctx) != nil {
		t.Error("rateLimiterFrom should be nil on a bare context")
	}
}

// ContextWithRateLimiter: BulkWalk paces throughput to the configured rate.
// We drive several real BulkWalks through the ctx seam at a known low rate and
// assert the total elapsed time is consistent with the limiter biting. Burst
// equals the rate (mirroring cycle.go), so the first `rate` calls are free and
// each subsequent call costs ~1/rate. Tolerances are generous and durations
// short to keep the timing assertion non-flaky.
func TestBulkWalkRateLimiterPacesThroughput(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("desc")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	// Rate 5/s, burst 1: the bucket starts with 1 token, so the first walk is
	// free and each of the next N-1 walks waits ~200ms. 4 paced waits ≈ 800ms.
	const calls = 5
	lim := rate.NewLimiter(rate.Limit(5), 1)
	var totalWait time.Duration
	ctx := ContextWithRateLimiter(context.Background(), lim)
	ctx = ContextWithRateLimitWaitObserver(ctx, func(d time.Duration) { totalWait += d })

	start := time.Now()
	for i := 0; i < calls; i++ {
		if _, err := BulkWalk(ctx, client, "1.3.6.1.2.1.1"); err != nil {
			t.Fatalf("BulkWalk #%d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// (calls-1) paced waits at 1/5s = 800ms expected. Lower bound guards that
	// the limiter actually held; we deliberately omit a tight upper bound to
	// avoid flakiness under load.
	const wantMin = 600 * time.Millisecond
	if elapsed < wantMin {
		t.Errorf("elapsed %v < %v: rate limiter did not pace throughput", elapsed, wantMin)
	}
	if totalWait <= 0 {
		t.Error("expected non-zero observed wait time from the rate-limit observer")
	}
}

// ContextWithRateLimiter: a cancelled context interrupts a waiting limit-block
// promptly and the walk is NOT issued. We set an effectively-infinite delay
// (rate so low the second token never arrives in-test) and a deadline already
// in the past, then assert Wait returns an error fast. A counting observer
// proves the wait callback is not fired on the error path.
func TestBulkWalkRateLimiterCtxCancel(t *testing.T) {
	pdus := []gsnmp.SnmpPDU{
		{Name: ".1.3.6.1.2.1.1.1.0", Type: gsnmp.OctetString, Value: []byte("desc")},
	}
	addr := snmptest.Start(t, "public", pdus)
	ip, port := snmptest.ParseAddr(addr)

	p := Params{IP: ip, Port: port, Community: []byte("public"), Timeout: 3 * time.Second}
	client, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Conn.Close() }()

	// Rate of ~1 token/hour, burst 0: no token is ever immediately available,
	// so Wait must block — until the context deadline (already expired) trips.
	lim := rate.NewLimiter(rate.Limit(1.0/3600.0), 0)
	observed := false
	ctx, cancel := context.WithCancel(context.Background())
	ctx = ContextWithRateLimiter(ctx, lim)
	ctx = ContextWithRateLimitWaitObserver(ctx, func(time.Duration) { observed = true })
	cancel() // already cancelled before the call

	start := time.Now()
	_, err = BulkWalk(ctx, client, "1.3.6.1.2.1.1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled-context rate-limit wait, got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Wait took %v to return after cancel, want prompt return", elapsed)
	}
	if observed {
		t.Error("rate-limit wait observer should not fire when Wait returns an error")
	}
}

// ContextWithPanicReporter: a nil reporter returns the original context
// unchanged and panicReporterFrom is nil on a bare context (the default —
// recover still happens, only the counter bump is skipped).
func TestContextWithPanicReporterNil(t *testing.T) {
	ctx := context.Background()
	if got := ContextWithPanicReporter(ctx, nil); got != ctx {
		t.Error("ContextWithPanicReporter(nil) should return the original context")
	}
	if panicReporterFrom(ctx) != nil {
		t.Error("panicReporterFrom should be nil on a bare context")
	}
}

// reportWalkPanic: recovers a panic, reports it under site "snmp_walk" through
// the injected reporter, and returns a sentinel error (does NOT re-panic).
func TestReportWalkPanicReportsAndReturnsSentinel(t *testing.T) {
	var gotSite string
	var calls int
	ctx := ContextWithPanicReporter(context.Background(), func(site string) {
		gotSite = site
		calls++
	})

	err := reportWalkPanic(ctx, "boom")
	if err == nil {
		t.Fatal("reportWalkPanic should return a non-nil sentinel error")
	}
	if !strings.Contains(err.Error(), "snmp walk panic") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("sentinel error = %q, want it to mention the panic value", err.Error())
	}
	if calls != 1 {
		t.Errorf("reporter called %d times, want 1", calls)
	}
	if gotSite != "snmp_walk" {
		t.Errorf("reporter site = %q, want %q", gotSite, "snmp_walk")
	}
}

// reportWalkPanic: the nil-reporter path is safe — it still returns a sentinel
// error and does not panic when no reporter is installed on the context.
func TestReportWalkPanicNilReporterSafe(t *testing.T) {
	err := reportWalkPanic(context.Background(), "boom")
	if err == nil {
		t.Fatal("reportWalkPanic should return a sentinel error even with no reporter")
	}
}

// The transport-goroutine recover pattern: a panic inside a goroutine body
// that mirrors BulkWalk's BulkWalkAll/WalkAll goroutines is recovered, the
// reporter is bumped, and a sentinel error arrives on the result channel
// instead of crashing the process.
func TestTransportGoroutineRecoverDeliversSentinel(t *testing.T) {
	var gotSite string
	ctx := ContextWithPanicReporter(context.Background(), func(site string) {
		gotSite = site
	})

	type walkResult struct {
		pdus []gsnmp.SnmpPDU
		err  error
	}
	ch := make(chan walkResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- walkResult{nil, reportWalkPanic(ctx, r)}
			}
		}()
		panic("hostile PDU decode")
	}()

	r := <-ch
	if r.err == nil {
		t.Fatal("recovered transport panic should deliver a sentinel error on the channel")
	}
	if !strings.Contains(r.err.Error(), "snmp walk panic") {
		t.Errorf("channel error = %q, want snmp walk panic sentinel", r.err.Error())
	}
	if gotSite != "snmp_walk" {
		t.Errorf("reporter site = %q, want %q", gotSite, "snmp_walk")
	}
}
