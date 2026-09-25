// Package graph implements the reconciliation and diff logic. It's
// the seam where the precedence-ladder policy from
// network-o11y-dev/docs/ARCHITECTURE.md actually executes.
//
// Reconcile takes the per-cycle Edge slice — every discovery module's
// output concatenated together — and returns the same edges with their
// PrecedenceRank, Confidence, and Direction populated, duplicates collapsed
// by rank. Diff compares two reconciled cycles and produces EdgeChange
// records for the change counter and the structured log event stream.
// Conflict records same-cycle disagreements (two sources naming different
// neighbours for the same local port, for example) — surfaced, never
// silently resolved.
//
// Pure functions, no I/O.
//
// # Design references
//
//   - Bejerano, Breitbart, Garofalakis, Rastogi — "Physical Topology
//     Discovery for Large Multisubnet Networks", IEEE INFOCOM 2003. The
//     foundational paper for handling conflicting edge observations from
//     multiple sources. The insight that L2 protocols (LLDP, FDB) should
//     supersede L3 inference (BGP, OSPF) for the same physical link underpins
//     the LD-10 precedence ladder.
//     https://ieeexplore.ieee.org/document/1208686
//   - Breitbart et al. — "The NetInventory System", IEEE/ACM ToN 2004.
//     System paper for the above; documents how conflicting sources are
//     ranked and how disagreements (ConflictNeighbourDisagreement) are
//     surfaced rather than silently resolved. ConflictNeighbourDisagreement
//     is currently the only conflict kind emitted; port-name encoding
//     variants are normalised in NormalizePortName before grouping, so they
//     do not produce a conflict.
//     https://dl.acm.org/doi/abs/10.1109/TNET.2004.828963
//   - prometheus/snmp_exporter (Apache 2.0) — confirms that port name
//     encoding differences between LLDP and CDP (the LldpPortId subtype
//     issue in lldp/lldp.go) are a known real-world source of false
//     conflicts; NormalizePortName below applies the same canonicalisation
//     approach before grouping. Source: collector/collector.go
//     combinedTypeMapping.
package graph

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode"

	"github.com/grafana/network-topology-exporter/internal/discovery"
)

// ChangeKind describes a topology mutation between two cycles.
type ChangeKind string

// ChangeKind values: the type of topology mutation between cycles.
const (
	ChangeAdded   ChangeKind = "added"
	ChangeRemoved ChangeKind = "removed"
	ChangeUpdated ChangeKind = "updated"
)

// EdgeChange is one entry in a per-cycle diff.
type EdgeChange struct {
	Kind   ChangeKind
	Before *discovery.Edge
	After  *discovery.Edge
}

// ConflictKind classifies the disagreement type. Mirrors the
// `conflict_type` label on TopologyConflictTotal per LD-10.
type ConflictKind string

const (
	// ConflictNeighbourDisagreement — sources name different neighbour
	// devices for the same local port. Most serious case.
	ConflictNeighbourDisagreement ConflictKind = "neighbour_disagreement"
)

// Conflict records one disagreement between sources within a single cycle.
type Conflict struct {
	SrcDevice string
	SrcPort   string
	Kind      ConflictKind
	Sources   []string         // discovery_proto values that disagreed (e.g. ["lldp", "cdp"])
	Edges     []discovery.Edge // the conflicting edge observations, kept for the log payload
}

