# Architecture

`network-topology-exporter` is a Go module built into two binaries: `cmd/topology-exporter`, modelled on the [Prometheus exporter pattern](https://prometheus.io/docs/instrumenting/writing_exporters/), and the optional `cmd/topology-hub` federation-hub aggregator (see [Federation](#federation) below). `cmd/topology-exporter` runs a periodic discovery cycle and exposes the results through two always-on, industry-standard signals — Prometheus metrics and structured JSON log lines to stderr — plus optional OTLP push and an optional RFC 8345 YANG document endpoint (see the output contract below). `cmd/topology-hub` runs no local discovery cycle at all; it aggregates spoke pushes and exposes the combined graph through the same two always-on signals.

## Design principles

The exporter emits standard signals only. No bespoke control-plane API, no proprietary RPCs. Anything that needs the topology graph queries Prometheus / Mimir; anything that needs a change feed ships the log stream to Loki (via Promtail / Alloy — not via this binary). Discovery is modular: each protocol lives in its own package under `internal/discovery/`, modules emit common `Device` and `Edge` value types, and the exporter coordinates while the metrics layer translates to Prometheus series.

**Output contract:** Prometheus metrics on `/metrics` and structured JSON log lines to stderr are the always-on outputs. OTLP push is optional and opt-in via `output.otlp.enabled: true`; when enabled, the exporter additionally pushes the topology graph as OTLP metrics to `/v1/metrics` and topology-change events as OTLP log records to `/v1/logs` on the configured endpoint. Tracing of the exporter's own discovery cycle is separately opt-in via `output.otlp.traces.enabled` (see `docs/operator/tracing.md`). With `output.yang.enabled: true`, `GET /topology/yang` additionally serves the reconciled graph as an RFC 8345/8346 YANG-JSON document for NETCONF/RESTCONF-aware consumers (see `docs/operator/yang-topology.md`). All optional paths are parallel to (not a replacement for) Prometheus scraping. Loki direct push and NetBox sync remain explicitly out of scope — those couplings belong in the shipping agent (Promtail, Alloy, Fluentd, custom NetBox integrations), not in this binary.

Two policies are binding on contributors:

- **Source-for-spec, never source-for-code (LD-09).** GPL-licensed monitoring source may be read for behavioural extraction into specifications only. The Go implementation is written from the specifications. Full guardrails in [`CONTRIBUTING.md`](../CONTRIBUTING.md).
- **Source-attributed reconciliation (LD-10).** Every emitted edge carries `discovery_proto`, `direction`, `link_kind`, and `precedence_rank`. Conflicts between sources emit a separate counter rather than being silently resolved. The reconciliation model is informed by Breitbart et al., "The NetInventory System" (IEEE/ACM ToN, 2004), which demonstrated that ranking multiple protocol sources for the same physical link — and surfacing disagreements rather than silently arbitrating — is the correct approach for heterogeneous network environments.

Four operational commitments:

- **Polling scope is bounded by an allow-list (LD-11).** The exporter polls only IPs in `discovery.scope.cidr_allow_list`. Out-of-scope neighbours emit a log line and increment `network_topology_out_of_scope_neighbours_total` but are never polled. The iterative BFS discovery pattern — seed device → query neighbour tables → enqueue new neighbours — is documented in "Discovering Neighbor Devices in Computer Network" (arXiv:1709.02209, 2017); the allow-list is the hard boundary that keeps BFS expansion safe in production.
- **Credential management is a first-class subsystem (LD-12).** Named profiles, per-IP and per-CIDR assignments, ordered fallback, and a token-bucket trial limiter. Per-device winning profiles are cached in the snapshot. The lockout risk on cold start (many auth failures in rapid succession triggering device-level SNMP lockout policies) informed the rate limiter design. Operator runbook: [`docs/operator/cold-start-credentials.md`](operator/cold-start-credentials.md).
- **The graph survives restart (LD-13).** A versioned JSON snapshot is written atomically after every successful cycle. On startup the exporter serves the previous edge set immediately with `network_topology_graph_stale=1` until the first live cycle completes.
- **Unidirectional links have a lifecycle (LD-14).** A link reported by only one endpoint for `discovery.unconfirmed_link_ttl_cycles` consecutive cycles (default 3) is removed. This directly addresses the partial-topology problem identified in "Improved algorithm for network topology discovery based on STP" (IEEE, 2011): bridges do not retain FDB entries for traffic not recently seen, so one-sided link reports are normal and should not be treated as permanent topology facts.

## Module layout

```
.
├── cmd/
│   ├── topology-exporter/main.go     # core entrypoint: HTTP server, discovery loop (standalone/uncoordinated/spoke)
│   └── topology-hub/main.go          # optional federation-hub entrypoint (federation.role: hub only)
├── internal/
│   ├── config/                       # YAML config schema + validation (LD-11 scope guard)
│   ├── version/                      # build metadata (ldflags-injected)
│   ├── metrics/                      # every Prometheus metric the exporter emits
│   ├── events/                       # structured log lines for topology change events
│   ├── credentials/                  # LD-12 named profiles, resolver, trial limiter, cache
│   ├── snapshot/                     # LD-13 versioned JSON persistence (atomic write)
│   ├── graph/                        # LD-10 reconciliation + LD-14 unconfirmed-link lifecycle
│   ├── federation/                   # LD-15, LD-17–LD-19 spoke-side + shared wire type; imported by cmd/topology-exporter
│   │   ├── payload.go                # SpokePayload shared wire type
│   │   └── spoke.go                  # pushes graph to hub after each cycle
│   ├── federationhub/                # LD-16, LD-18–LD-20 hub aggregation + HA; imported ONLY by cmd/topology-hub
│   │   ├── hub.go                    # push receiver, spoke-edge store, reconcile trigger
│   │   ├── hub_merge.go               # combined-graph construction, cross-boundary matching
│   │   ├── hub_push.go                # /spoke/push HTTP handling + reject contract
│   │   └── elector_k8s.go             # k8s.io/client-go Lease-based HA leader election (issue #71) — the only
│   │                                   # place this dependency is imported anywhere in the module
│   └── discovery/
│       ├── discovery.go              # shared Device / Edge / OutOfScopeNeighbour types + interfaces
│       ├── snmp/                     # SYSTEM group walk (RFC 3418)
│       ├── lldp/                     # IEEE 802.1AB LLDP-MIB
│       ├── cdp/                      # CISCO-CDP-MIB
│       ├── bgp/                      # RFC 4273 BGP4-MIB + BGP4-V2 vendor tables (Cisco/Arista/Juniper/Nokia)
│       ├── ospf/                     # RFC 4750 OSPF-MIB
│       ├── isis/                     # IS-IS adjacency walk (ISIS-MIB draft)
│       ├── mpls/                     # MPLS-TE-STD-MIB tunnel discovery
│       ├── arp/                      # IP-MIB ipNetToPhysicalTable (FDB enrichment only — not an edge source)
│       └── fdb/                      # RFC 4188 BRIDGE-MIB dot1dTpFdbTable, RFC 4363 dot1qTpFdbTable
├── config/example.yaml               # documented configuration schema
└── docs/operator/                    # runbooks
```

`internal/federation` and `internal/federationhub` used to be one package (`internal/federation`, including the hub). They were split along the `cmd/` boundary — not a build tag, since Go's `internal/` visibility rules require both binaries to stay in this one module (`docs/proposals/core-hub-split.md` §3.1) — so that `cmd/topology-exporter` never links `k8s.io/client-go`: the Kubernetes API client `internal/federationhub/elector_k8s.go` needs for opt-in HA leader election, regardless of `federation.role`, previously shipped in every build. `federation.role: hub` is rejected at startup by `cmd/topology-exporter` with a message pointing at `cmd/topology-hub`; `cmd/topology-hub` rejects every other role the same way. This was a pure repackaging — no behavior change for any role — see the proposal doc for the full before/after accounting.

## Discovery cycle

```
startup:
  snapshot.Load(path) → graph + credential cache (LD-13)
  network_topology_graph_stale = 1
  serve /metrics from the loaded graph

for each IP in cidr_allow_list, in parallel up to parallelism:
  profile = credentials.CachedProfile(ip) ?? Resolve(ip)
  credentials.AcquireTrial(ctx)        ── token-bucket (LD-12)
  device = snmp.Walk(ip, profile)      ── populates DeviceInfo
  credentials.RecordSuccess(device.ID, profile)
  for each enabled module (lldp, cdp, bgp, ospf, isis, mpls_te, fdb; arp enriches fdb):
    result = module.Probe(ip)
    for each out-of-scope neighbour: log line + counter (LD-11)

graph.Reconcile(edges) → ranked edges per LD-10 precedence ladder
graph.AgeUnconfirmed(edges, ages, ttl) → expired keys (LD-14)
graph.Diff(previous, current) → TopologyChangeTotal increments
                               + structured log lines per change (events.Logger)
store.Replace(graph)
snapshot.Write(graph + credential cache + ages) — atomic (LD-13)
network_topology_graph_stale = 0
observe network_topology_discovery_cycle_duration_seconds
```

Change events appear on two surfaces. The counter (`network_topology_change_total`) is what alerts fire on. The log line carries the full before/after edge record so the operator can answer "which edge changed?" without joining metric series in their head.

## Discovery contracts (hard-fail vs degraded)

Discovery modules now classify SNMP inputs as **required** or **optional**:

- **IS-IS**
  - `isisISAdjState` is required. Decode anomalies are hard-fail.
  - `isisISCircIfIndex`/`ifDescr` mapping for `SrcPort` is optional. Failures
    degrade output (`network.topology.degraded=true`,
    `network.topology.degraded_reason=missing_srcport_mapping`) but do not drop
    otherwise valid adjacency edges.
- **MPLS-TE**
  - `mplsTunnelOperStatus` is required. Decode anomalies are hard-fail.
  - `mplsTunnelAdminStatus` is optional metadata. Failures degrade output
    (`network.topology.degraded=true`) while preserving tunnel edges with
    `mpls_te.admin_status=unknown`.

This split prevents silent topology corruption for required signals while
preserving best-effort behavior for non-critical enrichments.

The IS-IS and MPLS-TE contracts above are two rows of a full per-walker audit.
[`docs/operator/failure-modes.md`](operator/failure-modes.md) is the complete
dependency × failure-mode × operator-signal matrix for every walker (LLDP, CDP,
OSPF, IS-IS, BGP, FDB, MPLS-TE, the SNMP system walk, and the ARP enrichment
sub-walk), and records which walkers have full outcome-counter coverage versus
which still degrade silently (issue #67).

## Intentional non-features

**ARP tables are not a topology source.** ARP tables (IP-MIB `ipNetToPhysicalTable`) record IP→MAC mappings on directly attached subnets. They encode L3 reachability, not physical adjacency. Two devices sharing a /24 both appear in each other's ARP tables even when connected through several intermediate switches. Bejerano et al. ("Physical Topology Discovery for Large Multisubnet Networks", IEEE INFOCOM 2003) and Pandey et al. ("IP Network Topology Discovery Using SNMP", ICOIN 2009) both use ARP data only as an IP→MAC resolution helper alongside the Bridge FDB — never as an independent edge source. If MAC→IP resolution is needed in a future FDB enhancement, ARP queries belong as an internal helper inside `internal/discovery/fdb`, not as a standalone module.

**NetBox writeback is out of scope.** The exporter emits observability signals only (Prometheus metrics, structured log lines). Writing discovered topology into NetBox would break the output contract: the binary would need to handle partial writes, NetBox downtime, auth failures, idempotency, and the risk of overwriting operator-curated documentation with incorrect discovery data. The correct pattern is a separate reconciliation process that reads from Prometheus/Mimir and writes to NetBox.

## Concurrency

The discovery scheduler runs one cycle at a time. Inside a cycle, a bounded worker pool of `discovery.parallelism` goroutines (default 32; `config/example.yaml` ships with the override 20) probes devices concurrently, each bounded by `discovery.timeout_per_device`. If a cycle overruns `discovery.interval`, the next cycle starts immediately — no queuing. `network_topology_discovery_cycle_duration_seconds` is the operator's signal that the interval needs to grow.

The credential trial limiter is shared across the worker pool so the global trial rate stays at `credentials.trial_rate_per_second` regardless of pool size. `TopologyCollector` holds the current graph in an `atomic.Pointer[discovery.Graph]`: `Update()` stores a new immutable snapshot atomically and `Collect()` loads it, so concurrent scrapes read the same snapshot with no lock contention and no empty-window gap. The credential cache uses a `sync.RWMutex`: success recorders hold the write lock briefly; scrapes take an RLock.

## Federation

A single exporter instance covers one contiguous address space. When multiple instances are deployed against non-overlapping CIDR ranges, links that cross the boundary between ranges are observed independently by each side — each instance sees its neighbour as out-of-scope and drops the edge. Neither instance produces a `DirectionBidirectional` edge for cross-boundary links because bidirectionality detection requires both endpoint observations in a single `graph.Reconcile` call.

This is a well-characterised structural problem. RFC 5441 (BRPC Procedure, Vasseur et al., IETF 2009) states it directly: in a multi-domain topology, no domain-wide topological truth exists at any single point. The IETF OPSAWG Digital Map draft (draft-havel-opsawg-digital-map-01, 2023) identifies the same gap in RFC 8345 itself: the YANG topology model cannot express a link whose endpoints belong to different network instances. Three modes address this at increasing operational complexity.

**LD-15: Uncoordinated mode — boundary observations, external stitching.**

`federation.role: uncoordinated` runs the standard discovery loop and additionally emits `network_topology_boundary_observation_info{peer_a, peer_b, reporting_device, src_port, proto}` — one series per out-of-scope neighbour, reset each cycle. `peer_a` and `peer_b` carry the canonical endpoint pair: the alphabetically-smaller of the reporting device's sysName and the NormaliseName-normalised `dst_hint` value is always `peer_a`. `reporting_device` identifies which instance contributed the observation. No inter-instance coordination is required at runtime. The canonical ordering means the Mimir recording rule that detects confirmed cross-boundary edges requires no label manipulation: `count by(peer_a, peer_b, proto)(network_topology_boundary_observation_info) == 2` fires exactly when both sides have reported and produces one series per physical link. The directional alternative — `{src_device, dst_hint}` labels — requires `label_replace` to swap label names before the join, produces two series per confirmed edge, and leaks synthetic join labels into dashboards; that complexity is unnecessary when the canonical-pair ordering is established at emission time. This is the approach the OPSAWG Digital Map draft endorses as the practical workaround for the RFC 8345 cross-domain link gap — external stitching, since the model itself cannot express it. The `dst_hint` source for `peer_a`/`peer_b` normalisation is the NormaliseName-normalised sysName from LLDP/CDP (`lldpRemSysName`, `cdpCacheDeviceId`), falling back to chassis-ID then management IP in that order. Name inconsistencies across domain boundaries produce silent stitching failures; see LD-19 for the fallback.

**LD-16: Hub/spoke mode — push transport, not pull.**

`federation.role: hub` / `federation.role: spoke` configures a hierarchical aggregation following the H-PCE architecture (RFC 6805, King and Farrel, IETF 2012; extended by RFC 8685, Dhody et al., IETF 2019). Spokes push their discovery results to the hub after each cycle; the hub aggregates, reconciles across all spoke domains, and emits the unified graph as Prometheus metrics. Transport is push for three reasons: BGP-LS (RFC 7752), stateful H-PCE (RFC 8751), and every SDN controller federation reviewed (ONOS, OpenDaylight) use push uniformly; push does not require the hub to know spoke addresses in advance, reducing configuration surface; and push communicates cycle completion directly — the hub knows a spoke's data is current because the spoke said so, not because the hub polled at a moment that may fall mid-cycle. The hub is a pure aggregator: it does no local SNMP discovery. This matches the H-PCE parent PCE model and avoids making the hub a bottleneck on both the aggregation and discovery paths.

`role: spoke` runs inside `cmd/topology-exporter`, same as `standalone`/`uncoordinated`. `role: hub` runs in a separate binary, `cmd/topology-hub` (`internal/federationhub`): operators running a hub switch to that binary/image instead of setting `federation.role: hub` on `cmd/topology-exporter`, which now rejects that role at startup with a pointer to `cmd/topology-hub`. This is a deployment-shape change, not a behavior change — `cmd/topology-hub` ships every capability `role: hub` had before the split, including the opt-in native-HA leader election below, verbatim.

**LD-17: Spoke pre-reconciles before pushing.**

Each spoke runs `graph.Reconcile` on its local domain edge set before pushing to the hub. The hub concatenates one reconciled edge set per spoke domain and runs a second Reconcile pass across the combined set. Cross-boundary bidirectionality fires on the hub's pass. The alternative — pushing raw unreconciled edges — would give the hub visibility into intra-domain protocol conflicts at the cost of significantly larger payloads and no improvement to cross-boundary detection, which is the primary motivation for hub/spoke deployment. RFC 6805's H-PCE architecture makes the same trade-off: child PCEs pre-process their domain topology before reporting to the parent, which sees domain summaries rather than raw LSDB. The information loss (intra-domain protocol conflicts are not visible to the hub) is acceptable because `graph.Reconcile` is deterministic and the spoke's `network_topology_conflict_total` counter surfaces disagreements locally.

**LD-18: Spoke liveness and link liveness are distinct failure modes.**

Spoke eviction — removing a silent spoke's edges from the hub's accumulated edge set — is governed by `federation.spoke_timeout`, not by `discovery.unconfirmed_link_ttl_cycles`. These address different failures: LD-14's unconfirmed-link TTL handles a link that stops being reported within an active domain; spoke timeout handles a collection domain going entirely silent. Conflating them would age out all of a silent spoke's edges at the same rate as a single unstable link, masking the spoke-failure signal and producing slow partial-topology degradation rather than a clear boundary-domain outage. RFC 8685 §5.3 identifies this conflation as a failure mode in early H-PCE implementations.

**LD-19: Known inter-domain links for stitching fallback.**

Automatic cross-boundary stitching — in both uncoordinated and hub/spoke modes — relies on matching `dst_hint` values across instances. RFC 8795 (YANG TE Topology, Liu et al., IETF 2021) introduces `inter-domain-plug-id` specifically because sysName-based matching fails in practice: one side may report a chassis-ID, the other a hostname, and they never match. Real multi-domain deployments — including Facebook's Express Backbone (Engineering Blog, 2017) — handle this with pre-configured per-link identifiers for known boundary ports rather than relying purely on auto-discovered name correlation. `federation.known_inter_domain_links` serves the same purpose: a list of `{local_device, local_port, remote_device, remote_port}` tuples the hub and uncoordinated recording rule treat as authoritative stitching overrides, regardless of what automatic name-matching produces. Operators configure these once for boundary ports with naming inconsistencies; the automatic path handles everything else. Absent this configuration, cross-boundary link stitching is best-effort and may silently produce no edge rather than a wrong edge.

**LD-20: Hub/spoke channel requires mutual TLS.**

The hub's `/spoke/push` HTTP endpoint accepts a `SpokePayload` that passes through `graph.Reconcile` and is then emitted directly as Prometheus metrics. Without transport-layer authentication, any host that can reach the hub's listen port can POST fabricated device and edge data — topology poisoning with no detection path, since the hub has no way to distinguish a legitimate spoke payload from a crafted one after the fact. A large fabricated payload also triggers unbounded growth in the Prometheus registry (one GaugeVec label tuple per fabricated edge), producing an out-of-memory kill vector. Spoke impersonation — a rogue spoke using a legitimate spoke's identifier — silently replaces correct topology data. The minimum viable protection is mutual TLS: each spoke presents a client certificate signed by an operator-controlled CA; the hub verifies the full certificate chain before processing the payload and rejects the connection at the TLS handshake if verification fails. Bearer token authentication in the `Authorization` header is operationally simpler but is vulnerable to credential theft on compromised management networks; for a component that directly governs what appears in network topology metrics and therefore what alerts fire, mTLS is the appropriate minimum. `federation.hub.tls_ca_cert`, `federation.hub.tls_cert`, and `federation.hub.tls_key` control hub-side TLS; `federation.spoke.tls_ca_cert`, `federation.spoke.tls_cert`, and `federation.spoke.tls_key` control the spoke client certificate. Both sides must be configured; the hub rejects plaintext connections.
