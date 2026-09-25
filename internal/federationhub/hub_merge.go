package federationhub

// Combined-graph construction: spoke registry snapshotting, cross-domain edge
// synthesis from OOS name-matching (LD-15/LD-19), known inter-domain link
// injection, and device-name normalisation. Split from hub.go (#168) —
// same-package move, no behaviour change.

import (
	"sort"
	"strings"
	"time"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/graph"
)

// spokesSnapshot returns a shallow copy of h.spokes. Caller must hold h.mu.
func (h *Hub) spokesSnapshot() map[string]spokeEntry {
	s := make(map[string]spokeEntry, len(h.spokes))
	for k, v := range h.spokes {
		s[k] = v
	}
	return s
}

// combinedGraphLocked is a convenience wrapper for tests that call it while
// already holding h.mu. Production paths use spokesSnapshot + buildCombinedGraph
// to move Reconcile outside the critical section.
func (h *Hub) combinedGraphLocked() discovery.Graph {
	g, _ := h.buildCombinedGraph(h.spokes)
	return g
}

// buildCombinedGraph constructs the unified discovery.Graph from the supplied
// spoke snapshot and the configured known inter-domain links. It runs a second
// graph.Reconcile pass so cross-boundary bidirectionality is detected at the
// hub level per LD-17. Does NOT require h.mu — call spokesSnapshot() under the
// lock first, then release h.mu before calling this function.
// The second return value is the count of unmatched OOS observations; callers
// should only publish this via HubOOSUnmatchedTotal after confirming the build
// wins in publishIfWinner.
func (h *Hub) buildCombinedGraph(spokes map[string]spokeEntry) (discovery.Graph, int) {
	var allDevices []discovery.Device
	seenDevices := make(map[string]bool)
	var allEdges []discovery.Edge

	spokeKeys := make([]string, 0, len(spokes))
	for k := range spokes {
		spokeKeys = append(spokeKeys, k)
	}
	sort.Strings(spokeKeys)

	for _, sk := range spokeKeys {
		entry := spokes[sk]
		for _, dev := range entry.payload.Devices {
			if !seenDevices[dev.ID] {
				seenDevices[dev.ID] = true
				allDevices = append(allDevices, dev)
			} else {
				h.logger.Warn("hub: duplicate device_id across spokes; using first alphabetically",
					"device_id", dev.ID, "spoke_id", sk)
			}
		}
		for _, e := range entry.payload.Edges {
			allEdges = append(allEdges, e)
			// A bidirectional edge in the spoke's pre-reconciled graph means
			// both sides were observed within that domain. Inject the reverse
			// observation so the hub's Reconcile pass also sees both sides and
			// preserves the bidirectional status.
			if e.Direction == discovery.DirectionBidirectional {
				rev := e
				rev.SrcDevice, rev.DstDevice = e.DstDevice, e.SrcDevice
				rev.SrcPort, rev.DstPort = e.DstPort, e.SrcPort
				allEdges = append(allEdges, rev)
			}
		}
	}

	// Cross-boundary edge detection via OOS name-matching (LD-15/LD-19
	// best-effort path). For each out-of-scope observation from spoke A
	// where NeighbourHint matches another spoke B's ReportingDevice, and B
	// has a matching reverse observation, synthesise a pair of one-sided
	// edges so the hub's Reconcile pass can detect bidirectionality.
	//
	// oosIndex uses a slice value to handle multi-port links (e.g. LAG bonds
	// where the same device pair appears on multiple port combinations).
	type oosKey struct{ device, hint string }
	type oosVal struct {
		reportingPort string
		proto         string
	}
	oosIndex := make(map[oosKey][]oosVal)

	// Detection pass: build canonical→[]raw maps to warn when distinct raw names
	// collide on the same canonical name (e.g. "core-sw-01.dc1" and
	// "core-sw-01.dc2" both normalise to "core-sw-01"). Logged at most once per
	// canonical name per merge cycle via the warnedNames guard.
	rawNamesForCanonical := make(map[string]map[string]struct{})
	for _, entry := range spokes {
		for _, n := range entry.payload.OutOfScope {
			for _, raw := range []string{n.ReportingDevice, n.NeighbourHint} {
				canon := h.canonicalizeDeviceName(raw)
				if rawNamesForCanonical[canon] == nil {
					rawNamesForCanonical[canon] = make(map[string]struct{})
				}
				rawNamesForCanonical[canon][raw] = struct{}{}
			}
		}
	}
	warnedNames := make(map[string]bool)
	for canon, rawSet := range rawNamesForCanonical {
		if len(rawSet) > 1 {
			if !warnedNames[canon] {
				warnedNames[canon] = true
				rawList := make([]string, 0, len(rawSet))
				for r := range rawSet {
					rawList = append(rawList, r)
				}
				h.logger.Warn("hub: ambiguous device name normalisation — multiple raw names map to the same canonical name; check for FQDN collisions",
					"canonical", canon,
					"raw_names", rawList,
				)
			}
		}
	}

	for _, entry := range spokes {
		for _, n := range entry.payload.OutOfScope {
			k := oosKey{h.canonicalizeDeviceName(n.ReportingDevice), h.canonicalizeDeviceName(n.NeighbourHint)}
			oosIndex[k] = append(oosIndex[k], oosVal{n.ReportingPort, n.Proto})
		}
	}

	type pairKey struct{ a, b string }
	type portPairKey struct{ aPort, bPort string }
	visited := make(map[pairKey]map[portPairKey]bool)

	var unmatchedCount int
	for k, locals := range oosIndex {
		remotes, ok := oosIndex[oosKey{k.hint, k.device}]
		if !ok {
			// Log at debug so operators can spot naming inconsistencies (e.g.
			// "Gi0/1" on one side vs "GigabitEthernet0/1" on the other) when
			// troubleshooting missing cross-domain edges.
			h.logger.Debug("hub: OOS neighbour has no reverse observation; possible naming mismatch",
				"device", k.device, "hint", k.hint)
			unmatchedCount++
			continue
		}

		a, b := k.device, k.hint
		if a > b {
			a, b = b, a
		}
		pk := pairKey{a, b}
		if visited[pk] == nil {
			visited[pk] = make(map[portPairKey]bool)
		}

		for _, local := range locals {
			for _, remote := range remotes {
				// Canonical port pair: port of the alphabetically-smaller device first,
				// so (sw-a,sw-b) and (sw-b,sw-a) iterations produce the same key.
				var aPort, bPort string
				if a == k.device {
					aPort, bPort = local.reportingPort, remote.reportingPort
				} else {
					aPort, bPort = remote.reportingPort, local.reportingPort
				}
				ppk := portPairKey{aPort, bPort}
				if visited[pk][ppk] {
					continue
				}
				visited[pk][ppk] = true

				proto := local.proto
				if proto == "" {
					proto = remote.proto
				}
				// Inject both sides so Reconcile sees len(sides) >= 2 → bidirectional.
				// proto here is a raw string from OutOfScopeNeighbour.Proto (spoke wire
				// format); cast to DiscoveryProtocol — Edge validation accepts any
				// non-empty string at this layer.
				allEdges = appendEdgePair(allEdges,
					k.device, local.reportingPort,
					k.hint, remote.reportingPort,
					discovery.DiscoveryProtocol(proto), discovery.LinkKindEthernet,
					discovery.ConfidenceMedium, 2,
				)
			}
		}
	}

	// Inject LD-19 known inter-domain links as authoritative overrides.
	// Rank 0 beats all protocol-observed edges so configured links always win.
	for i, link := range h.cfg.KnownInterDomainLinks {
		if !seenDevices[link.LocalDevice] || !seenDevices[link.RemoteDevice] {
			h.logger.Warn("hub: IDL endpoint not in combined graph; skipping",
				"link_idx", i,
				"local_device", link.LocalDevice,
				"remote_device", link.RemoteDevice,
			)
			continue
		}
		linkKind := discovery.LinkKind(link.LinkKind)
		if linkKind == "" {
			linkKind = discovery.LinkKindEthernet
		}
		allEdges = appendEdgePair(allEdges,
			link.LocalDevice, link.LocalPort,
			link.RemoteDevice, link.RemotePort,
			discovery.DiscoveryProtocolConfigured, linkKind,
			discovery.ConfidenceHigh, 0,
		)
	}

	reconciledEdges, _ := graph.Reconcile(allEdges)
	return discovery.Graph{
		Devices: allDevices,
		Edges:   reconciledEdges,
	}, unmatchedCount
}

