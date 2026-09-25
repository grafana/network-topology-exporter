package snmp

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"unicode"

	g "github.com/gosnmp/gosnmp"

	"github.com/grafana/network-topology-exporter/internal/sanitize"
)

const (
	// oidIfNameTable is the IF-MIB ifXTable.ifName column (RFC 2863 §3.1.4).
	oidIfNameTable = "1.3.6.1.2.1.31.1.1.1.1"
	// OIDIfDescr is the IF-MIB ifTable.ifDescr column (RFC 2863 §3.1.2).
	// Older devices that lack the ifXTable ifName column use this OID instead.
	OIDIfDescr = "1.3.6.1.2.1.2.2.1.2"
	// OIDIPNetToMediaPhysAddr is the ipNetToMediaTable.ipNetToMediaPhysAddress
	// column (RFC 1213 §4.2.2): a map of ifIndex.remoteIP → MAC address.
	OIDIPNetToMediaPhysAddr = "1.3.6.1.2.1.4.22.1.2"
)

// TableOID is a validated SNMP table-root OID string. Using a named type
// prevents per-PDU instance OIDs (which include the row suffix) from being
// accidentally passed as a metric label, which would create unbounded cardinality.
type TableOID string

// DecodeIssue describes a decode anomaly observed while parsing SNMP table PDUs.
// It is emitted through SetDecodeIssueObserver for metrics/logging aggregation.
type DecodeIssue struct {
	Module string
	OID    TableOID // always a table-root OID, never a per-row instance OID
	Reason string
	Count  int
}

// IntMapDecodeStats reports anomalies from WalkToIntMapStrict.
type IntMapDecodeStats struct {
	TotalRows      int
	ValidRows      int
	InvalidRows    int
	InvalidRatio   float64
	DecodeFailures int
	TrimFailures   int
	Samples        []string
}

type decodeIssueReporterKey struct{}

// ContextWithDecodeIssueReporter returns a child context with a decode issue reporter.
func ContextWithDecodeIssueReporter(ctx context.Context, fn func(DecodeIssue)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, decodeIssueReporterKey{}, fn)
}

func reportDecodeIssue(ctx context.Context, issue DecodeIssue) {
	fn, _ := ctx.Value(decodeIssueReporterKey{}).(func(DecodeIssue))
	if fn != nil {
		fn(issue)
	}
}

// ReportDecodeIssue surfaces a single per-row decode rejection to the decode
// issue reporter installed on ctx (via ContextWithDecodeIssueReporter), if any.
// It is the exported entry point for walker packages outside this package
// (lldp/cdp/ospf/fdb) that cannot call the unexported reportDecodeIssue.
//
// oid MUST be a table-root OID (never a per-row instance OID) to keep the
// resulting metric label low-cardinality; reason should be a short, stable
// snake_case string suitable as a metric label value. count is the number of
// rejected rows being reported (use 1 per row at a continue site).
func ReportDecodeIssue(ctx context.Context, module string, oid TableOID, reason string, count int) {
	reportDecodeIssue(ctx, DecodeIssue{
		Module: module,
		OID:    oid,
		Reason: reason,
		Count:  count,
	})
}

func appendDecodeSample(samples []string, sample string) []string {
	const maxSamples = 5
	if len(samples) >= maxSamples {
		return samples
	}
	return append(samples, sample)
}

// RequiredTablePolicy defines hard-fail thresholds for required tables.
type RequiredTablePolicy struct {
	MinValidRows    int
	MaxInvalidRatio float64
}

// WalkToIntMapStrict walks oid and returns a map from OID suffix to integer
// value plus decode stats. Non-integer PDU values are counted as decode
// failures and excluded from the result map.
func WalkToIntMapStrict(ctx context.Context, client *g.GoSNMP, module, oid string) (map[string]int, IntMapDecodeStats, error) {
	pdus, err := BulkWalk(ctx, client, oid)
	if err != nil {
		return nil, IntMapDecodeStats{}, err
	}
	prefix := "." + oid + "."
	result := make(map[string]int, len(pdus))
	stats := IntMapDecodeStats{}
	for _, pdu := range pdus {
		key, ok := TrimOIDPrefix(pdu.Name, prefix)
		if !ok {
			stats.TrimFailures++
			stats.Samples = appendDecodeSample(stats.Samples, "trim:"+pdu.Name)
			continue
		}
		v, ok := PDUIntStrict(pdu)
		if !ok {
			stats.DecodeFailures++
			stats.Samples = appendDecodeSample(stats.Samples, fmt.Sprintf("decode:%s:%T", key, pdu.Value))
			continue
		}
		result[key] = v
	}
	stats.TotalRows = len(pdus)
	stats.ValidRows = len(result)
	stats.InvalidRows = stats.DecodeFailures + stats.TrimFailures
	if stats.TotalRows > 0 {
		stats.InvalidRatio = float64(stats.InvalidRows) / float64(stats.TotalRows)
	}
	if module != "" {
		if stats.DecodeFailures > 0 {
			reportDecodeIssue(ctx, DecodeIssue{
				Module: module,
				OID:    TableOID(oid),
				Reason: "invalid_type",
				Count:  stats.DecodeFailures,
			})
		}
		if stats.TrimFailures > 0 {
			reportDecodeIssue(ctx, DecodeIssue{
				Module: module,
				OID:    TableOID(oid),
				Reason: "invalid_oid",
				Count:  stats.TrimFailures,
			})
		}
	}
	return result, stats, nil
}

