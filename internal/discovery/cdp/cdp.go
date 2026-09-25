// Package cdp discovers Cisco-proprietary neighbour relationships via CDP.
//
// Specification sources:
//   - CISCO-CDP-MIB (vendor-published) — cdpCacheTable at
//     1.3.6.1.4.1.9.9.23.1.2.1.1. Index is (ifIndex, neighbourIndex).
//     cdpCacheDeviceId (.6), cdpCacheDevicePort (.7), cdpCachePlatform (.8),
//     cdpCacheAddress (.4) / cdpCacheAddressType (.3).
//   - RFC 2863 — IF-MIB ifXTable. ifName (1.3.6.1.2.1.31.1.1.1.1.{ifIndex})
//     maps the ifIndex from the CDP OID index to a human-readable port name.
//
// Design references:
//   - arXiv:1709.02209, 2017 — BFS via cdpCacheTable is equivalent to
//     lldpRemTable for Cisco-only environments.
//
// CDP is Cisco-proprietary; only Cisco devices respond. If LLDP is also
// enabled and both protocols report the same link, the graph layer ranks
// LLDP above CDP (lower PrecedenceRank) per the LD-10 ladder.
package cdp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	gsnmp "github.com/gosnmp/gosnmp"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	snmputil "github.com/grafana/network-topology-exporter/internal/discovery/snmp"
)

const (
	oidCDPCacheTable = "1.3.6.1.4.1.9.9.23.1.2.1"
	precedenceRank   = 3
)

// walkerCDP is the fixed walker label for network_topology_walker_outcome_total
// (issue #98). The outcome label values and the {walker, outcome} forwarder live
// in snmputil (snmputil.Outcome*, snmputil.RecordProtocolWalkerOutcome).
const walkerCDP = "cdp"

// cdpCacheTable column numbers (CISCO-CDP-MIB).
const (
	colAddressType = 3
	colAddress     = 4
	colDeviceID    = 6
	colDevicePort  = 7
)

type cacheKey struct{ ifIndex, neighIndex int }

type cacheEntry struct {
	addrType int
	addr     []byte
	deviceID string
	devPort  string
}

// Walk returns CDP-discovered edges for the device at p.IP. localDevice is
// the sysName from the SNMP SYSTEM walk.
func Walk(ctx context.Context, p snmputil.Params, localDevice string, allowedNets []*net.IPNet) (_ []discovery.Edge, _ []discovery.OutOfScopeNeighbour, retErr error) {
	client, release, err := snmputil.Acquire(p)
	if err != nil {
		snmputil.RecordProtocolWalkerOutcome(&p, walkerCDP, snmputil.OutcomeError)
		return nil, nil, fmt.Errorf("cdp %s: %w", p.IP, err)
	}
	// release sees the walk's final error so the pool can evict the session on
	// connection-level failures (#164).
	defer func() { release(retErr) }()

	ifNames, err := snmputil.WalkIfNamesWithFallback(ctx, client)
	if err != nil {
		snmputil.RecordProtocolWalkerOutcome(&p, walkerCDP, snmputil.OutcomeError)
		return nil, nil, fmt.Errorf("cdp ifname %s: %w", p.IP, err)
	}

	// cdpCacheTable is the base table for the outcome accounting: hadPDUs
	// distinguishes "MIB unimplemented" (zero PDUs — expected on non-Cisco
	// devices) from a device that does cache neighbours.
	entries, hadPDUs, err := walkCacheTable(ctx, client)
	if err != nil {
		snmputil.RecordProtocolWalkerOutcome(&p, walkerCDP, snmputil.OutcomeError)
		return nil, nil, fmt.Errorf("cdp cache %s: %w", p.IP, err)
	}

	edges, oos, decoded := buildEdges(ctx, localDevice, ifNames, entries, allowedNets)

	// Terminal outcome classification (edges / mib_unimplemented / no_neighbours
	// / walker_drift) lives in snmputil.ClassifyNeighbourOutcome, shared with the
	// LLDP/OSPF/FDB walkers.
	snmputil.RecordProtocolWalkerOutcome(&p, walkerCDP, snmputil.ClassifyNeighbourOutcome(len(edges), hadPDUs, decoded))

	return edges, oos, nil
}