// Reconcile collapses per-cycle observations from multiple discovery sources
// into a single ranked edge set.
//
// Algorithm:
//  1. Group edges by their normalised EdgeKey (endpoint-sorted, port names
//     passed through NormalizePortName) so that encoding variants like
//     "GigabitEthernet0/1" and "Gi0/1" land in the same bucket.
//  2. Within each bucket, detect bidirectionality: if both endpoint devices
//     appear as the SrcDevice of at least one observation, the link is
//     DirectionBidirectional. Otherwise DirectionUnidirectional.
//  3. Select the edge with the lowest PrecedenceRank (1=highest priority).
//     When multiple edges tie at the winning rank, prefer the one from the
//     canonical (alphabetically-first) side so output is deterministic.
//  4. Normalise the chosen edge's SrcDevice/SrcPort/SrcIfIndex/DstDevice/
//     DstPort/DstIfIndex to the canonical order. Port names and ifIndexes are
//     preserved from the winning observation (not from the normalised group
//     key) so the emitted edge reflects the original encoding of the
//     highest-precedence source.
//  5. Sort the result by EdgeKey so output order is deterministic across calls.
func Reconcile(edges []discovery.Edge) ([]discovery.Edge, []Conflict) {
	if len(edges) == 0 {
		return nil, nil
	}

	type group struct {
		byRank  map[int][]discovery.Edge
		minRank int
		sides   map[string]bool // SrcDevice values that reported this link
	}

	groups := make(map[EdgeKey]*group, len(edges))
	for i := range edges {
		e := &edges[i]
		if e.SrcDevice == "" || e.DstDevice == "" {
			// Drop edges with missing device names — these are malformed
			// observations that cannot be keyed or reconciled correctly.
			continue
		}
		if e.SrcDevice == e.DstDevice {
			// Drop self-loop — protocol artefact where a device reports itself
			// as both endpoints. These are never valid physical links and pollute
			// the conflict-detection and lifecycle-ageing logic.
			continue
		}
		k := normalizedGroupKey(*e)
		g, ok := groups[k]
		if !ok {
			g = &group{
				byRank:  make(map[int][]discovery.Edge),
				minRank: e.PrecedenceRank,
				sides:   make(map[string]bool),
			}
			groups[k] = g
		}
		g.sides[e.SrcDevice] = true
		g.byRank[e.PrecedenceRank] = append(g.byRank[e.PrecedenceRank], *e)
		if e.PrecedenceRank < g.minRank {
			g.minRank = e.PrecedenceRank
		}
	}

	result := make([]discovery.Edge, 0, len(groups))
	resultByNormKey := make(map[EdgeKey]discovery.Edge, len(groups))
	for k, g := range groups {
		bidirectional := len(g.sides) >= 2
		candidates := g.byRank[g.minRank]

		// Prefer the observation from the canonical (A) side, then break ties
		// by DiscoveryProto lexically so the result is stable regardless of
		// the order discovery modules deliver edges in the input slice.
		chosen := candidates[0]
		for _, c := range candidates {
			side := c.SrcDevice == k.SrcDevice
			chosenSide := chosen.SrcDevice == k.SrcDevice
			switch {
			case side && !chosenSide:
				chosen = c // canonical side always beats non-canonical
			case side == chosenSide && c.DiscoveryProto < chosen.DiscoveryProto:
				chosen = c // same-side tie: lexically-first proto wins
			}
		}

		// Normalise to canonical endpoint order using the chosen edge's own
		// port names so the emitted edge preserves the original port encoding
		// from the winning observation (not the normalised group key form).
		if chosen.SrcDevice != k.SrcDevice {
			chosen.SrcDevice, chosen.DstDevice = chosen.DstDevice, chosen.SrcDevice
			chosen.SrcPort, chosen.DstPort = chosen.DstPort, chosen.SrcPort
			chosen.SrcIfIndex, chosen.DstIfIndex = chosen.DstIfIndex, chosen.SrcIfIndex
		}
		if bidirectional {
			chosen.Direction = discovery.DirectionBidirectional
		} else {
			chosen.Direction = discovery.DirectionUnidirectional
		}
		// Normalise port names on the emitted edge so that Key() and
		// AgeUnconfirmed produce the same EdgeKey regardless of which
		// protocol won the precedence race in this cycle (e.g. LLDP
		// "GigabitEthernet0/1" vs CDP "Gi0/1"). Without this, a winning
		// protocol change between cycles produces different Key() values,
		// causing Diff to emit spurious ChangeRemoved+ChangeAdded pairs and
		// AgeUnconfirmed to lose its counter.
		chosen.SrcPort = NormalizePortName(chosen.SrcPort)
		chosen.DstPort = NormalizePortName(chosen.DstPort)
		result = append(result, chosen)
		resultByNormKey[k] = chosen
	}

	slices.SortFunc(result, func(a, b discovery.Edge) int {
		return compareEdgeKey(Key(a), Key(b))
	})

	// Single pass over groups builds the portNeighbours conflict index.
	// SrcPort in portKey is normalised so that "Gi0/1" and "GigabitEthernet0/1"
	// observations land in the same bucket and can be compared for neighbour
	// disagreement regardless of port name encoding.
	type portKey struct{ SrcDevice, SrcPort string }
	type portObservation struct {
		key        EdgeKey
		dstDevice  string
		rawSrcPort string // original port name as reported by the protocol
	}
	portNeighbours := make(map[portKey][]portObservation, len(groups))
	for k, g := range groups {
		for _, candidates := range g.byRank {
			for _, observed := range candidates {
				pk := portKey{observed.SrcDevice, NormalizePortName(observed.SrcPort)}
				portNeighbours[pk] = append(portNeighbours[pk], portObservation{
					key:        k,
					dstDevice:  observed.DstDevice,
					rawSrcPort: observed.SrcPort,
				})
			}
		}
	}

	var conflicts []Conflict

	for pk, observations := range portNeighbours {
		if len(observations) < 2 {
			continue
		}
		dst := observations[0].dstDevice
		allSame := true
		for _, obs := range observations[1:] {
			if obs.dstDevice != dst {
				allSame = false
				break
			}
		}
		if allSame {
			continue
		}
		seenProto := make(map[discovery.DiscoveryProtocol]bool)
		var sources []string
		var conflictEdges []discovery.Edge
		seenKey := make(map[EdgeKey]bool)
		for _, obs := range observations {
			k := obs.key
			if seenKey[k] {
				continue
			}
			seenKey[k] = true
			g := groups[k]
			for _, c := range g.byRank[g.minRank] {
				if !seenProto[c.DiscoveryProto] {
					seenProto[c.DiscoveryProto] = true
					sources = append(sources, string(c.DiscoveryProto))
				}
			}
			if e, ok := resultByNormKey[k]; ok {
				conflictEdges = append(conflictEdges, e)
			}
		}
		slices.Sort(sources)
		// Sort observations by rawSrcPort so that when multiple protocol
		// encodings exist (e.g. "Gi0/1" vs "GigabitEthernet0/1"), the
		// lexically-first name is chosen deterministically rather than
		// whichever map iteration order happened to arrive first.
		slices.SortFunc(observations, func(a, b portObservation) int {
			return strings.Compare(a.rawSrcPort, b.rawSrcPort)
		})
		// Use the raw port name from the first observation so operators see
		// the name the protocol actually reported (e.g. "GigabitEthernet0/1"),
		// not the normalized key form (e.g. "Gi0/1").
		rawPort := observations[0].rawSrcPort
		conflicts = append(conflicts, Conflict{
			SrcDevice: pk.SrcDevice,
			SrcPort:   rawPort,
			Kind:      ConflictNeighbourDisagreement,
			Sources:   sources,
			Edges:     conflictEdges,
		})
	}

	if len(conflicts) == 0 {
		return result, nil
	}
	slices.SortFunc(conflicts, func(a, b Conflict) int {
		if c := strings.Compare(a.SrcDevice, b.SrcDevice); c != 0 {
			return c
		}
		return strings.Compare(a.SrcPort, b.SrcPort)
	})
	return result, conflicts
}

