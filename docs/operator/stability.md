# Stability Matrix

This document is the authoritative statement of which surfaces are frozen, the semver contract that applies to each, and the measurable criteria for operational readiness. It is written for operators who need to decide whether to pin a version or upgrade freely, and for contributors who need to know what constitutes a breaking change.

Cross-references:

- Metric list: [`docs/metrics.md`](../metrics.md)
- Config key reference: [`config/example.yaml`](../../config/example.yaml)
- Roadmap and release plan: [`ROADMAP.md`](../../ROADMAP.md)
- Contribution rules: [`CONTRIBUTING.md`](../../CONTRIBUTING.md)

---

## Stability from v1.0.0

As of **v1.0.0** (2026-06-18), the five surfaces below are semver-stable.
Breaking changes require a **major version bump** (`v2.0.0`). Additive changes
(new optional config keys, new metrics) are minor-version changes.

See [ROADMAP.md](../../ROADMAP.md) for forward-looking work.

### 1. Config schema

The frozen artefact is `config/example.yaml`. Every key documented there is part of the contract.

| Key group | Representative keys |
|---|---|
| Discovery controls | `discovery.interval`, `discovery.timeout_per_device`, `discovery.timeout_per_module`, `discovery.parallelism`, `discovery.cycle_budget_fraction`, `discovery.scope.cidr_allow_list`, `discovery.unconfirmed_link_ttl_cycles`, `discovery.max_graph_devices`, `discovery.max_graph_edges`, `discovery.per_target_pdu_rate_per_second` (#72), `discovery.target_overrides` (#74), `discovery.snmp.session_pool.{enabled,max_idle}` (#83) |
| Module toggles | `modules.{lldp,cdp,bgp,ospf,fdb,arp,isis,mpls_te}.enabled` |
| BGP walker | `modules.bgp.disable_v2_mib` |
| Credentials | `credentials.profiles[].{name,type,community_env,username_env,auth_protocol,auth_key_env,priv_protocol,priv_key_env}`, `credentials.assignments`, `credentials.fallback_order`, `credentials.trial_rate_per_second` |
| Snapshot | `snapshot.path` |
| Federation | `federation.role`, `federation.spoke_timeout`, `federation.known_inter_domain_links`, `federation.hub.*`, `federation.spoke.*` |
| Output | `output.otlp.{enabled,endpoint,timeout,heartbeat_cycles,protocol}`, `output.otlp.traces.{enabled,sample_rate}` |
| Listen | `listen.addr`, `listen.web_config_file`, `listen.debug_listen_addr` (#69) |
| Targets | `targets[].{host,port,site,labels}` |

**Breaking change** means: a key is renamed, removed, or its accepted value set changes in a way that requires operator action before upgrade. Additive changes (new optional keys with sensible defaults) are not breaking.

**Key naming history:** the BGP walker toggle is `modules.bgp.disable_v2_mib` and the hub name-matching toggle is `federation.hub.loose_device_name_matching`. TLS on the main listener is configured via `listen.web_config_file` (exporter-toolkit web-config); there is no `listen.tls_cert_file`/`listen.tls_key_file` key. Config is parsed with `KnownFields(true)`, so an unrecognised key is a hard startup failure rather than a silent no-op.

### 2. Metric names and label sets

The frozen artefact is the table in [`docs/metrics.md`](../metrics.md). Every metric name and label set documented there is part of the contract.

Key metrics (summary — full details in `docs/metrics.md`):

| Metric | Type | Frozen label set |
|---|---|---|
| `network_topology_device_info` | gauge | `device_id`, `vendor`, `model`, `os_version`, `site`, `sys_name` (added in issue #227 — **breaking**, see CHANGELOG) |
| `network_topology_device_uptime_seconds` | gauge | `device_id` |
| `network_topology_edge_info` | gauge | `src_device`, `src_port`, `src_if_index`, `dst_device`, `dst_port`, `dst_if_index`, `discovery_proto`, `link_kind`, `direction` (`src_if_index`/`dst_if_index` added in issue #227 — **breaking**, see CHANGELOG) |
| `network_topology_change_total` | counter | `change_kind`, `discovery_proto` |
| `network_topology_out_of_scope_neighbours_total` | gauge | (none) |
| `network_topology_graph_stale` | gauge | (none) |
| `network_topology_snapshot_last_written_timestamp_seconds` | gauge | (none) |
| `network_topology_discovery_cycle_duration_seconds` | histogram | (none) |
| `network_topology_discovery_module_duration_seconds` | histogram | `module` |
| `network_topology_bgp_walker_outcome_total` | counter | `walker`, `outcome` |
| `network_topology_walker_outcome_total` | counter | `walker`, `outcome` (#98) |
| `network_topology_system_walk_anomaly_total` | counter | `reason` (#101) |
| `network_topology_snmp_rate_limit_wait_seconds` | histogram | (none) (#72) |
| `network_topology_snmp_session_pool_size` | gauge | (none) (#83) |
| `network_topology_snmp_session_pool_hits_total` | counter | (none) (#83) |
| `network_topology_snmp_session_pool_misses_total` | counter | (none) (#83) |
| `network_topology_snmp_session_pool_evictions_total` | counter | `reason` (#83) |
| `network_topology_federation_spoke_up` | gauge | `spoke_id` |
| `network_topology_federation_spoke_push_failures_total` | counter | (none) |
| `network_topology_otlp_push_total` | counter | `status`, `reason` |

**Breaking change** means: a metric is renamed, removed, a label is renamed or its value set changes in an incompatible way, or a metric's type changes. Adding a new label (widening the series fingerprint) is breaking. Adding a new metric is not.

Adding a `reason` label value within an existing closed enum (e.g. a new `network_topology_bgp_walker_outcome_total{outcome=...}` value) is treated as potentially breaking if existing alerts use a negation match — the CHANGELOG entry will note it.

### 3. CLI flags

The frozen artefact is `--help` output and the README quickstart. The four defined flags are:

| Flag | Default | Meaning |
|---|---|---|
| `--config.file` | `/etc/topology-exporter/config.yaml` | Path to the YAML configuration file |
| `--web.listen-address` | `:9100` | Address on which to expose `/metrics`, `/healthz`, and `/readyz` |
| `--log.level` | `info` | Log level: `debug \| info \| warn \| error` |
| `--version` | — | Print version string and exit |

**Breaking change** means: a flag is renamed or removed, or its semantics change in a way that requires operator action. Adding new flags with sensible defaults is not breaking.

### 4. Snapshot format

The frozen artefact is the JSON schema of `snapshot.json`. The current schema version is **3** (`internal/snapshot/snapshot.go: CurrentVersion = 3`).

The schema version is written to every snapshot file as `"version": 3`. The loader rejects files with a version number it does not recognise; it does not attempt a silent migration. At v1.0 GA:

- **Minor version bumps** may add optional fields to the schema. The loader handles unknown fields gracefully (Go's `encoding/json` ignores them by default). Version number remains `3` for additive changes.
- **Schema version increments** (4, 5, …) indicate that reading the file with an older binary is not supported. A migration path (if one exists) is documented in the CHANGELOG entry for the release that bumps the version.
- A downgrade from a binary that wrote version `N` to one that only understands version `N-1` will result in the older binary refusing to load the snapshot and cold-starting from zero. The CHANGELOG will call this out as a breaking change.

**Breaking change** means: the schema version is incremented, or a previously required field is removed or changes meaning.

### 5. Federation API

The frozen artefact is the `SpokePayload` wire type (`internal/federation/payload.go`) and the HTTP contract for `POST /spoke/push` on the hub's mTLS listener.

Current federation API shape:

| Aspect | Value |
|---|---|
| Endpoint | `POST /spoke/push` on `federation.hub.listen_addr` (default `:9101`) |
| Transport | mTLS — client cert required, verified against `federation.hub.tls_ca_cert` |
| Payload | `SpokePayload` serialised as JSON |
| Reject response | HTTP 413 (size budget) / 409 (stale generation) / 400 (invalid label or structural), with `{"reason": "...", "detail": {...}}` JSON body |
| Accept response | HTTP 204 (no body) |
| `SpokePayload` fields | `spoke_id` (string), `cycle_at` (RFC 3339), `devices` (array), `edges` (array), `out_of_scope` (array), `ages` (map) |

There is no explicit federation API version number in the wire format today. This is a known gap — a `"federation_api_version"` field is planned before v1.0 GA to allow the hub to reject incompatible spoke payloads with a clear error rather than a validation failure. Until then, spoke and hub **must** run the same minor version.

**Breaking change** means: a required field is added, removed, or its type changes; the HTTP status codes for accept/reject change; the mTLS contract changes (e.g. new required SAN validation).

---

## Structural commitments

Three commitments from the ROADMAP that have measurable acceptance criteria:

### Every discovery walker is real-device validated

**What it means:** Synthetic-only test coverage (PDU streams hand-crafted from MIB documents) is insufficient for v1.0 GA. Each walker must have a `lab/` directory with captures from real hardware or a supported containerlab image.

**Current status:**

| Walker | Validation status |
|---|---|
| Cisco IOL / IOS-XE (cbgpPeer2Table) | Real-device validated (`lab/cisco-iol-bgp/`, `lab/cisco-iosxe-bgp/`) |
| Arista cEOS (enterprise BGP4V2) | Real-device validated (`lab/arista-ceos-bgp/`) |
| Nokia SR-OS (tBgpPeerNgTable) | **Validated** against SR-OS 25.7.R2 (#57) |
| Juniper Junos (jnxBgpM2PeerTable) | Validated against vJunos-router JUNOS 25.4R1.12 (#56) |
| LLDP (IEEE 802.1AB) | Real-device validated via containerlab E2E (`make test-e2e`) |
| CDP (CISCO-CDP-MIB) | Synthetic tests; real-device captures not yet landed |
| OSPF, IS-IS, FDB, MPLS-TE | Synthetic tests; real-device captures not yet landed |

**Acceptance criterion:** all walkers in the table above show "real-device validated" before the v1.0.0 banner is flipped.

### No silent failure modes

**What it means:** Every external dependency the exporter relies on (per-vendor MIB shape, per-protocol assumption) has either a hard-fail contract or a `network_topology_*_outcome_total` counter labelled with the specific failure shape. Operators can alert on degradation rather than discovering it through dashboard absence.

**Acceptance criterion:** `docs/metrics.md` has an entry for every failure counter, and each entry names the alert expression an operator should use.

### Operator runbook is sufficient for a new operator to deploy, alert, and troubleshoot

**What it means:** An operator who has never read the source code should be able to deploy the exporter, configure basic alerts, and diagnose common problems using the documentation in `docs/operator/` and the referenced runbooks.

**Acceptance criterion:** `docs/operator/upgrades.md`, `docs/operator/slos.md`, this stability matrix, and the failure-mode coverage audit are cross-referenced from the README.

---

## Deprecation policy

Breaking changes to any semver-stable surface require a **major version bump**.
Within a major version:

1. **Minimum overlap:** a deprecated config key, metric label value, or CLI flag must remain functional for at least **one full minor release** after the deprecation is announced.
2. **Startup warning:** deprecated config keys emit a `WARN`-level structured log line on startup, naming the deprecated key, the replacement, and the release in which the deprecated form will be removed.
3. **CHANGELOG note:** the release that introduces the deprecation has a `### Deprecated` section entry; the release that removes the deprecated form has a `### Removed` section entry.
4. **No silent migration:** the exporter does not silently rewrite a deprecated config file. If a deprecated key is present and the exporter accepts it, it logs the deprecation warning and uses the deprecated behaviour unchanged. Operators migrate on their own schedule within the overlap window.

---

## Related documents

- [`ROADMAP.md`](../../ROADMAP.md) — forward-looking plan
- [`docs/metrics.md`](../metrics.md) — full metric reference with recommended alerts
- [`config/example.yaml`](../../config/example.yaml) — config schema reference
- [`CONTRIBUTING.md`](../../CONTRIBUTING.md) — what constitutes a breaking change for contributors
- [`docs/supported-platforms.md`](../supported-platforms.md) — per-vendor walker validation status