// WalkToIntMap walks oid and returns a map from OID suffix to integer value.
// The suffix is the instance index portion of the OID (everything after the
// column prefix). Returns nil on walk error.
func WalkToIntMap(ctx context.Context, client *g.GoSNMP, oid string) (map[string]int, error) {
	m, _, err := WalkToIntMapStrict(ctx, client, "", oid)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// TableOutcome is the three-state result of EvaluateRequiredTablePolicy.
type TableOutcome int

// TableOK, TableDegraded, and TableHardFail are the possible TableOutcome values.
const (
	TableOK       TableOutcome = iota // usable, no invalid rows
	TableDegraded                     // usable, but some rows failed to decode
	TableHardFail                     // failed a required-table floor; see Reason
)

// TableVerdict is the result of EvaluateRequiredTablePolicy. Reason is set only
// when Outcome == TableHardFail.
type TableVerdict struct {
	Outcome TableOutcome
	Reason  string
}

// IsHardFail reports whether the table failed a required-table floor.
func (v TableVerdict) IsHardFail() bool { return v.Outcome == TableHardFail }

// IsDegraded reports whether the table is usable but had decode anomalies.
func (v TableVerdict) IsDegraded() bool { return v.Outcome == TableDegraded }

// EvaluateRequiredTablePolicy checks decode stats against the required table policy.
func EvaluateRequiredTablePolicy(stats IntMapDecodeStats, policy RequiredTablePolicy) TableVerdict {
	if stats.ValidRows < policy.MinValidRows {
		return TableVerdict{Outcome: TableHardFail, Reason: "required_table_no_valid_rows"}
	}
	if policy.MaxInvalidRatio >= 0 && stats.InvalidRatio > policy.MaxInvalidRatio {
		return TableVerdict{Outcome: TableHardFail, Reason: "required_table_invalid_ratio_exceeded"}
	}
	if stats.InvalidRows > 0 {
		return TableVerdict{Outcome: TableDegraded}
	}
	return TableVerdict{Outcome: TableOK}
}

// WalkIfNames walks the IF-MIB ifXTable.ifName column (RFC 2863 §3.1.4) and
// returns a map of ifIndex → ifName. Multiple discovery modules (CDP, FDB)
// require this mapping to translate bridge/CDP port numbers to interface names.
func WalkIfNames(ctx context.Context, client *g.GoSNMP) (map[int]string, error) {
	return walkIntIndexedStrings(ctx, client, oidIfNameTable)
}

// WalkIfDescr walks the IF-MIB ifTable.ifDescr column (RFC 2863 §3.1.2) and
// returns a map of ifIndex → ifDescr string.
func WalkIfDescr(ctx context.Context, client *g.GoSNMP) (map[int]string, error) {
	return walkIntIndexedStrings(ctx, client, OIDIfDescr)
}

// WalkIfNamesWithFallback walks ifXTable.ifName (RFC 2863 §3.1.4). If ifXTable
// is unavailable, falls back to ifTable.ifDescr (§3.1.2). ifDescr values are
// not guaranteed to be unique across module boundaries on chassis devices; use
// the returned values for edge identification only, not as stable identifiers.
// If both walks fail, returns an empty map and the ifName error.
func WalkIfNamesWithFallback(ctx context.Context, client *g.GoSNMP) (map[int]string, error) {
	names, err := WalkIfNames(ctx, client)
	if err == nil && len(names) > 0 {
		return names, nil
	}
	descr, descrErr := WalkIfDescr(ctx, client)
	if descrErr == nil && len(descr) > 0 {
		return descr, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, descrErr
}

// WalkARPTable walks the ipNetToMediaTable and returns a map of MAC address
// string → IP address string for all ARP entries on the device. Only valid
// 6-byte MAC entries with a parseable IPv4 suffix are included.
func WalkARPTable(ctx context.Context, client *g.GoSNMP) (map[string]string, error) {
	pdus, err := BulkWalk(ctx, client, OIDIPNetToMediaPhysAddr)
	if err != nil {
		return nil, err
	}
	// OID suffix format: ifIndex.a.b.c.d where a.b.c.d is the remote IPv4 address.
	prefix := "." + OIDIPNetToMediaPhysAddr + "."
	result := make(map[string]string, len(pdus))
	for _, pdu := range pdus {
		suffix, ok := TrimOIDPrefix(pdu.Name, prefix)
		if !ok {
			continue
		}
		// suffix is "ifIndex.a.b.c.d"; strip the ifIndex component.
		dot := strings.Index(suffix, ".")
		if dot < 0 {
			continue
		}
		ipStr := suffix[dot+1:] // "a.b.c.d"
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		var macBytes []byte
		switch v := pdu.Value.(type) {
		case []byte:
			macBytes = v
		case string:
			macBytes = []byte(v)
		}
		if len(macBytes) != 6 {
			continue
		}
		mac := net.HardwareAddr(macBytes).String()
		result[mac] = ip.String()
	}
	return result, nil
}

func walkIntIndexedStrings(ctx context.Context, client *g.GoSNMP, oid string) (map[int]string, error) {
	pdus, err := BulkWalk(ctx, client, oid)
	if err != nil {
		return nil, err
	}
	prefix := "." + oid + "."
	names := make(map[int]string, len(pdus))
	for _, pdu := range pdus {
		idxStr, ok := TrimOIDPrefix(pdu.Name, prefix)
		if !ok {
			continue
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		// SanitisePortName caps and control-strips: these values become
		// localPort in LLDP/CDP edge construction. Issue #13.
		names[idx] = SanitisePortName(PDUString(pdu))
	}
	return names, nil
}

// PDUString extracts a string value from a PDU. It handles both string and
// []byte value types, returning "" for any other type. Trailing NUL bytes
// (\x00) are stripped; some SNMP devices (e.g. certain Cisco IOS versions)
// terminate OctetString values with a NUL, which would otherwise cause device
// IDs to diverge from their non-NUL counterparts when used as graph keys.
func PDUString(pdu g.SnmpPDU) string {
	switch v := pdu.Value.(type) {
	case string:
		return strings.TrimRight(v, "\x00")
	case []byte:
		return strings.TrimRight(string(v), "\x00")
	}
	return ""
}

// PDUInt extracts an integer value from a PDU. It handles int, int32, uint,
// and uint32 value types, returning 0 for any other type.
func PDUInt(pdu g.SnmpPDU) int {
	v, ok := PDUIntStrict(pdu)
	if !ok {
		return 0
	}
	return v
}

// PDUIntStrict extracts an integer value from a PDU and reports whether
// extraction succeeded.
//
// Use this (paired with ReportDecodeIssue) instead of PDUInt at every site
// that decodes an enum-state column (#170): a lenient decode maps a corrupt
// or mistyped PDU to 0, which most MIB enums treat as "absent/down", so the
// row is silently dropped with no decode-issue accounting. Call sites only
// need a one-line comment naming what the silent 0 would have meant locally.
func PDUIntStrict(pdu g.SnmpPDU) (int, bool) {
	switch v := pdu.Value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		if v < math.MinInt || v > math.MaxInt {
			return 0, false
		}
		return int(v), true
	case uint:
		if uint64(v) > math.MaxInt {
			return 0, false
		}
		return int(v), true
	case uint32:
		if uint64(v) > math.MaxInt { // always false on 64-bit; correct on 32-bit
			return 0, false
		}
		return int(v), true
	case uint64:
		if v > math.MaxInt {
			return 0, false
		}
		return int(v), true
	}
	return 0, false
}

// PDUBytes extracts a byte slice from a PDU. It handles []byte and string
// value types, returning nil for any other type. The returned slice is always
// a fresh copy.
func PDUBytes(pdu g.SnmpPDU) []byte {
	switch v := pdu.Value.(type) {
	case []byte:
		return append([]byte(nil), v...)
	case string:
		return []byte(v)
	}
	return nil
}

// PDUIPv4 extracts an IPv4 address from an SNMP PDU. gosnmp may decode
// the value as a dotted-decimal string or as a raw 4-byte slice.
func PDUIPv4(pdu g.SnmpPDU) net.IP {
	switch v := pdu.Value.(type) {
	case string:
		if ip := net.ParseIP(v); ip != nil {
			return ip.To4()
		}
	case []byte:
		if len(v) == 4 {
			return net.IP(append([]byte(nil), v...))
		}
	}
	return nil
}

// SplitOIDComponent splits the leading numeric component from an OID suffix
// string of the form "col.rest" or "col" (last component). Returns the integer
// value, the remaining string after the dot, and true on success.
func SplitOIDComponent(s string) (int, string, bool) {
	i := strings.IndexByte(s, '.')
	if i < 0 {
		// last component
		v, err := strconv.Atoi(s)
		return v, "", err == nil
	}
	v, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, "", false
	}
	return v, s[i+1:], true
}

// TrimOIDPrefix strips prefix from s and returns the remainder. Returns
// ("", false) when s does not start with prefix or the remainder is empty.
func TrimOIDPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	rest := s[len(prefix):]
	if rest == "" {
		return "", false
	}
	return rest, true
}