// Diff returns the changes between two reconciled edge sets. Both slices must
// come from Reconcile (canonical endpoint order, one entry per EdgeKey) so the
// key lookups are stable.
func Diff(before, after []discovery.Edge) []EdgeChange {
	if len(before) == 0 && len(after) == 0 {
		return nil
	}

	beforeMap := make(map[EdgeKey]discovery.Edge, len(before))
	for _, e := range before {
		beforeMap[Key(e)] = e
	}

	var changes []EdgeChange

	for i := range after {
		a := &after[i]
		k := Key(*a)
		b, exists := beforeMap[k]
		if !exists {
			aCopy := *a
			changes = append(changes, EdgeChange{Kind: ChangeAdded, After: &aCopy})
		} else if edgeMateriallyChanged(b, *a) {
			bCopy := b
			aCopy := *a
			changes = append(changes, EdgeChange{Kind: ChangeUpdated, Before: &bCopy, After: &aCopy})
		}
		delete(beforeMap, k)
	}

	var removals []EdgeChange
	for k := range beforeMap {
		b := beforeMap[k]
		bCopy := b
		removals = append(removals, EdgeChange{Kind: ChangeRemoved, Before: &bCopy})
	}
	slices.SortFunc(removals, func(i, j EdgeChange) int {
		return compareEdgeKey(Key(*i.Before), Key(*j.Before))
	})
	changes = append(changes, removals...)

	return changes
}

// edgeMateriallyChanged returns true when two edges describing the same
// physical link (same EdgeKey) differ in a way that warrants a log event and
// metric increment. Timestamp-only churn is not a change.
func edgeMateriallyChanged(before, after discovery.Edge) bool {
	return before.Direction != after.Direction ||
		before.DiscoveryProto != after.DiscoveryProto ||
		before.Confidence != after.Confidence ||
		before.PrecedenceRank != after.PrecedenceRank ||
		before.LinkKind != after.LinkKind
}

