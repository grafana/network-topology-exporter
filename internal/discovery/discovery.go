// Package discovery defines the Device and Edge value types every protocol
// module produces. Concrete walking logic lives in
// internal/discovery/<protocol>/.
//
// # Graph model reference
//
// The abstract structure (nodes, links, termination-points) follows the
// conceptual model in RFC 8345 — "A YANG Data Model for Network Topologies"
// (Clemm et al., 2018, https://datatracker.ietf.org/doc/html/rfc8345).
// RFC 8345 is the current IETF standard representation for topology data;
// the Device/Edge naming here is equivalent to its node/link vocabulary.
//
// # Edge schema
//
// The Edge schema is shaped by LD-10 (network-o11y-dev/docs/ARCHITECTURE.md):
// every edge carries the source protocol, direction, confidence bucket,
// direct-vs-indirect adjacency classification, and precedence rank so the
// graph layer can reconcile without re-deriving that information.
//
// The Confidence and Adjacency fields are informed by:
//   - Bejerano, Breitbart, Garofalakis, Rastogi — "Physical Topology
//     Discovery for Large Multisubnet Networks", IEEE INFOCOM 2003. The
//     direct/indirect classification (one MAC on a port = direct; many MACs
//     = indirect) is the core contribution of that paper.
//     https://ieeexplore.ieee.org/document/1208686
//   - Breitbart et al. — "The NetInventory System", IEEE/ACM ToN 2004.
//     Describes ranking multiple protocol sources for the same physical link;
//     the precedence ladder in LD-10 follows this approach.
//     https://dl.acm.org/doi/abs/10.1109/TNET.2004.828963
package discovery

import (
	"fmt"
	"strings"
	"time"
)

const (
	// MetadataKeyDegraded marks an edge emitted in degraded mode.
	// The OTLP exporter prepends "network.topology." when emitting attributes,
	// so this bare key avoids double-prefixing (e.g. "network.topology.network.topology.degraded").
	MetadataKeyDegraded = "degraded"
	// MetadataKeyDegradedReason carries the degraded-mode reason code.
	MetadataKeyDegradedReason = "degraded_reason"
	// MetadataKeyPeerChassisMac is set by the LLDP walker on edges where the peer
	// advertises a MAC chassis ID and a sysName, enabling FDB MAC→sysName resolution.
	MetadataKeyPeerChassisMac = "peer_chassis_mac"

	// DegradedReasonRequiredTablePartialDecode means a walker decoded a
	// required MIB table but flagged the result as partial (some rows
	// failed decode). DegradedReason* constants appear in the
	// MetadataKeyDegradedReason field on edges and in the
	// network_topology_discovery_degraded_total `reason` label; changing
	// these strings breaks downstream consumers.
	DegradedReasonRequiredTablePartialDecode = "required_table_partial_decode"
	// DegradedReasonMissingSrcPortMapping means a walker produced an edge
	// without resolving SrcPort against the host's ifIndex inventory.
	DegradedReasonMissingSrcPortMapping = "missing_srcport_mapping"
	// DegradedReasonMissingAdminStatusWalk means the IF-MIB ifAdminStatus
	// walk did not return any usable rows for the device.
	DegradedReasonMissingAdminStatusWalk = "missing_admin_status_walk"
	// DegradedReasonInvalidAdminStatusDecode means at least one
	// ifAdminStatus row could not be decoded into a known enum value.
	DegradedReasonInvalidAdminStatusDecode = "invalid_admin_status_decode"
	// DegradedReasonUnsupportedIPVersion means a walker observed a peer
	// address family it cannot resolve (e.g. IPv6 on an IPv4-only walker).
	DegradedReasonUnsupportedIPVersion = "unsupported_ip_version"
)

// PolicyError marks a module-level policy failure with a machine-readable reason.
type PolicyError struct {
	Module string
	Reason string
	Err    error
}

func (e *PolicyError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s policy failure: %s", e.Module, e.Reason)
	}
	return fmt.Sprintf("%s policy failure: %s: %v", e.Module, e.Reason, e.Err)
}