func walkCacheTable(ctx context.Context, client *gsnmp.GoSNMP) (map[cacheKey]*cacheEntry, bool, error) {
	pdus, err := snmputil.BulkWalk(ctx, client, oidCDPCacheTable)
	if err != nil {
		return nil, false, err
	}
	hadPDUs := len(pdus) > 0

	const prefix = "." + oidCDPCacheTable + ".1."
	entries := make(map[cacheKey]*cacheEntry)
	for _, pdu := range pdus {
		suffix, ok := snmputil.TrimOIDPrefix(pdu.Name, prefix)
		if !ok {
			continue
		}
		// suffix: col.ifIndex.neighIndex
		col, rest, ok := snmputil.SplitOIDComponent(suffix)
		if !ok {
			snmputil.ReportDecodeIssue(ctx, walkerCDP, oidCDPCacheTable, "index_unparseable", 1)
			continue
		}
		ifIdx, neighStr, ok := snmputil.SplitOIDComponent(rest)
		if !ok {
			snmputil.ReportDecodeIssue(ctx, walkerCDP, oidCDPCacheTable, "index_unparseable", 1)
			continue
		}
		if ifIdx <= 0 {
			snmputil.ReportDecodeIssue(ctx, walkerCDP, oidCDPCacheTable, "index_unparseable", 1)
			continue
		}
		neighIdx, err := strconv.Atoi(neighStr)
		if err != nil {
			snmputil.ReportDecodeIssue(ctx, walkerCDP, oidCDPCacheTable, "index_unparseable", 1)
			continue
		}
		if neighIdx <= 0 {
			snmputil.ReportDecodeIssue(ctx, walkerCDP, oidCDPCacheTable, "index_unparseable", 1)
			continue
		}

		k := cacheKey{ifIdx, neighIdx}
		e := entries[k]
		if e == nil {
			e = &cacheEntry{}
			entries[k] = e
		}
		switch col {
		case colAddressType:
			e.addrType = snmputil.PDUInt(pdu)
		case colAddress:
			e.addr = snmputil.PDUBytes(pdu)
		case colDeviceID:
			e.deviceID = snmputil.NormaliseName(snmputil.PDUString(pdu))
		case colDevicePort:
			// SanitisePortName caps and control-strips the device-supplied
			// port name; well-behaved devices conform to CISCO-CDP-MIB's
			// OCTET STRING SIZE(0..255), but a non-conforming device would
			// otherwise silently fail the hub's federation push validator.
			// Issue #13.
			e.devPort = snmputil.SanitisePortName(snmputil.PDUString(pdu))
		}
	}
	return entries, hadPDUs, nil
}

// buildEdges returns (edges, oos, decoded). decoded reports whether at least
// one cache row decoded cleanly — i.e. yielded a non-empty neighbour device ID
// and port — regardless of whether it ultimately produced an in-scope edge.
// The Walk-level outcome accounting uses it to distinguish "no_neighbours"
// (rows decoded, nothing usable) from "walker_drift" (PDUs arrived but every
// row was decoder-rejected). See issue #98.
func buildEdges(ctx context.Context, localDevice string, ifNames map[int]string, entries map[cacheKey]*cacheEntry, allowedNets []*net.IPNet) ([]discovery.Edge, []discovery.OutOfScopeNeighbour, bool) {
	var edges []discovery.Edge
	var oos []discovery.OutOfScopeNeighbour
	var decoded bool
	now := time.Now()

	for k, e := range entries {
		if e.deviceID == "" || e.devPort == "" {
			snmputil.ReportDecodeIssue(ctx, walkerCDP, oidCDPCacheTable, "empty_device_id", 1)
			continue
		}
		// The row carries a usable neighbour identity: it decoded cleanly.
		// Anything that drops it below is a scope filter, not decoder drift.
		decoded = true

		localPort := ifNames[k.ifIndex]
		if localPort == "" {
			localPort = fmt.Sprintf("if%d", k.ifIndex)
		}

		// LD-11: cdpCacheAddress with addrType 1 (IPv4) gives the neighbor IP.
		remIP := cdpNeighborIP(e)
		if remIP == nil && len(allowedNets) > 0 && !snmputil.IsCatchAll(allowedNets) {
			// Cannot validate scope for non-IP neighbor; skip when scope filtering is active.
			slog.Debug("cdp: skipping non-IP neighbor when scope filtering is active",
				"device", localDevice, "neighbor", e.deviceID)
			continue
		}
		if remIP != nil && snmputil.OutOfScope(remIP, allowedNets) {
			oos = append(oos, snmputil.NewOutOfScopeNeighbour(
				string(discovery.DiscoveryProtocolCDP), localDevice, localPort, e.deviceID, now))
			continue
		}

		edges = append(edges, discovery.Edge{
			SrcDevice:      localDevice,
			SrcPort:        localPort,
			SrcIfIndex:     k.ifIndex,
			DstDevice:      e.deviceID,
			DstPort:        e.devPort,
			DiscoveryProto: discovery.DiscoveryProtocolCDP,
			Direction:      discovery.DirectionUnidirectional,
			Confidence:     discovery.ConfidenceHigh,
			Adjacency:      discovery.AdjacencyDirect,
			PrecedenceRank: precedenceRank,
			LinkKind:       discovery.LinkKindEthernet,
			ObservedAt:     now,
		})
	}

	return edges, oos, decoded
}

// cdpNeighborIP extracts the IP address from cdpCacheAddress. addrType 1 is
// IPv4 (4-byte addr); addrType 20 is IPv6 (16-byte addr) per CISCO-CDP-MIB.
// Returns nil for all other address types or unexpected lengths.
func cdpNeighborIP(e *cacheEntry) net.IP {
	switch {
	case e.addrType == 1 && len(e.addr) == 4:
		return append(net.IP{}, e.addr...)
	case e.addrType == 20 && len(e.addr) == 16:
		return append(net.IP{}, e.addr...)
	}
	return nil
}