// portPrefixes maps vendor long-form interface name prefixes to their
// canonical short form. Ordered longest-first to prevent shorter prefixes
// matching prematurely (e.g. "GigabitEthernet" before "TenGigabitEthernet").
var portPrefixes = []struct{ long, short string }{
	{"HundredGigabitEthernet", "Hu"},
	{"HundredGigE", "Hu"},
	{"FortyGigabitEthernet", "Fo"},
	{"TwentyFiveGigE", "Twe"},
	{"TenGigabitEthernet", "Te"},
	{"TwoGigabitEthernet", "Tw"},
	{"GigabitEthernet", "Gi"},
	{"FastEthernet", "Fa"},
	{"Ethernet", "Eth"},
	{"Management", "Mgmt"},
	{"Port-channel", "Po"},
	{"Loopback", "Lo"},
	{"Tunnel", "Tu"},
	{"Vlan", "Vl"},
}

// NormalizePortName maps long-form interface names to their canonical short
// form so that LLDP and CDP observations for the same physical port land in
// the same edge group during reconciliation regardless of encoding. Matching
// is case-insensitive against the prefix table; the suffix (slot/module/port
// numbers) is preserved verbatim from the input.
//
// Ports without a known prefix are returned unchanged. The prefix table
// targets vendors that emit a long form needing collapse — primarily Cisco
// IOS/IOS-XE. Vendors whose native interface names are already canonical
// short forms pass through untouched, which is correct behavior, not a gap:
//
//   - Juniper Junos: ge-0/0/0, xe-1/0/1, et-2/0/0, ae0
//   - Nokia SR-OS:   1/1/1, lag-1
//   - Arista EOS:    Et1/1 (and Ethernet-prefixed forms via the table)
//   - Cisco NX-OS:   Eth1/1, mgmt0
//   - Extreme EXOS:  1, 1:1
//   - Aruba/HPE:     1/1/1, A1, Trk1
//
// If a future vendor emits a long form that needs collapsing, add it to
// portPrefixes above (longest-first to avoid partial matches).
func NormalizePortName(name string) string {
	// Strip control characters (mirrors NormaliseName in pdu.go). Some devices
	// embed \r, \x01, or other control chars in port name responses; leaving them
	// in causes edge key instability — "Gi0/1\rgarbage" and "Gi0/1" produce
	// different EdgeKeys, so the same physical port appears as two separate edges
	// across polling cycles.
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	lower := strings.ToLower(name)
	for _, p := range portPrefixes {
		if strings.HasPrefix(lower, strings.ToLower(p.long)) {
			return p.short + name[len(p.long):]
		}
	}
	return name
}

// normalizedGroupKey returns the canonical EdgeKey for grouping purposes,
// with port names passed through NormalizePortName. Used only inside
// Reconcile so that encoding variants of the same port land in one bucket.
func normalizedGroupKey(e discovery.Edge) EdgeKey {
	a := EdgeKey{
		SrcDevice: e.SrcDevice, SrcPort: NormalizePortName(e.SrcPort),
		DstDevice: e.DstDevice, DstPort: NormalizePortName(e.DstPort),
	}
	b := EdgeKey{
		SrcDevice: e.DstDevice, SrcPort: NormalizePortName(e.DstPort),
		DstDevice: e.SrcDevice, DstPort: NormalizePortName(e.SrcPort),
	}
	if compareEdgeKey(a, b) <= 0 {
		return a
	}
	return b
}

// EdgeKey is the LD-14 lifecycle key. Two Edge observations describe the
// same physical link when they share an EdgeKey, regardless of which
// discovery protocol produced them. The graph layer tracks the
// consecutive-unconfirmed-cycle counter against this key in the snapshot.
type EdgeKey struct {
	SrcDevice string
	SrcPort   string
	DstDevice string
	DstPort   string
}

// Key returns the LD-14 lifecycle key for an Edge. Endpoints are sorted so
// the same physical link emits the same key regardless of which side the
// discovery protocol named first.
func Key(e discovery.Edge) EdgeKey {
	a := EdgeKey{SrcDevice: e.SrcDevice, SrcPort: e.SrcPort, DstDevice: e.DstDevice, DstPort: e.DstPort}
	b := EdgeKey{SrcDevice: e.DstDevice, SrcPort: e.DstPort, DstDevice: e.SrcDevice, DstPort: e.SrcPort}
	if compareEdgeKey(a, b) <= 0 {
		return a
	}
	return b
}