func (e *PolicyError) Unwrap() error { return e.Err }

// JoinReasonCodes canonicalizes a reason slice into a deduplicated comma list.
func JoinReasonCodes(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	seen := make(map[string]bool)
	ordered := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if reason == "" || seen[reason] {
			continue
		}
		seen[reason] = true
		ordered = append(ordered, reason)
	}
	return strings.Join(ordered, ",")
}

// Device is the inventory record for one network node.
//
// Output projections (issue #150) diverge intentionally, like Edge above:
// Prometheus network_topology_device_info carries {device_id, vendor, model,
// os_version, site, sys_name} unconditionally (Uptime is a separate metric,
// Labels are not emitted as series), guarded by TestDeviceInfoLabelSchemaStable;
// OTLP deviceAttrs emits the same fields (as "device_id", "sys_name", ...) but
// OMITS empty ones; YANG maps to RFC 8345 node attributes. Do not unify the
// three — decide per output when adding a field.
type Device struct {
	ID        string // sysName (NormaliseName'd: lowercased); fallback: management IP
	SysName   string // sysName, case-preserving (control-char strip + trim + truncate, no ToLower); empty if unresolved. See docs/proposals/snmp-exporter-label-alignment.md §5.2 — this is the join key against snmp_exporter's own sysName label, which snmp_exporter emits in the device's original case.
	Vendor    string
	Model     string
	OSVersion string
	Site      string // from per-target enrichment config, not SNMP
	Uptime    time.Duration
	Labels    map[string]string // free-form labels from per-target enrichment config
}

// DiscoveryProtocol identifies the discovery source that produced an Edge.
// The underlying string is the wire-format value emitted in Prometheus labels,
// OTLP attributes, and snapshot JSON — changing these strings breaks consumers.
// The name intentionally repeats the package (discovery.DiscoveryProtocol):
// the metric label and OTLP attribute key is "discovery_protocol", and
// renaming the type to "Protocol" would create discovery.Protocol calls that
// no longer self-document at the use site.
type DiscoveryProtocol string //nolint:revive // see doc comment: name is part of the wire contract

// DiscoveryProtocol values: one constant per emitter that constructs an Edge.
// "configured" is reserved for federation hub-injected LD-19 inter-domain links.
const (
	DiscoveryProtocolLLDP       DiscoveryProtocol = "lldp"
	DiscoveryProtocolCDP        DiscoveryProtocol = "cdp"
	DiscoveryProtocolBGP        DiscoveryProtocol = "bgp"
	DiscoveryProtocolOSPF       DiscoveryProtocol = "ospf"
	DiscoveryProtocolFDB        DiscoveryProtocol = "fdb"
	DiscoveryProtocolISIS       DiscoveryProtocol = "isis"
	DiscoveryProtocolMPLSTE     DiscoveryProtocol = "mpls_te"
	DiscoveryProtocolConfigured DiscoveryProtocol = "configured" // hub-injected from LD-19 KnownInterDomainLinks
)

// String returns the underlying wire value, satisfying fmt.Stringer.
func (p DiscoveryProtocol) String() string { return string(p) }

// Valid reports whether p is one of the declared DiscoveryProtocol constants.
func (p DiscoveryProtocol) Valid() bool {
	switch p {
	case DiscoveryProtocolLLDP, DiscoveryProtocolCDP, DiscoveryProtocolBGP,
		DiscoveryProtocolOSPF, DiscoveryProtocolFDB, DiscoveryProtocolISIS,
		DiscoveryProtocolMPLSTE, DiscoveryProtocolConfigured:
		return true
	}
	return false
}

// LinkKind describes the transport semantics of a discovered link, independent
// of how the link was discovered. Emitted as the `link_kind` Prometheus label
// and OTLP attribute. The underlying string is the wire format.
type LinkKind string

