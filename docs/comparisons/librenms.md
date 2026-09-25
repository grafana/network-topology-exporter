# `network-topology-exporter` vs LibreNMS topology discovery

**Date:** 2026-05-22
**Scope:** Topology discovery only. LibreNMS is a full network management
system; this comparison is restricted to the topology-discovery code path
in each project.

LibreNMS is a full network management system; topology discovery is one of
~50 modules it owns. This exporter is single-purpose: discover, reconcile,
emit a topology graph and nothing else. The comparison has to be careful —
they're not the same shape of thing. The fair scope is just the
topology-discovery code path in each.

## 1. TL;DR

| Axis | network-topology-exporter | LibreNMS topology |
|---|---|---|
| Tech stack | Go 1.26, `gosnmp`, Prometheus client, OTLP SDK | PHP 8 + Laravel 11, `gosnmp` via `net-snmp` CLI, Eloquent ORM, RRD |
| Runtime model | Single binary, persistent process, in-memory graph + on-disk snapshot | Cron-driven shell-out (`discovery.php` every 6 h), state lives in MySQL |
| Topology data store | In-memory `map[Key]Edge` per cycle, JSON snapshot for cold start (`internal/snapshot/`) | `links` table in MySQL (`database/migrations/2018_07_03_091314_create_links_table.php`) |
| Output | `/metrics` scrape + structured JSON logs + optional OTLP push | MySQL rows + RRD time-series → server-side Blade view + VisJS client |
| Visualization | Decoupled (Grafana node-graph panel on the metrics) | Built-in `/maps/network` page, VisJS rendering |
| Discovery cycle | Default 60 s, every cycle is a full walk | Default 6 hours, full walk |
| Vendor dispatch | Canonical-vendor switch with shared MIB walker (`vendorSpecFor` in `internal/discovery/bgp/bgp_vendor.go`); falls back to vendor-neutral RFC walker | `if/elseif ($device['os'])` chain per module (`includes/discovery/discovery-protocols.inc.php`) |
| Reconciliation | Edge-diff + 2-source conflict detection + stale-edge aging (`internal/graph/graph.go`) | Hard-delete rows not seen in current run; no cross-protocol conflict surfacing |
| Federation | mTLS spoke/hub with payload validation (`internal/federation/` spoke, `internal/federationhub/` hub — split into separate binaries, `docs/proposals/core-hub-split.md`) | Shared MySQL + Redis + per-device `poller_group`; no inter-instance sync |
| License | AGPL-3.0 | GPL v3 |
| Topology LOC | ~5,000 (excl. tests) | ~3,000 (xdp + supporting code) |
| Tests | ~20,000 LOC, ~1.8× the implementation | Some, mostly integration via fixture-driven `tests/` |

## 2. Architecture & deployment model

**LibreNMS** is a "deploy a server" affair: PHP-FPM, MariaDB, Redis,
RRDCached, optionally Memcached. Discovery is a cron-driven CLI invocation
(`discovery-wrapper.py 1` at `33 */6 * * *` in `dist/librenms.cron`) which
spawns a configurable number of PHP worker processes. Each worker calls
`app/Console/Commands/DeviceDiscover.php` → `PerDeviceProcess` →
`DiscoverDevice` job. Discovery is shared-nothing across pollers but they
all hit the same MySQL.

**This exporter** is a single Go binary. One process, one config file,
persistent across cycles, `/metrics` HTTP endpoint scraped by Prometheus.
There's no separate "discover vs poll" split — every cycle is a full
discovery, and the output IS the metrics. Federation is opt-in mTLS
spoke→hub aggregation, not a poller-pool model.

Practical consequence: a LibreNMS install on a small lab is ~600 MB of
deps and brings up four daemons. The exporter is ~18 MB, one binary, no DB.

## 3. Discovery scheduling

LibreNMS:

- Cron entry: `33 */6 * * *` → `discovery-wrapper.py 1`. Default 6 h interval.
- New-device scan separately at `*/5` → `lnms device:discover new`.
- Per-device module selection has a four-level override cascade
  (`LibreNMS/Polling/ModuleStatus.php`): CLI flag → device attribute → OS
  yaml → global config. `core` always runs first.
- Implication: any change in topology takes up to 6 h to appear in the UI
  by default. Operators tune this manually.

This exporter:

- `discovery.interval` defaults to **60 s** (`config/example.yaml` ships the
  default value), hard guarded by a per-cycle deadline of
  `interval × discovery.cycle_budget_fraction` (default 0.8) via
  `context.WithDeadline` in `internal/app/cycle.go`.
