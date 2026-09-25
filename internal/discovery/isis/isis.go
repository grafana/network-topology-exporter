// Package isis infers L3 topology from IS-IS adjacency tables.
//
// Invariants:
//   - Only adjacency state up(3) emits edges.
//   - The isisISAdjIPAddr suffix encodes the address family in two leading octets
//     (InetAddressType + length, RFC 4444). IPv6 rows (type=2, len=16) are
//     skipped with an INFO log; IPv4 rows (type=1, len=4) drop the 6-octet tail
//     to derive adjKey, then look up the adjacency state. IPv6 skips do not
//     degrade any emitted IPv4 edges — both families are validly observable on
//     the same device.
//   - adjState decode errors are hard-fail (required signal).
//   - circuit/ifDescr joins are optional; failures degrade SrcPort enrichment.
package isis

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	gsnmp "github.com/gosnmp/gosnmp"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	snmputil "github.com/grafana/network-topology-exporter/internal/discovery/snmp"
)

const (
	oidISISAdjState         = "1.3.6.1.2.1.138.1.6.1.1.2"
	oidISISAdjIPAddr        = "1.3.6.1.2.1.138.1.6.2.1.2"
	oidISISCircIfIndex      = "1.3.6.1.2.1.138.1.4.1.1.3"
	requiredMinValidRows    = 0
	requiredMaxInvalidRatio = 0.50
	// precedenceRank 5: IS-IS ranked above OSPF (6) because it is more commonly
	// the primary IGP on service-provider networks and carries richer TE data.
	// Ladder: LLDP=2, CDP=3, FDB=4, IS-IS=5, OSPF=6, BGP=7, MPLS-TE=8.
	precedenceRank = 5
	isisAdjStateUp = 3
)

// Walk returns IS-IS adjacency edges for the device at p.IP. Only adjacencies
// in state up(3) produce edges. Neighbours outside allowedNets go to the
// OutOfScopeNeighbour slice; pass nil to skip scope enforcement.
func Walk(ctx context.Context, p snmputil.Params, localDevice string, allowedNets []*net.IPNet) (_ []discovery.Edge, _ []discovery.OutOfScopeNeighbour, retErr error) {
	client, release, err := snmputil.Acquire(p)
	if err != nil {
		return nil, nil, fmt.Errorf("isis %s: %w", p.IP, err)
	}
	// release sees the walk's final error so the pool can evict the session on
	// connection-level failures (#164).
	defer func() { release(retErr) }()

	states, stateDegraded, err := walkAdjStates(ctx, client)
	if err != nil {
		return nil, nil, fmt.Errorf("isis adjState %s: %w", p.IP, err)
	}

	var circIfs map[string]circuitIf
	var degradedReasons []string
	if stateDegraded {
		degradedReasons = append(degradedReasons, discovery.DegradedReasonRequiredTablePartialDecode)
	}
	if len(states) > 0 {
		var srcPortDegradedReason string
		circIfs, srcPortDegradedReason, err = walkCircuitIfNames(ctx, client)
		if err != nil {
			slog.Debug("isis: circuit ifName walk failed; SrcPort will be empty", "device", p.IP, "err", err)
			degradedReasons = append(degradedReasons, discovery.DegradedReasonMissingSrcPortMapping)
		} else if srcPortDegradedReason != "" {
			degradedReasons = append(degradedReasons, srcPortDegradedReason)
		}
	}

	edges, oos, sawIPv6, err := walkAdjIPAddrs(ctx, client, localDevice, states, circIfs, discovery.JoinReasonCodes(degradedReasons), allowedNets)
	if err != nil {
		return nil, nil, fmt.Errorf("isis adjIPAddr %s: %w", p.IP, err)
	}
	// Report skipped IPv6 adjacencies via the direct module sink rather than
	// edge metadata: an IPv6-only device produces zero IPv4 edges, so there
	// would be nothing to stamp (same zero-edge problem as FDB, issue #100).
	// Once per walk regardless of how many IPv6 rows were skipped.
	if sawIPv6 {
		snmputil.RecordDegraded(&p, "isis", discovery.DegradedReasonUnsupportedIPVersion)
	}
	return edges, oos, nil
}

func walkAdjStates(ctx context.Context, client *gsnmp.GoSNMP) (map[string]int, bool, error) {
	states, stats, err := snmputil.WalkToIntMapStrict(ctx, client, "isis", oidISISAdjState)
	if err != nil {
		return nil, false, err
	}
	verdict := snmputil.EvaluateRequiredTablePolicy(stats, snmputil.RequiredTablePolicy{
		MinValidRows:    requiredMinValidRows,
		MaxInvalidRatio: requiredMaxInvalidRatio,
	})
	if verdict.IsHardFail() {
		return nil, false, &discovery.PolicyError{
			Module: "isis",
			Reason: verdict.Reason,
			Err:    fmt.Errorf("adjState stats: valid=%d total=%d invalid=%d ratio=%.3f", stats.ValidRows, stats.TotalRows, stats.InvalidRows, stats.InvalidRatio),
		}
	}
	return states, verdict.IsDegraded(), nil
}