// LinkKind values: one constant per transport semantic recognised by the
// discovery layer. "logical" covers tunnel / VPN endpoints; "ip" covers
// routing-protocol adjacencies (BGP/OSPF/IS-IS).
const (
	LinkKindEthernet LinkKind = "ethernet"
	LinkKindMPLSTE   LinkKind = "mpls-te"
	LinkKindIP       LinkKind = "ip"
	LinkKindLogical  LinkKind = "logical"
)

// String returns the underlying wire value, satisfying fmt.Stringer.
func (k LinkKind) String() string { return string(k) }

// Valid reports whether k is one of the declared LinkKind constants.
func (k LinkKind) Valid() bool {
	switch k {
	case LinkKindEthernet, LinkKindMPLSTE, LinkKindIP, LinkKindLogical:
		return true
	}
	return false
}

// Direction records whether a link was confirmed from both endpoints (the
// other end's discovery protocol agrees the link exists) or only one end.
// Bidirectional confirmation is a strong signal; unidirectional links survive
// in the graph but at lower precedence per LD-10.
type Direction string

// Direction values: confirmed from both endpoints or only one.
const (
	DirectionBidirectional  Direction = "bidirectional"
	DirectionUnidirectional Direction = "unidirectional"
)

// String returns the underlying wire value, satisfying fmt.Stringer.
func (d Direction) String() string { return string(d) }

// Valid reports whether d is one of the declared Direction constants.
func (d Direction) Valid() bool {
	switch d {
	case DirectionBidirectional, DirectionUnidirectional:
		return true
	}
	return false
}

// Confidence is the coarse three-bucket classification used by LD-10. v1 does
// not learn confidence from history; the bucket is a function of the source
// protocol and direction. v2 may layer a learned score on top without
// changing this enum.
type Confidence string

// Confidence bucket values: high for LLDP/CDP direct, medium for FDB indirect, low for heuristics.
const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// String returns the underlying wire value, satisfying fmt.Stringer.
func (c Confidence) String() string { return string(c) }

// Valid reports whether c is one of the declared Confidence constants.
func (c Confidence) Valid() bool {
	switch c {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
		return true
	}
	return false
}

// Adjacency is the NetXMS-style direct-vs-indirect classification: a port
// with exactly one MAC in its bridge FDB is direct (probably points at a
// real device); a port with multiple MACs is indirect (downstream switch,
// hypervisor host, aggregator). Emitted to Prometheus as the `link_kind`
// label per LD-10.
type Adjacency string

// Adjacency values: direct means one MAC on the port; indirect means multiple (downstream switch, hypervisor).
const (
	AdjacencyDirect   Adjacency = "direct"
	AdjacencyIndirect Adjacency = "indirect"
	AdjacencyUnknown  Adjacency = "unknown" // for sources that can't tell (BGP, OSPF, manual)
)

// String returns the underlying wire value, satisfying fmt.Stringer.
func (a Adjacency) String() string { return string(a) }

// Valid reports whether a is one of the declared Adjacency constants.
func (a Adjacency) Valid() bool {
	switch a {
	case AdjacencyDirect, AdjacencyIndirect, AdjacencyUnknown:
		return true
	}
	return false
}