- Per-device parallelism bounded by `discovery.parallelism` (default 32;
  `config/example.yaml` overrides it to 20) via a semaphore.
- Per-device panic recovery on the worker goroutine.
- No module ordering machinery — the per-device walker just iterates the
  enabled modules.

The 60 s vs 6 h gulf is the cleanest single-number contrast: LibreNMS
optimises for "100s of devices, slow change" while this exporter optimises
for "metrics endpoint that reflects current state".

## 4. Protocol coverage

| Protocol | network-topology-exporter | LibreNMS |
|---|---|---|
| LLDP (IEEE 802.1AB) | ✅ `internal/discovery/lldp/lldp.go` — single RFC-conformant walker, all 7 chassis-id subtypes + 7 port-id subtypes | ✅ default branch in `discovery-protocols.inc.php` + LLDP-V2 → V1 field-name remapping |
| CDP (Cisco) | ✅ `internal/discovery/cdp/cdp.go` | ✅ Cisco-only branch in `discovery-protocols.inc.php` |
| BGP | ✅ `bgp.go`, `bgp_v2.go`, `bgp_vendor.go` — 4 vendor walkers (Cisco cbgpPeer2, Arista bgp4v2, Juniper jnxBgpM2, Nokia tBgp) + RFC fallback | ❌ no topology-relevant BGP module; only sensors/counters |
| OSPFv2 | ✅ `internal/discovery/ospf/ospf.go` | ✅ `LibreNMS/Modules/Ospf.php` — but poll-only, not topology |
| OSPFv3 | ❌ (yet — there's a plan at `plans/bgp4v2-ipv6.md`) | ✅ `Ospfv3Nbr` model |
| IS-IS | ✅ `internal/discovery/isis/isis.go` | partial — sensor-level only |
| MPLS-TE | ✅ implied by README ("MPLS-TE discovery") | ❌ |
| FDB (Q-BRIDGE) | ✅ `internal/discovery/fdb/fdb.go` — handles per-VLAN community walks, 7-byte MAC, max_vlans cap | ✅ `includes/discovery/fdb-table/bridge.inc.php` — also handles 7-byte MAC, but per-OS branching for IOS/IOSXE/NX-OS |
| FDP (Brocade) | ❌ | ✅ Brocade-specific branch |
| ARP / ipNetToMedia | partial (used as enrichment, not first-class) | ✅ `LibreNMS/Modules/ArpTable.php` writes to `ipv4_mac` table |
| Vendor proprietary LLDP variants | ❌ — strict RFC compliance; non-conforming devices skipped with debug log | ✅ Mikrotik, TPLink JetStream, Nokia TIMETRA, PBN/BDCOM (≈7 vendor-specific OID branches in xdp) |

LibreNMS wins on vendor breadth. This exporter wins on protocol depth and
conformance — every BGP walker is structurally rewritten from device
captures (`lab/cisco-iosxe-bgp/`) with adversarial tests for `walker_drift`
etc.

## 5. Vendor dispatch — the code-style fault line

LibreNMS (`includes/discovery/discovery-protocols.inc.php`):

```php
} elseif ($device['os_group'] == 'cisco') {
    $cdpsample = snmpwalk_cache_oid(...);
} elseif ($device['os'] == 'routeros' && version_compare($device['version'], '7.7') < 0) {
    $lldp_array = snmpwalk_cache_threepart_oid(...);
} elseif ($device['os'] == 'timos') {
    ...
}
```

Linear `elseif` chain. Adding a vendor = adding a branch. Branches share
little code. The author of each branch picks their own data shape, then
maps to the common `discover_link()` writer.

This exporter (`vendorSpecFor` in `internal/discovery/bgp/bgp_vendor.go`):

```go
func vendorSpecFor(vendor string) *vendorTableSpec {
    switch vendor {
    case "cisco":          return &ciscoCbgpPeer2Spec
    case "arista":         return &aristaBgp4v2Spec
    case "juniper":        return &juniperJnxBgpM2PeerSpec
    case "nokia", "alcatel-lucent": return &nokiaTBgpPeerSpec
    default:               return nil
    }
}
```

The dispatch returns a *spec* (a `vendorTableSpec` value with OIDs + index
decoder + outcome predicates), then a shared walker drives all four.
Adding a vendor = registering one more `vendorTableSpec`. The walker code
stays put.

The trade-off: LibreNMS's pattern is faster to add an "obviously
different" vendor branch (no need to fit it into a shared spec), but rots
fast — branches diverge in error handling and naming over time. The
exporter's pattern is stricter to extend (you must conform to the spec)
but the result is consistent: `bgp.go` has one walker, four vendors, named
outcome constants (`outcome=walker_drift` vs `outcome=no_peers`), and one
set of tests that exercises all of them
(`internal/discovery/bgp/bgp_outcome_test.go`).

## 6. LLDP walker — same MIB, different code

Both walk `1.0.8802.1.1.2.1.4.1.1.*` (lldpRemTable). The exporter does it
once, RFC-style:

```go
// internal/discovery/lldp/lldp.go — walkRemEntries
func walkRemEntries(ctx, client) (map[remKey]*remEntry, error) {
    pdus, _ := snmputil.BulkWalk(ctx, client, oidRemTable)
    for _, pdu := range pdus {
        // suffix: col.timeMark.portNum.remIndex  — IEEE 802.1AB §9.5.5
        col, rest, _ := SplitOIDComponent(suffix)
        _, rest, _ := SplitOIDComponent(rest)        // skip timeMark
        portNum, remStr, _ := SplitOIDComponent(rest)
        ...
    }
}
```

The walker validates every value before storing (see `walkRemEntries` in
`lldp.go`): the chassis-id subtype must be 1–7, MAC chassis IDs must be
exactly 6 bytes, and network-address chassis IDs must have a sane IANA address
family byte. Out-of-spec PDUs are skipped with a counter increment
(`ReportDecodeIssue` with reasons like `chassis_subtype_invalid`,
`chassis_mac_bad_length`), not silently dropped.

LibreNMS (`discovery-protocols.inc.php` LLDP branch):

```php
$lldp_array = snmpwalk_cache_oid($device, 'lldpRemTable', [], 'LLDP-MIB');
foreach ($lldp_array as $key => $lldp_if_array) {
    foreach ($lldp_if_array as $entry_key => $lldp_instance) {
        // ... mostly direct array access, no schema validation ...
    }
}
```

It trusts the SNMP walk's output and depends on the upstream `snmpwalk`
CLI for MIB resolution. Validation happens implicitly via PHP type
juggling; out-of-spec values land in the DB as-is and surface later as
"weird links in the UI". The bug rate on edge cases (non-MAC chassis IDs,
ifAlias port IDs, manufacturer's-creative-interpretation port
descriptions) is visibly higher in LibreNMS — `git log` on
discovery-protocols.inc.php has years of "fix lldp for vendor X" commits.

## 7. Data model

LibreNMS `links` table:

```
id, local_port_id, local_device_id, remote_port_id (nullable),
active (bool, schema-present but unused), protocol (varchar(11)),
remote_hostname, remote_device_id (NOT NULL, 0 if unresolved),
remote_port, remote_platform, remote_version
```

Each row is one *directional* observation from one device's perspective.
The same physical link appears twice (once per endpoint's local
discovery). Reconciliation is implicit — both sides are visible to the UI,
no cross-correlation.

This exporter's `discovery.Edge` (`internal/discovery/discovery.go`):

```go
type Edge struct {
    SrcDevice      string
    SrcPort        string
    DstDevice      string
    DstPort        string
    DiscoveryProto DiscoveryProtocol // lldp | cdp | bgp | ospf | fdb | isis | mpls_te | configured
    Direction      Direction
    Confidence     Confidence
    Adjacency      Adjacency
    PrecedenceRank int               // 0 = highest priority
    LinkKind       LinkKind          // ethernet | ip | mpls-te | logical
    ObservedAt     time.Time         // cycle this edge was emitted
    Metadata       map[string]string // protocol-specific extras, nil when none
}
```

There is no `FirstSeen`/`LastSeen`/`UnconfirmedAge`/`Sources` field on the
discovery `Edge`: the discovery layer emits a single `ObservedAt` timestamp
per cycle, and the unconfirmed-link aging counter is graph-layer state, not a
field on the struct.

Two-source reconciliation: `internal/graph/graph.go` merges per-protocol
observations and tags `direction = bidirectional` only when both
endpoints' walks confirm the same edge in a single cycle. A conflict is
emitted when two protocols give different neighbour names for the same local
port, which gets a Prometheus counter
(`network_topology_conflict_total{conflict_type="neighbour_disagreement"}`).

That conflict-detection layer is a real semantic gap from LibreNMS —
there, if CDP and LLDP say different things about a port, both rows just
land in the table and the UI shows whichever was queried first.

## 8. Reconciliation, conflict, staleness

LibreNMS:

- Stale links: hard-deleted at the end of every discovery run
  (`Link::where('id', $test['id'])->delete()` in the discovery-protocols
  module). No grace period. If a device is unreachable mid-discovery, all
  its links vanish from the next cycle.
- Orphan links: `Link::doesntHave('device')->delete()` runs in the same path.
- No "edge added" / "edge removed" event surface. Changes are visible only
  by diffing two snapshots of the DB.

This exporter:

- Soft-aging via `discovery.unconfirmed_link_ttl_cycles` (default 3) — a
  unidirectional link is retained for that many discovery *cycles* (tracked
  by the graph layer's internal `UnconfirmedCycles` counter, not a duration)
  before removal. Aging is measured in cycles, not wall-clock time.
- Change events: every transition emits a structured JSON log line with
  before/after edge records:

  ```json
  {"msg":"topology change","change_kind":"removed","before_src_device":"core-sw-01",...}
  ```

- Prometheus counter
  `network_topology_change_total{change_kind="added|removed|updated"}`
  increments per cycle.
- Snapshot on disk (`internal/snapshot/`) survives restart so `/metrics`
  serves the previous graph immediately — no "blank window" during a
  cold-start cycle.

LibreNMS has no equivalent of any of these. The change-event surface is
the single biggest argument for the exporter when downstream tools care
about "what changed in the last hour".

## 9. Output / visualization

LibreNMS:

- Storage is the visualization. The PHP server queries `links`, renders a
  Blade view, and ships data to a VisJS frontend
  (`app/Http/Controllers/Maps/NetMapController.php`). The map is a
  *deliverable* of LibreNMS.
- Per-device "neighbours" tab visible iff `Link::where('local_device_id',
  $id)->exists()`.
- Geo-maps grouped by `device.location` lat/lng.

This exporter:

- No built-in UI. Outputs metrics; Grafana's node-graph panel (or any
  other consumer) is the visualization. The repo ships curated dashboards
  at `dashboards/test-harness/topology-graph.json`.
- Three output channels in parallel: Prometheus `/metrics` (pull),
  structured JSON logs to stderr (push to any log aggregator), optional
  OTLP push (push to any OTLP receiver).
- This decoupling is the project's thesis — "topology data in formats
  Prometheus/Grafana already understand", per the README.

If you already run Grafana, the exporter's no-UI approach is a feature.
If you don't, LibreNMS's batteries-included rendering is.

## 10. Federation / scale

LibreNMS:

- Shared MySQL + Redis + RRDCached. Pollers shard by
  `device.poller_group`. Redis coordinates ownership at runtime.
  Documented ceiling: ~1,000 devices per poller.
- Not federation in the strict sense — pollers don't talk to each other.
  They share state through the DB.
- No mTLS between pollers and the central store; depends on the network
  being trusted.

This exporter:

- mTLS hub/spoke (`internal/federation/` for the spoke side, shipped in
  `cmd/topology-exporter`; `internal/federationhub/` for the hub side,
  shipped in the separate `cmd/topology-hub` binary — see
  `docs/proposals/core-hub-split.md`). Spokes push their reconciled
  per-CIDR graph to a hub on `:9101/spoke/push`. Hub re-reconciles across
  all spokes, surfaces unified metrics.
- Spoke push validates client cert via `RequireAndVerifyClientCert` +
  SANs allow-list (`docs/operator/federation.md`).
- "Uncoordinated" mode is also documented — no hub, each spoke emits
  `boundary_observation_info` and a Mimir recording rule does the
  cross-instance match.
- Single instance handles "a contiguous CIDR range" — explicit scale
  ceiling, no claimed ceiling like 1000 devices.

The exporter's federation is meaningfully more secure (mTLS enforced) but
more opinionated (one cidr per spoke; one role per process).

## 11. Credential management

LibreNMS: per-device SNMP credentials stored encrypted (Laravel
`Crypt::encrypt()`) in `devices.snmp_v3_authpass` / `community` columns.
Decrypted at use-time in PHP memory. No automatic rotation. PHP's GC
reclaims when the variable goes out of scope; no explicit zeroization.

This exporter: profile-based credentials with per-IP cache and a
token-bucket rate limiter (`internal/credentials/`), and an explicit
`Zeroize()` on the `snmpwalk.Params` after every walk — the method is
defined in `internal/discovery/snmp/zeroize.go` and invoked from the cycle
runner in `internal/app/probe.go`. Every credential byte slice is
overwritten with zeros before the goroutine exits. Snapshot persists only
the profile *name* per IP, never plaintext. The "cold-start credentials"
doc (`docs/operator/cold-start-credentials.md`) is dedicated to the
auth-flood problem on restart.

LD-12 (the credential resolver) is one of the cleanest single-purpose
subsystems in the exporter. LibreNMS has no equivalent — credential
failures fan out as unbounded SNMP retries.

## 12. Code quality & supply chain

| | exporter | LibreNMS |
|---|---|---|
| Static analysis | golangci-lint, configured per-repo, gates CI | PHP-CS-Fixer for style; PHPStan optional |
| Test ratio (LOC) | 1.78× (19,809 test : 11,178 impl) | unclear, mostly integration |
| Fuzz/property tests | a couple of fuzz seeds in `internal/discovery/bgp/` | none |
| Panic recovery | per-device `recover()` with metric counter | n/a — PHP exception model |
| Audit history | `docs/audits/2026-05-architectural-review.md` + REMEDIATION.md tracking | issue tracker driven |
| Supply chain | `go.sum`, no vendored deps, SBOM generated in the release workflow (anchore/sbom-action, SPDX) | composer.lock + composer audit; PHP ecosystem larger and noisier |
| License | AGPL-3.0 (copyleft, incl. network/SaaS use) | GPL v3 (copyleft) |

The exporter is post-audit code: the May 2026 review
(`docs/audits/2026-05-architectural-review.md`) drove 28 numbered
remediation items. LibreNMS hasn't had a comparable structured
audit publicly.

## 13. When to choose which

**Choose LibreNMS for topology when:**

- You don't have Grafana already
- You need a single product to do metrics, syslog, alerting, AND topology
- Your network is mostly known vendors and you want the UI for free
- You're OK with 6-hour discovery cadence and MySQL-backed state
- GPL v3 is acceptable in your environment

**Choose this exporter when:**

- You already run Prometheus/Mimir/Grafana
- You need topology data as a *signal* alongside other metrics, not as a UI
- You care about RFC conformance and explicit conflict detection
- You're discovering large CIDRs that want sharded federation
- Vendor proprietary protocols (Mikrotik LLDP variants, TIMETRA-LLDP,
  JetStream) are not in your fleet
- You need MPLS-TE topology (LibreNMS doesn't do it)
- AGPL-3.0 (strong network-use copyleft) is acceptable in your environment

## 14. What each would steal from the other

If LibreNMS were starting over with this exporter as a reference, they'd want:

- The vendor-spec dispatch pattern (`bgp_vendor.go`)
- The change-event log surface
- The conflict-detection layer
- Credential zeroization

If this exporter were to absorb a LibreNMS-style design point, the obvious
one is **vendor proprietary LLDP variants**. The strict RFC-only LLDP
walker silently drops a lot of real-world devices (Mikrotik, JetStream,
some Aruba). A `vendorSpecFor()`-style dispatch in
`internal/discovery/lldp/` to add a Mikrotik branch and a TIMETRA branch
would close a real gap without violating the project's "clean room of MIB
walkers" thesis.

## Sources

- LibreNMS docs: [Discovery](https://docs.librenms.org/Support/Discovery/), [Network Map](https://docs.librenms.org/Extensions/Network-Map/), [Distributed Poller](https://docs.librenms.org/Extensions/Distributed-Poller/)
- LibreNMS source: [discovery.php](https://github.com/librenms/librenms/blob/master/discovery.php), [discovery-protocols.inc.php](https://github.com/librenms/librenms/blob/master/includes/discovery/discovery-protocols.inc.php), [ArpTable.php](https://github.com/librenms/librenms/blob/master/LibreNMS/Modules/ArpTable.php), [Ospf.php](https://github.com/librenms/librenms/blob/master/LibreNMS/Modules/Ospf.php), [links migration](https://github.com/librenms/librenms/blob/master/database/migrations/2018_07_03_091314_create_links_table.php)
- This repo: `internal/discovery/lldp/lldp.go`, `internal/discovery/bgp/bgp_vendor.go`, `internal/graph/graph.go`, `internal/federation/`, `internal/federationhub/`, `docs/audits/2026-05-architectural-review.md`