// circuitIf is one IS-IS circuit's resolved local interface: its name (used
// as SrcPort) and the underlying ifIndex (used as SrcIfIndex) — always
// resolved together from the same isisISCircIfIndex/ifDescr join, so they're
// kept as one struct rather than two maps that would otherwise have to be
// looked up in lockstep.
type circuitIf struct {
	name    string
	ifIndex int
}

// walkCircuitIfNames returns a map from "{sysInst}.{circIdx}" to the
// resolved local interface, built by joining isisISCircIfIndex (circuit →
// ifIndex) with ifDescr (ifIndex → interface name).
func walkCircuitIfNames(ctx context.Context, client *gsnmp.GoSNMP) (map[string]circuitIf, string, error) {
	circIfIndex, stats, err := snmputil.WalkToIntMapStrict(ctx, client, "isis", oidISISCircIfIndex)
	if err != nil {
		return nil, "", err
	}
	degradedReason := ""
	if stats.InvalidRows > 0 {
		degradedReason = discovery.DegradedReasonMissingSrcPortMapping
	}
	ifNames, err := snmputil.WalkIfDescr(ctx, client)
	if err != nil {
		return nil, "", err
	}
	result := make(map[string]circuitIf, len(circIfIndex))
	for key, ifIdx := range circIfIndex {
		if name, ok := ifNames[ifIdx]; ok {
			result[key] = circuitIf{name: name, ifIndex: ifIdx}
		}
	}
	return result, degradedReason, nil
}

func walkAdjIPAddrs(ctx context.Context, client *gsnmp.GoSNMP, localDevice string, states map[string]int, circIfs map[string]circuitIf, degradedReason string, allowedNets []*net.IPNet) ([]discovery.Edge, []discovery.OutOfScopeNeighbour, bool, error) {
	pdus, err := snmputil.BulkWalk(ctx, client, oidISISAdjIPAddr)
	if err != nil {
		return nil, nil, false, err
	}
	const prefix = "." + oidISISAdjIPAddr + "."
	now := time.Now()
	var edges []discovery.Edge
	var oos []discovery.OutOfScopeNeighbour
	loggedIPv6Skip := false
	sawIPv6 := false

	for _, pdu := range pdus {
		suffix, ok := snmputil.TrimOIDPrefix(pdu.Name, prefix)
		if !ok {
			continue
		}
		parts := strings.Split(suffix, ".")
		const ipv4TailLen = 6
		const ipv6TailLen = 18 // ipSubType(1) + ipLen(1) + 16 addr octets
		if len(parts) > ipv6TailLen &&
			parts[len(parts)-ipv6TailLen] == "2" &&
			parts[len(parts)-ipv6TailLen+1] == "16" {
			sawIPv6 = true
			if !loggedIPv6Skip {
				adjKey := strings.Join(parts[:len(parts)-ipv6TailLen], ".")
				slog.Info("isis: IPv6 adjacency skipped (IPv6 edges not yet supported); IPv4 edges on this device are unaffected", "device", localDevice, "adj_key", adjKey)
				loggedIPv6Skip = true
			}
			continue
		}
		if len(parts) <= ipv4TailLen || parts[len(parts)-ipv4TailLen] != "1" || parts[len(parts)-ipv4TailLen+1] != "4" {
			continue
		}
		adjKey := strings.Join(parts[:len(parts)-ipv4TailLen], ".")
		state, known := states[adjKey]
		if !known || state != isisAdjStateUp {
			continue
		}
		ip := snmputil.PDUIPv4(pdu)
		if ip == nil {
			continue
		}
		if ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
			continue
		}
		// Derive circuit key: adjKey is "{sysInst}.{circIdx}.{adjIdx}"; circuit
		// key is the first two components.
		adjParts := strings.SplitN(adjKey, ".", 3)
		var circKey string
		if len(adjParts) >= 2 {
			circKey = adjParts[0] + "." + adjParts[1]
		} else {
			slog.Debug("isis: malformed adjKey, SrcPort will be empty", "adj_key", adjKey)
		}
		resolved := circIfs[circKey]
		edgeDegradedReason := degradedReason
		if edgeDegradedReason == "" && (circKey == "" || resolved.name == "") {
			edgeDegradedReason = discovery.DegradedReasonMissingSrcPortMapping
		}
		if snmputil.OutOfScope(ip, allowedNets) {
			oos = append(oos, snmputil.NewOutOfScopeNeighbour("isis", localDevice, resolved.name, ip.String(), now))
			continue
		}
		edges = append(edges, discovery.Edge{
			SrcDevice:      localDevice,
			SrcPort:        resolved.name,
			SrcIfIndex:     resolved.ifIndex,
			DstDevice:      ip.String(),
			DiscoveryProto: discovery.DiscoveryProtocolISIS,
			Direction:      discovery.DirectionUnidirectional,
			Confidence:     discovery.ConfidenceMedium,
			Adjacency:      discovery.AdjacencyDirect,
			PrecedenceRank: precedenceRank,
			LinkKind:       discovery.LinkKindIP,
			ObservedAt:     now,
			Metadata:       isisMetadata(edgeDegradedReason),
		})
	}
	return edges, oos, sawIPv6, nil
}

func isisMetadata(degradedReason string) map[string]string {
	if degradedReason == "" {
		return nil
	}
	return map[string]string{
		discovery.MetadataKeyDegraded:       "true",
		discovery.MetadataKeyDegradedReason: degradedReason,
	}
}