// EdgeKeyString serialises an EdgeKey to the pipe-delimited format used in
// snapshot.File.UnconfirmedAges ("srcDevice|srcPort|dstDevice|dstPort").
// The key must be in canonical order (from Key()) for round-trips to be
// stable. Literal "%" characters are percent-encoded as "%25" first, then
// literal "|" characters are encoded as "%7C", so that EdgeKeyFromString can
// round-trip without ambiguity.
func EdgeKeyString(k EdgeKey) string {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "%", "%25")
		return strings.ReplaceAll(s, "|", "%7C")
	}
	return esc(k.SrcDevice) + "|" + esc(k.SrcPort) + "|" + esc(k.DstDevice) + "|" + esc(k.DstPort)
}

// EdgeKeyFromString parses the pipe-delimited snapshot format back into an
// EdgeKey. Returns an error if s does not have exactly three separators.
// "%7C" in any field is unescaped back to "|" first, then "%25" is unescaped
// back to "%".
func EdgeKeyFromString(s string) (EdgeKey, error) {
	parts := strings.SplitN(s, "|", 4)
	if len(parts) != 4 {
		return EdgeKey{}, fmt.Errorf("graph: invalid edge key %q (want srcDevice|srcPort|dstDevice|dstPort)", s)
	}
	unesc := func(s string) string {
		s = strings.ReplaceAll(s, "%7C", "|")
		return strings.ReplaceAll(s, "%25", "%")
	}
	return EdgeKey{SrcDevice: unesc(parts[0]), SrcPort: unesc(parts[1]), DstDevice: unesc(parts[2]), DstPort: unesc(parts[3])}, nil
}

// AgeUnconfirmed advances the LD-14 lifecycle counter by one cycle for every
// edge in current that is unidirectional, and returns the keys to remove
// because their counter has reached ttl. ages is the persisted counter map
// from the snapshot (LD-13); it's mutated in place so the caller can write
// it back to the next snapshot. A bidirectional observation resets the
// counter to zero — bidirectional links never time out via this path.
//
// ttl mirrors discovery.unconfirmed_link_ttl_cycles from config (default 3).
func AgeUnconfirmed(current []discovery.Edge, ages map[EdgeKey]int, ttl int) []EdgeKey {
	if ages == nil {
		return nil
	}
	seen := make(map[EdgeKey]bool, len(current))
	var expired []EdgeKey
	for _, e := range current {
		k := Key(e)
		seen[k] = true
		if e.Direction == discovery.DirectionBidirectional {
			delete(ages, k)
			continue
		}
		// First time this edge is seen as unidirectional: record it at 0 so it
		// gets a full cycle before any expiry check. On subsequent unconfirmed
		// cycles the counter increments and expires once it reaches ttl.
		if _, exists := ages[k]; !exists {
			ages[k] = 0
		} else {
			ages[k]++
			if ages[k] >= ttl {
				expired = append(expired, k)
				delete(ages, k) // reset so a reappearing edge gets a fresh counter
			}
		}
	}
	// Edges absent from this cycle are also unconfirmed: increment their
	// counter and expire them once they reach ttl absent cycles, rather than
	// resetting (deleting) them immediately.
	for k := range ages {
		if !seen[k] {
			ages[k]++
			if ages[k] >= ttl {
				expired = append(expired, k)
				delete(ages, k)
			}
		}
	}
	return expired
}

// AgesToEdgeKeys converts the string-keyed map from snapshot.File.UnconfirmedAges
// back to the EdgeKey-keyed form used by AgeUnconfirmed. Entries whose key
// does not parse (malformed snapshot) are silently dropped.
func AgesToEdgeKeys(in map[string]int) map[EdgeKey]int {
	out := make(map[EdgeKey]int, len(in))
	for s, v := range in {
		k, err := EdgeKeyFromString(s)
		if err != nil {
			slog.Warn("graph: AgesToEdgeKeys: skipping malformed edge key", "key", s)
			continue
		}
		out[k] = v
	}
	return out
}

// EdgeKeysToAges converts the EdgeKey-keyed counter map back to the
// string-keyed form that snapshot.File serialises to JSON.
func EdgeKeysToAges(in map[EdgeKey]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[EdgeKeyString(k)] = v
	}
	return out
}

func compareEdgeKey(a, b EdgeKey) int {
	switch {
	case a.SrcDevice < b.SrcDevice:
		return -1
	case a.SrcDevice > b.SrcDevice:
		return 1
	case a.SrcPort < b.SrcPort:
		return -1
	case a.SrcPort > b.SrcPort:
		return 1
	case a.DstDevice < b.DstDevice:
		return -1
	case a.DstDevice > b.DstDevice:
		return 1
	case a.DstPort < b.DstPort:
		return -1
	case a.DstPort > b.DstPort:
		return 1
	default:
		return 0
	}
}