// normalizeDeviceName lowercases s and strips the domain suffix (everything after
// the first dot). This allows bare hostnames and FQDNs to match, but will
// incorrectly merge devices that share a hostname across different domains.
// A warning is emitted when this ambiguity is detected at merge time.
func normalizeDeviceName(s string) string {
	s = strings.ToLower(s)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	return s
}

// canonicalizeDeviceName returns the canonical form of a device name for OOS
// neighbour matching. The default since v1.3.0 is strict matching: only
// case-folding is applied (preserving domain suffixes so "core-sw.dc1" and
// "core-sw.dc2" remain distinct). When LooseDeviceNameMatching is true, it
// delegates to normalizeDeviceName which strips domain suffixes — the
// pre-v1.3.0 behaviour for single-site reconciliation of FQDN/short pairs.
func (h *Hub) canonicalizeDeviceName(s string) string {
	if h.cfg.Hub.LooseDeviceNameMatching {
		return normalizeDeviceName(s)
	}
	return strings.ToLower(s)
}

// appendEdgePair appends a forward and reverse discovery.Edge to edges and
// returns the extended slice. Both edges share the same protocol, link kind,
// confidence, and precedence rank; direction is always unidirectional because
// the hub's Reconcile pass promotes to bidirectional when both sides are seen.
func appendEdgePair(edges []discovery.Edge, src, srcPort, dst, dstPort string, proto discovery.DiscoveryProtocol, linkKind discovery.LinkKind, confidence discovery.Confidence, rank int) []discovery.Edge {
	base := discovery.Edge{
		DiscoveryProto: proto,
		Direction:      discovery.DirectionUnidirectional,
		Confidence:     confidence,
		Adjacency:      discovery.AdjacencyUnknown,
		PrecedenceRank: rank,
		LinkKind:       linkKind,
		ObservedAt:     time.Now(),
	}
	fwd := base
	fwd.SrcDevice, fwd.SrcPort = src, srcPort
	fwd.DstDevice, fwd.DstPort = dst, dstPort
	rev := base
	rev.SrcDevice, rev.SrcPort = dst, dstPort
	rev.DstDevice, rev.DstPort = src, srcPort
	return append(edges, fwd, rev)
}
