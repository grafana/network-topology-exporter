# Changelog

All notable changes are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project adheres to
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `sys_name` label/attribute on `network_topology_device_info` and OTLP device
  metrics — the case-preserving sysName, alongside the existing lowercased
  `device_id`. Joins against Alloy/`prometheus.exporter.snmp`'s own
  case-sensitive `sysName` label. (issue #227)
- `src_if_index`/`dst_if_index` labels/attributes on `network_topology_edge_info`
  and OTLP edge metrics — the IF-MIB ifIndex for each endpoint, when the
  discovery protocol resolves one. Joins against snmp_exporter's
  ifIndex-keyed `if_mib` rows. (issue #227)

### Changed

- **Breaking:** OTLP device attribute renamed `device` → `device_id`, to match
  the Prometheus label name. (issue #227)
- Federation uncoordinated mode's `network_topology_boundary_observation_info`
  now case-folds `peer_a`/`peer_b`/`reporting_device` at emission — fixes
  mixed-case fleets being unmatchable by a hand-written PromQL/Mimir recording
  rule (PromQL has no `lower()`). The underlying graph snapshot and federation
  wire payload are unaffected; hub mode's own matching and collision-detection
  diagnostic are unchanged. (issue #227)

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
  a negative `src_if_index`/`dst_if_index` — the two new int fields had no
  validation at all.

### Internal

- `internal/discovery/isis`: replaced two parallel maps
  (`circIfNames`/`circIfIndexes`) with a single `map[string]circuitIf`, since
  they were always populated and looked up together.
rationale and migration notes.

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
