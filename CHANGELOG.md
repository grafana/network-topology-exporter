# Changelog

All notable changes are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project adheres to
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **Breaking:** `sys_name` label/attribute on `network_topology_device_info`
  and OTLP device metrics — the case-preserving sysName, alongside the
  existing lowercased `device_id`. Joins against
  Alloy/`prometheus.exporter.snmp`'s own case-sensitive `sysName` label. Per
  `docs/operator/stability.md`'s stability policy, adding a label to a frozen
  Prometheus metric's label set is itself a breaking change (it widens the
  series fingerprint), even though `sys_name` is often empty. `docs/metrics.md`
  and `docs/operator/stability.md`'s frozen label-set table for
  `network_topology_device_info` are updated to match. (issue #227)
- **Breaking:** `src_if_index`/`dst_if_index` labels/attributes on
  `network_topology_edge_info` and OTLP edge metrics — the IF-MIB ifIndex for
  each endpoint, when the discovery protocol resolves one. Joins against
  snmp_exporter's ifIndex-keyed `if_mib` rows. Same breaking
  label-set-widening rationale as `sys_name` above; `docs/metrics.md` and
  `docs/operator/stability.md`'s frozen label-set table for
  `network_topology_edge_info` are updated to match. (issue #227)
- `device_id` OTLP attribute on the device gauge, matching the Prometheus
  label of the same name. Not breaking on its own — see "Deprecated" below
  for how this interacts with the existing `device` attribute. (issue #227)

### Changed

- **Breaking:** Federation uncoordinated mode's
  `network_topology_boundary_observation_info` now case-folds
  `peer_a`/`peer_b`/`reporting_device` at emission — fixes mixed-case fleets
  being unmatchable by a hand-written PromQL/Mimir recording rule (PromQL has
  no `lower()`). This metric's label *set* is unchanged; this is a
  value-semantics change to labels already in it, tagged breaking to match
  this project's own precedent for that category of change (see the
  `network_topology_bgp_walker_outcome_total` issue #27/#31 entries in this
  file's history). Migration: a recording rule, alert, or dashboard variable
  that filtered/joined on the pre-#227 mixed-case values (e.g.
  `reporting_device="Sw-1"`) must switch to the lowercased form
  (`reporting_device="sw-1"`) or match case-insensitively. The underlying
  graph snapshot and federation wire payload are unaffected; hub mode's own
  matching and collision-detection diagnostic are unchanged. (issue #227)

### Deprecated

- OTLP device gauge's `device` attribute is deprecated in favor of
  `device_id` (issue #227) — not removed this release. Per
  `docs/operator/stability.md`'s deprecation policy (minimum one full minor
  release of overlap), the exporter now emits **both** `device` and
  `device_id` on every `network_topology_device_info` OTLP data point, with
  identical values, so existing consumers reading `device` keep working
  unchanged. `device` is scheduled for removal in the next MAJOR version;
  migrate OTLP consumers to `device_id` during this window. See
  `docs/otlp-schema.md` for the attribute table and versioning policy.

### Fixed

- `docs/architecture.md` LD-15/LD-19: corrected two claims that overstated
  implemented behavior — LD-15 described a NormaliseName + chassis-ID/IP
  fallback chain for the boundary-observation neighbour hint that doesn't
  exist; LD-19 described `known_inter_domain_links` as consumed by both hub
  mode and the uncoordinated recording rule, but only hub mode actually
  applies it. (See `docs/proposals/snmp-exporter-label-alignment.md` for the
  full design rationale and migration notes.)
- `graph.Reconcile`'s canonical-order normalisation now swaps `SrcIfIndex`/
  `DstIfIndex` along with `SrcDevice`/`SrcPort`/`DstDevice`/`DstPort` — without
  this, roughly half of all edges would land the wrong device's ifIndex on the
  wrong side after reconciliation.
- Added `sys_name` to the field checks `internal/federation/hub_validate.go`
  and `internal/snapshot/snapshot.go` already run on `vendor`/`model`/
  `os_version`/`site` — it had the same UTF-8/length exposure as those fields
  but was missing from both checks.
- `internal/federation/hub_validate.go` now rejects a spoke payload edge with
  a `src_if_index`/`dst_if_index` outside `[0, limits.MaxIfIndex]` — previously
  only negative values were rejected, with no upper bound at all. The same
  bound is now enforced on every path that builds these labels, not just the
  federation hub-ingest path: the default single-instance path
  (`internal/metrics/topology_collector.go`'s `ifIndexLabel`, which degrades
  an out-of-range value to the same empty label the "unresolved" case already
  produces, since `Collect` implements `prometheus.Collector` and cannot
  return an error) and the on-disk snapshot loader
  (`internal/snapshot/snapshot.go`), which now independently re-checks
  `SrcIfIndex`/`DstIfIndex` the same way it already checks other edge fields'
  byte lengths. `limits.MaxIfIndex` (`internal/limits`) is the single shared
  bound all three sites import, matching this package's existing convention
  for `MaxDeviceIDBytes`/`MaxPortNameBytes`/etc.

### Internal

- `internal/discovery/isis`: replaced two parallel maps
  (`circIfNames`/`circIfIndexes`) with a single `map[string]circuitIf`, since
  they were always populated and looked up together.
- `internal/discovery/lldp/lldp.go`: documented that `SrcIfIndex: k.portNum`
  relies on `lldpLocPortNum` (LLDP-MIB / IEEE 802.1AB) being equivalent to the
  IF-MIB `ifIndex` — an equivalence 802.1AB only recommends, not guarantees,
  and one this codebase has not yet validated against a real multi-vendor
  fleet. Flagged as worth validating before treating LLDP-sourced
  `src_if_index`/`dst_if_index` as an authoritative join key for dashboards or
  alerts. Documentation only; no behavior change.

## [1.0.0] - 2026-06-18

Initial release under the [Grafana](https://github.com/grafana) organization.

### Added

- SNMP discovery for LLDP, CDP, BGP (RFC 4273 + vendor MIBs), OSPF, FDB,
  IS-IS, and MPLS-TE, with graph reconciliation across protocols.
- Prometheus metrics at `/metrics` (~50 series) for device inventory, topology
  edges, discovery outcomes, federation health, and operational signals.
- Structured JSON log lines for topology change events and walker outcomes.
- Versioned on-disk snapshot so `/metrics` serves the previous graph on restart.
- Optional OTLP push for topology metrics, change-event logs, and (opt-in)
  discovery-cycle traces.
- Optional RFC 8345 / RFC 8346 YANG topology output at `/topology/yang`.
- Multi-instance federation: standalone, hub, spoke, and uncoordinated roles
  with mTLS spoke→hub push and opt-in hub high availability (Kubernetes lease
  leader election).
- Credential profiles (SNMP v2c/v3) with env-var indirection, trial rate
  limiting, and profile invalidation.
- Kubernetes deployment paths: Helm chart and Kustomize overlays (standalone,
  hub, spoke).
- Multi-arch container images (`linux/amd64`, `linux/arm64`) on GHCR with
  cosign keyless signing, SLSA provenance attestations, and offline release
  tarballs.
- Containerlab-based vendor labs (Cisco, Arista, Juniper, Nokia) and SR Linux
  e2e coverage.

### Security

- Supply-chain hardened CI: pinned actions, cosign-signed release artefacts,
  SPDX SBOM, and govulncheck in the release pipeline.