// Edge is one observation of a link between two devices, made by a single
// discovery protocol. Multiple Edge values can describe the same physical
// link from different sources; reconciliation per LD-10 is the graph
// package's job, not the discovery module's.
//
// Edge serves as both a raw protocol observation (with MAC/IP DstDevice values)
// and a canonical graph edge (with resolved sysName DstDevice). The synthesis
// step in runCycle converts observations to canonical form before reconciliation.
//
// Output projections (issue #150 — read before "unifying" them). An Edge is
// projected to three outputs that DELIBERATELY carry different field subsets;
// this divergence is intentional, not drift, and the three mappers are kept
// separate on purpose:
//   - Prometheus network_topology_edge_info (internal/metrics/topology_collector.go):
//     a fixed, minimal label schema {src_device, src_port, dst_device, dst_port,
//     discovery_proto, link_kind, direction}. It deliberately OMITS Confidence,
//     Adjacency, PrecedenceRank, and Metadata to bound label cardinality — these
//     vary per-edge and would multiply series. Adding a label here is a conscious
//     cardinality decision (guarded by TestEdgeInfoLabelSchemaStable in metrics).
//   - OTLP edgeAttrs (internal/output/otlp): the full attribute set including
//     confidence/adjacency/precedence_rank and every Metadata key (attribute
//     budgets, not label cardinality, govern there). Note the proto attribute key
//     is "proto", not "discovery_proto".
//   - YANG (internal/output/yang): RFC 8345 nested structure — a bidirectional
//     Edge becomes two directed links, fields map to specific YANG leaves.
//
// A single shared projection was assessed and rejected (#150): the outputs differ
// in selection, attribute-key names, conditional emission, sanitization semantics,
// and structure, so a common abstraction would need more knobs than the three
// direct mappers and would risk silently changing a wire contract. When adding a
// field to Edge, decide PER OUTPUT whether it belongs.
type Edge struct {
	SrcDevice string
	SrcPort   string
	DstDevice string
	DstPort   string

	// SrcIfIndex / DstIfIndex are the IF-MIB ifIndex for each endpoint, when the
	// discovery protocol resolves one — 0 means unresolved (emitted as an empty
	// label, mirroring how SrcPort/DstPort already degrade to empty). Populated
	// today by LLDP/CDP/FDB/IS-IS (always the local/reporting side only — none of
	// these protocols carry the remote device's own ifIndex numbering); left at
	// zero for BGP/OSPF/MPLS-TE, which have no interface-scoped MIB row to resolve
	// one from. See docs/proposals/snmp-exporter-label-alignment.md §4 — this is
	// the join key against snmp_exporter's ifIndex-keyed if_mib rows.
	SrcIfIndex int
	DstIfIndex int

	// LD-10 reconciliation labels. The metric layer maps these directly onto
	// `network_topology_edge_info` labels.
	DiscoveryProto DiscoveryProtocol // lldp | cdp | bgp | ospf | fdb | isis | mpls_te | configured
	Direction      Direction
	Confidence     Confidence
	Adjacency      Adjacency
	PrecedenceRank int // 0 = highest (LD-19 configured overrides); lower rank = higher priority. See LD-10 ladder.

	// LinkKind describes the link's transport semantics, independent of how it
	// was discovered: "ethernet" for L2 LLDP/CDP/FDB observations, "ip" for
	// routing-protocol peer adjacencies (BGP, OSPF, IS-IS), "mpls-te" for
	// MPLS TE tunnels, "logical" for tunnel / VPN endpoints. Not part of the
	// LD-10 label set; consumed by dashboards that want to filter by transport.
	LinkKind LinkKind

	// LD-14 lifecycle. ObservedAt timestamps the cycle this Edge was emitted;
	// the graph layer tracks UnconfirmedCycles internally and removes a link
	// once it has been unidirectional for DiscoveryConfig.UnconfirmedLinkTTLCycles
	// in a row. The discovery module fills ObservedAt and leaves the counter
	// at zero — the counter is graph-layer state, not per-cycle state.
	ObservedAt time.Time

	// Metadata holds protocol-specific attributes that don't fit the core schema.
	// Keys and values are free-form strings. Nil when no extra metadata is present.
	Metadata map[string]string
}

// OutOfScopeNeighbour is the LD-11 surface for neighbours discovered via
// LLDP/CDP whose IP falls outside the configured CIDR allow-list. The
// exporter records the report but never polls the neighbour.
type OutOfScopeNeighbour struct {
	ReportingDevice string
	ReportingPort   string
	NeighbourHint   string // chassis-id, hostname, or IP — whatever the source gave us
	Proto           string // discovery protocol that reported this neighbour (lldp, cdp, …)
	FirstSeen       time.Time
	LastSeen        time.Time
}

// Graph is the reconciled view of the network at one point in time. It's
// what the snapshot writer serialises and what the metric collector reads
// to build /metrics responses.
type Graph struct {
	Devices    []Device
	Edges      []Edge
	OutOfScope []OutOfScopeNeighbour
}