// IPInNets reports whether ip is contained in any of the given networks.
func IPInNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IsCatchAll returns true if any CIDR in the list is 0.0.0.0/0 or ::/0.
func IsCatchAll(nets []*net.IPNet) bool {
	for _, n := range nets {
		ones, bits := n.Mask.Size()
		if ones == 0 && bits != 0 {
			return true
		}
	}
	return false
}

// stripControlAndTrim removes embedded control characters (NUL bytes are
// already handled by PDUString, but some devices embed \r, \x01, or similar
// in sysName responses) and trims surrounding whitespace — the cleanup
// shared by NormaliseName and NormaliseNamePreserveCase, ahead of each
// function's own lowercase-or-not step and truncation. Kept separate from
// truncation so each function truncates its own final form (NormaliseName
// after lowercasing, NormaliseNamePreserveCase without).
func stripControlAndTrim(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// NormaliseNamePreserveCase applies stripControlAndTrim and truncates to 255
// bytes on a UTF-8 boundary (RFC 1213 sysName is SIZE(0..255); capped here
// before the string becomes a map key, graph ID, or federation payload
// field). Case is preserved deliberately: this is also used for
// discovery.Device.SysName, the case-preserving join key against
// snmp_exporter's own sysName label — snmp_exporter passes the MIB value
// through in the device's original case (see
// docs/proposals/snmp-exporter-label-alignment.md §2), and PromQL has no
// case-folding function, so a lowercased value can never match it.
func NormaliseNamePreserveCase(s string) string {
	return sanitize.TruncateAtRuneBoundary(stripControlAndTrim(s), 255)
}

// NormaliseName is NormaliseNamePreserveCase's cleanup plus lowercasing,
// lowercased before truncating (not after) so a device name that changes
// byte length under case-folding still truncates the same as it always has.
// Used to normalise sysName / device-ID values from SNMP PDUs consistently
// across LLDP, CDP, and the SYSTEM group walker.
func NormaliseName(s string) string {
	return sanitize.TruncateAtRuneBoundary(strings.ToLower(stripControlAndTrim(s)), 255)
}

// SanitisePortName caps a port-name string at 255 bytes on a rune boundary
// and strips embedded control characters. Use at every site where a device
// PDU value flows into discovery.Edge.SrcPort, discovery.Edge.DstPort, or
// discovery.OutOfScopeNeighbour.ReportingPort.
//
// Unlike NormaliseName above, SanitisePortName preserves case and surrounding
// whitespace because port names are not normalised by the discovery layer —
// graph.NormalizePortName handles canonical-form collapse downstream during
// reconciliation. SanitisePortName is purely a boundary defence against
// non-conforming device PDUs.
//
// The 255-byte cap matches the MIB definitions for the inbound fields:
//
//   - IEEE 802.1AB-2016 lldpLocPortDesc / lldpRemPortDesc — SnmpAdminString
//     (RFC 3414) SIZE(0..255)
//   - CISCO-CDP-MIB cdpCacheDevicePort — OCTET STRING SIZE(0..255)
//   - RFC 2863 IF-MIB ifName / ifDescr — DisplayString SIZE(0..255)
//
// The hub's federation push validator (internal/federation/hub.go) enforces
// a 256-byte ceiling on the same fields; the one-byte gap is defensive
// headroom. Sanitising at discovery time prevents an oversized device PDU
// from silently failing the entire spoke push at the hub.
func SanitisePortName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return sanitize.TruncateAtRuneBoundary(s, 255)
}

// ParseCIDRs parses a slice of CIDR strings and returns only the valid IPNet
// values; malformed entries are silently skipped. Use ParseCIDRsStrict when
// the caller cannot tolerate silent drops (e.g. user-provided input that has
// not been pre-validated by config.Load).
func ParseCIDRs(cidrs []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, s := range cidrs {
		_, n, err := net.ParseCIDR(s)
		if err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}

// ParseCIDRsStrict parses cidrs and returns an error if any entry is malformed.
// Use this when the input has not been pre-validated by config.Load.
func ParseCIDRsStrict(cidrs []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, s := range cidrs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", s, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}
