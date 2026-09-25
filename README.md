# network-topology-exporter

A standalone, AGPL-3.0-licensed Go exporter that discovers network topology over SNMP — walking LLDP, CDP, BGP, OSPF, FDB, IS-IS, and MPLS-TE tables — and emits five signals:

- **Prometheus metrics** for device inventory and topology edges, scraped via `/metrics`.
- **Structured log lines** (JSON to stderr) for topology change events and operational state.
- **A versioned JSON snapshot** on disk so `/metrics` serves the previous graph immediately on restart.
- **OTLP push** (optional) — topology edges and devices as OTLP metrics, change events as OTLP log records, and (opt-in) discovery-cycle traces, delivered directly to any OTLP-capable receiver.
- **A YANG topology document** (optional) — `GET /topology/yang` renders the current reconciled graph as RFC 8345 / RFC 8346 YANG-JSON (`ietf-network` / L3 topology) for interchange with NETCONF/RESTCONF-aware tooling. Enable with `output.yang.enabled`.

By default there is no bespoke control-plane API and the exporter does not push to external systems. Anything that needs the topology graph queries Prometheus / Mimir. Topology change events are log lines — ship them to Loki, Elasticsearch, or any log aggregator using the collector agent already in your stack (Promtail, Alloy, Fluentd). Enable `output.otlp` to additionally push topology state directly to an OTLP endpoint.

Protocol names, MIB names, and other abbreviations used throughout the docs (OOS, USM, PDU, …) are defined in the [glossary](docs/glossary.md).

## Why this exists

The Prometheus / Grafana stack already covers storage, query, alerting, and visualisation. What it doesn't cover is a network-discovery surface that produces topology data in formats that stack already understands. Think `snmp_exporter` for the topology problem: a Grafana panel renders the network graph by querying Mimir directly, with no bespoke graph endpoint sitting in front of the data.

For a side-by-side feature comparison with LibreNMS, SuzieQ, Nautobot, OpenNMS, and SolarWinds NPM, see [`docs/comparisons/matrix.md`](docs/comparisons/matrix.md).

## Status

**v1.0.0 — initial Grafana release.** SNMP / LLDP / CDP / BGP / OSPF / FDB /
IS-IS / MPLS-TE discovery, graph reconciliation, credential management,
snapshot persistence, multi-instance federation (including opt-in hub high
availability), optional OTLP push, and optional RFC 8345 YANG output are
implemented and covered by unit, integration, and end-to-end tests.

The config schema, metric names, CLI flags, on-disk snapshot format, and
federation API are semver-stable from v1.0.0 — see
[`docs/operator/stability.md`](docs/operator/stability.md). Future work is
tracked in [ROADMAP.md](ROADMAP.md).

## Quickstart

New here? [`GETTING_STARTED.md`](GETTING_STARTED.md) is a copy-pasteable, step-by-step walkthrough for both standalone and hub-spoke modes.

```bash
go build -o bin/topology-exporter ./cmd/topology-exporter
./bin/topology-exporter --config.file=config/example.yaml
curl http://localhost:9100/metrics   # topology metrics
curl http://localhost:9100/readyz    # 503 during startup, 200 after first cycle
curl http://localhost:9100/healthz   # last cycle timestamp and error count
```

Or via Docker:

```bash
docker build -t network-topology-exporter:dev .
docker run --rm -p 9100:9100 \
  -v $PWD/config/example.yaml:/etc/topology-exporter/config.yaml:ro \
  network-topology-exporter:dev
```

## Installation

Released images are multi-arch (`linux/amd64`, `linux/arm64`), cosign-keyless
signed, and carry SLSA build-provenance attestations. Two install paths cover
registry-restricted and air-gapped environments:

1. **GHCR (default):**

   ```bash
   docker pull ghcr.io/grafana/network-topology-exporter:latest
   ```

2. **Offline tarball (air-gapped)** — each GitHub Release attaches a
   cosign-signed `network-topology-exporter-<version>-offline.tar.gz` bundling
   the static binaries (with sigs/certs), the example config, the Helm chart,
   and the Kustomize overlays for `curl`-and-untar deployment.

### Kubernetes

| Method | Command |
|---|---|
| Helm | `helm install topology-exporter deploy/helm/topology-exporter` |
| Kustomize | `kubectl apply -k deploy/kustomize/overlays/standalone` |

The Kustomize overlays `standalone`, `hub`, and `spoke` map to the corresponding
`federation.role`; they mirror the Helm chart's rendered output for GitOps
(Argo CD / Flux) or Helm-free clusters. See
[`deploy/kustomize/README.md`](deploy/kustomize/README.md).

`federation.role: hub` deploys a **different image**,
`ghcr.io/grafana/network-topology-hub` (built from `cmd/topology-hub`,
`Dockerfile.hub`) — both the `hub` Kustomize overlay and the Helm chart
(`image.hubRepository`) select it automatically; every other role keeps
`ghcr.io/grafana/network-topology-exporter`.

## Emitted signals

### Prometheus metrics

The table below is a representative subset — the full reference (all ~50 series, label values, cardinality budget, and recommended alerts) is [`docs/metrics.md`](docs/metrics.md).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `network_topology_device_info` | gauge (always 1) | `device_id`, `vendor`, `model`, `os_version`, `site` | One series per discovered device. |
| `network_topology_device_uptime_seconds` | gauge | `device_id` | Per-device uptime from sysUpTime. |
| `network_topology_edge_info` | gauge (always 1) | `src_device`, `src_port`, `dst_device`, `dst_port`, `discovery_proto`, `link_kind`, `direction` | One series per discovered edge. |
| `network_topology_change_total` | counter | `change_kind` (added\|removed\|updated), `discovery_proto` | Topology mutations between cycles. Use `increase()` not `rate()`. |
| `network_topology_out_of_scope_neighbours_total` | gauge | (none) | Count of LLDP/CDP neighbours outside the CIDR allow-list — "out of scope" (OOS) — this cycle. Detail in log lines. |
| `network_topology_graph_stale` | gauge (0/1) | (none) | 1 while serving the startup snapshot; 0 after first live cycle. |
| `network_topology_snapshot_last_written_timestamp_seconds` | gauge | (none) | Wall-clock time of most recent snapshot write. |
| `network_topology_snapshot_loaded_devices_total` | gauge | (none) | Device count loaded from snapshot at startup. |
| `network_topology_discovery_devices_total` | gauge | `status` (success\|failed), `reason` | Per-cycle device-discovery outcome. `status=success` rows carry `reason=n/a`; failures carry a `reason` sub-label (e.g. `reason=timeout`). |
| `network_topology_discovery_cycle_duration_seconds` | histogram | (none) | End-to-end discovery cycle wall time. |
| `network_topology_discovery_module_duration_seconds` | histogram | `module` | Per-module wall time within a cycle. |
| `network_topology_snmp_walks_total` | counter | `status` (ok\|timeout\|error) | SNMP walk outcomes, aggregated across all devices. |
| `network_topology_credential_trials_total` | counter | `status` (ok\|failed) | Credential trial attempts under the LD-12 rate limiter. |
| `network_topology_conflict_total` | counter | `conflict_type` | Source disagreements detected during reconciliation. The only `conflict_type` currently emitted is `neighbour_disagreement` (two protocols name different neighbours for the same local port). |
| `network_topology_federation_spoke_up` | gauge (0/1) | `spoke_id` | Hub mode only. 1 while spoke has pushed within `spoke_timeout`; 0 after eviction. |
| `network_topology_federation_spoke_last_push_timestamp_seconds` | gauge | `spoke_id` | Hub mode only. Wall-clock time of most recent push from each spoke. |
| `network_topology_federation_spoke_push_failures_total` | counter | (none) | Spoke mode only. Incremented each time a push exhausts all retries. |
| `network_topology_hub_oos_unmatched_total` | gauge | (none) | Hub mode only. Out-of-scope (OOS) neighbour hints that could not be matched to any known device name. |
| `network_topology_otlp_push_total` | counter | `status` (ok\|error\|dropped) | Incremented after each OTLP push attempt. Only present when `output.otlp.enabled: true`. `dropped` means the per-process push concurrency limit (`maxOTLPPushConcurrency = 4`) was already saturated and this attempt was discarded without contacting the receiver — see troubleshooting §11/§OTLP push saturation. |

Full label reference, cardinality budget, and recommended alerts: [`docs/metrics.md`](docs/metrics.md).

No metric uses a raw IP address or free-form text as a label value.

### Log lines

All operational output goes to structured JSON on stderr. Topology change events include before/after edge records:

```json
{"time":"...","level":"WARN","msg":"topology change","change_kind":"removed","before_src_device":"core-sw-01","before_src_port":"Gi0/1","before_dst_device":"access-sw-03","before_dst_port":"Gi0/24","before_proto":"lldp","before_direction":"bidirectional"}
```

Ship these to Loki with the `{job="topology-exporter"}` label using the collector already in your stack.

### YANG topology document

When `output.yang.enabled: true`, `GET /topology/yang` renders the current reconciled topology as an RFC 8345 `ietf-network` document (with the RFC 8346 L3-unicast network-type marker and an `ntx-topology` augmentation carrying discovery provenance). This is an *interchange* encoding — the structured-document complement to the metric and log signals — for feeding the discovered graph into NETCONF/RESTCONF tooling or other controllers. The emitted document is validated against the canonical YANG modules with `yanglint` in CI.

```yaml
output:
  yang:
    enabled: true
    network_id: network-topology-exporter   # the network-id in the emitted document
```

Mapping contract, module set, and example output: [`docs/operator/yang-topology.md`](docs/operator/yang-topology.md).

## Configuration

See [`config/example.yaml`](config/example.yaml) for the full schema. Four operational commitments:

- **Polling scope is bounded by a CIDR allow-list.** Anything outside is recorded as `network_topology_out_of_scope_neighbours_total` (and a log line) and never polled. Defined in `discovery.scope.cidr_allow_list`.
- **Credentials are named profiles with rate-limited trial.** Per-IP and per-CIDR assignments resolve before the fallback list; the global trial rate is bounded so cold start doesn't lock devices out. Cold-start runbook: [`docs/operator/cold-start-credentials.md`](docs/operator/cold-start-credentials.md).
- **The graph survives restarts.** A versioned JSON snapshot reloads on startup so `/metrics` serves the previous edge set immediately with `network_topology_graph_stale=1` until the first live cycle completes.
- **Unidirectional links expire.** A link reported by only one endpoint for three consecutive cycles is removed and emits a `network_topology_change_total{change_kind="removed"}` increment.

## Discovery modules

Each module is independently enabled and runs within the same cycle. Edges from multiple sources covering the same physical link are reconciled by precedence — the highest-precedence source wins; the rest increment `network_topology_conflict_total` if they disagree.

Precedence is encoded as an integer rank where **lower rank = higher priority** (rank 2 beats rank 5; rank 0 is reserved for hub-injected authoritative overrides). When two sources cover the same physical link, the lowest-rank observation wins and any disagreeing observations increment `network_topology_conflict_total`.

| Module | Protocol / MIB | Rank | YAML key | Notes |
|--------|---------------|------|----------|-------|
| LLDP | LLDP-MIB (IEEE 802.1AB) | 2 | `modules.lldp.enabled` | Standard; preferred source when available. |
| CDP | CISCO-CDP-MIB | 3 | `modules.cdp.enabled` | Cisco-only; lower precedence than LLDP. |
| FDB | BRIDGE-MIB (RFC 4188) | 4 | `modules.fdb.enabled` | Layer-2 only; noisy on large networks. |
| IS-IS | ISIS-MIB (RFC 4444) | 5 | `modules.isis.enabled` | |
| OSPF | OSPF-MIB (RFC 4750) | 6 | `modules.ospf.enabled` | |
| BGP | BGP4-MIB (RFC 4273) | 7 | `modules.bgp.enabled` | |
| MPLS-TE | MPLS-TE-STD-MIB (RFC 3812) | 8 | `modules.mpls_te.enabled` | |

### LLDP (IEEE 802.1AB)

Walks `lldpLocPortTable` and `lldpRemTable`. `SrcPort` is decoded from `lldpLocPortIdSubtype` / `lldpLocPortId`, falling back to `lldpLocPortDesc`. `DstDevice` is the remote system name and `DstPort` is the remote port from the same row. Highest discovery confidence — the protocol is designed for this.

```yaml
modules:
  lldp:
    enabled: true
```

### CDP (CISCO-CDP-MIB)

Walks `cdpCacheTable`. `DstDevice` is `cdpCacheDeviceId`; `DstPort` is `cdpCacheDevicePort`. Cisco-proprietary — non-Cisco devices return empty walks.

```yaml
modules:
  cdp:
    enabled: true
```

### FDB (RFC 4188 / Q-BRIDGE)

Walks `dot1dTpFdbTable` and `dot1dBasePortTable`; on VLAN-aware switches it also walks `dot1qTpFdbTable` and `dot1qVlanCurrentTable`. Emits one edge per MAC seen on exactly one bridge port — multi-MAC ports are uplinks rather than direct peers and are suppressed. `DstDevice` is the raw MAC; graph reconciliation resolves it to a device identity via L3 ARP correlation, so enable `modules.arp.enabled` alongside FDB. `max_vlans` caps the number of per-VLAN community walks on classic Cisco IOS (`0` = unlimited).

```yaml
modules:
  fdb:
    enabled: true
    max_vlans: 0
  arp:
    enabled: true   # required to translate MAC → device on FDB edges
```

### IS-IS (RFC 4444)

Walks `isisISAdjState` and `isisISAdjIPAddrTable`. Only adjacencies in state `up(3)` produce edges. `DstDevice` is the neighbour IP from `isisISAdjIPAddrTable`.

```yaml
modules:
  isis:
    enabled: true
```

### OSPF (RFC 4750)

Walks `ospfNbrTable`. Only neighbours in state `full(8)` or `twoWay(4)` produce edges. `DstDevice` is `ospfNbrIpAddr`; unnumbered P2P links (`0.0.0.0`), link-local, and loopback neighbour addresses are skipped to avoid emitting unusable edges.

```yaml
modules:
  ospf:
    enabled: true
```

### BGP (RFC 4273 BGP4-MIB)

Walks `bgpPeerTable` (RFC 4273, IPv4-only) plus vendor BGP4V2 tables under enterprise OIDs for IPv6 and VRF peers. Only peers in state `established(6)` produce edges; `DstDevice` is the remote peer IP. Confidence is Low and adjacency is `unknown` because BGP peers are not necessarily directly connected. Set `disable_v2_mib: true` to fall back to RFC 4273-only behaviour if a vendor walker regresses.

```yaml
modules:
  bgp:
    enabled: true
    disable_v2_mib: false   # default; true disables the BGP4V2 vendor walkers
```

**Vendor walker coverage.** The v2 walkers ship with mixed validation status:

| Vendor | MIB / OID | Status |
|--------|-----------|--------|
| Cisco | `cbgpPeer2Table` (1.3.6.1.4.1.9.9.187.1.2.5) | Real-device validated against Cisco IOL 17.12.1 (2026-05-16) and cross-confirmed against IOS-XE hardware (2026-05-30) |
| Arista | enterprise BGP4V2 (1.3.6.1.4.1.30065.4.1.1.2) | Real-device validated (cEOS 4.36) |
| Juniper | `jnxBgpM2PeerTable` (1.3.6.1.4.1.2636.5.1.1.2.1.1) | Real-device validated against vJunos-router (JUNOS 25.4R1.12, 2026-06) — see `lab/juniper-jnxbgp/` |
| Nokia | `tBgpPeerNgTable` (1.3.6.1.4.1.6527.3.1.2.14.4.7) | Real-device validated against SR-OS 25.7.R2 (7750 SR, 2026-06, #57). Modern SR-OS populates this next-gen table; the legacy `tBgpPeerTable` (.13.2) is empty, and pre-Ng SR-OS / SR Linux fall back to RFC 4273 |

All four vendor walkers are now real-device validated (the per-vendor capture milestone is complete). `disable_v2_mib: true` remains as the escape hatch if a vendor walker regresses on a newer firmware. The walker exposes failure modes via `network_topology_bgp_walker_outcome_total{outcome=...}` (labels include `walker_drift`, `malformed_index`, `mib_unimplemented`) so a broken vendor walk is alertable rather than silent.

### MPLS-TE (RFC 3812)

Walks `mplsTunnelOperStatus`. `SrcPort` is formatted as `te-tunnel<idx>` where `idx` is the tunnel index from the MIB. `DstDevice` is the egress LSR IP.

```yaml
modules:
  mpls_te:
    enabled: true
```

## OTLP output

The exporter can push topology state directly to any OTLP-compatible receiver (Grafana Alloy, OpenTelemetry Collector, Grafana Cloud OTLP endpoint, etc.). This is additive — Prometheus `/metrics` continues to work unchanged.

```yaml
output:
  otlp:
    enabled: true
    endpoint: "http://alloy:4318"   # must be http:// or https://
    timeout: 10s                    # per-push deadline; default 10s
    heartbeat_cycles: 10            # push full graph every N cycles even if no changes; default 10
```

**What gets pushed:**

| Signal | OTLP path | Content |
|--------|-----------|---------|
| Full graph | `POST /v1/metrics` | `network_topology_edge_info` and `network_topology_device_info` gauge metrics, one data point per edge/device. Sent every cycle when there are changes, and unconditionally every `heartbeat_cycles` cycles. Metric names match the Prometheus `/metrics` series. |
| Change events | `POST /v1/logs` | OTLP log records — severity INFO for added/updated edges, WARN for removed edges. Mirrors the JSON log lines already written to stderr. |
| Discovery-cycle traces (opt-in) | `POST /v1/traces` | OTel spans of the exporter's own discovery cycle — `discovery.cycle` → `target.poll` → `<module>.walk` / `credentials.resolve`, plus `graph.reconcile` and (federation spoke) `spoke.push` continued on the hub as `hub.handlePush`. Disabled by default; enable with `output.otlp.traces.enabled`. See [`docs/operator/tracing.md`](docs/operator/tracing.md). |

**Heartbeat semantics:** If no topology change is detected, the exporter skips the `/v1/metrics` push to avoid redundant data. Every `heartbeat_cycles` cycles (default: every 10th cycle) the full graph is pushed unconditionally so downstream receivers can detect a stale or silently-dropped connection.

**Self-monitoring metric:** `network_topology_otlp_push_total{status="ok|error|dropped"}` — a counter incremented after each OTLP push attempt, scraped via the normal `/metrics` endpoint. `dropped` indicates the push was discarded because the per-process concurrency limit was saturated; alert on `rate(...{status=~"error|dropped"}[5m]) > 0`.

### Receiving OTLP in Grafana Alloy

```alloy
otelcol.receiver.otlp "topology" {
  grpc { endpoint = "0.0.0.0:4317" }
  http { endpoint = "0.0.0.0:4318" }

  output {
    metrics = [otelcol.exporter.prometheus.topology.input]
    logs    = [otelcol.exporter.loki.topology.input]
  }
}

otelcol.exporter.prometheus "topology" {
  forward_to = [prometheus.remote_write.mimir.receiver]
}

otelcol.exporter.loki "topology" {
  forward_to = [loki.write.default.receiver]
}
```

Set `output.otlp.endpoint: "http://<alloy-host>:4318"` in the exporter config to target the HTTP receiver above.

## Federation

A single instance covers one contiguous CIDR range. Links that cross a boundary between ranges are visible to each side independently but neither produces a confirmed bidirectional edge because that requires both endpoint observations in a single reconcile call. Three modes bridge this:

| Mode | How it works |
|---|---|
| `uncoordinated` | Each instance emits `network_topology_boundary_observation_info` per OOS neighbour. A Mimir recording rule `count by(peer_a,peer_b,proto)(...) == 2` fires when both sides report — no inter-instance coordination required. |
| `spoke` | Instances push their reconciled graph to a hub after each cycle. The hub aggregates, re-reconciles across all domains, and emits unified metrics. Requires mutual TLS (mTLS). |
| `hub` | Pure aggregator — no local SNMP discovery. Receives spoke pushes on a separate listener (default `:9101`). Runs as a **separate binary/image**, `cmd/topology-hub` (`ghcr.io/grafana/network-topology-hub`) — only it links `k8s.io/client-go`, needed for the opt-in native-HA leader election below. `standalone`/`uncoordinated`/`spoke` all run `cmd/topology-exporter` as before. |

Spoke pushes are gzip-compressed by default (`federation.spoke.compression: gzip`, or `none` to disable) — topology JSON typically shrinks 10–20×, keeping large graphs well below the hub's 32 MiB body cap.

The hub can optionally run **highly available** on Kubernetes: multiple replicas elect a leader via a `Lease`, spokes follow the leader through a readiness-gated Service, and a fresh leader rebuilds the combined graph from the spokes' next pushes. HA is opt-in (`federation.hub.ha.enabled`); single-hub deployments are unchanged. See the [federation runbook](docs/operator/federation.md#hub-high-availability-native-opt-in).

Federation runbook (mTLS setup, HA, tuning, troubleshooting): [`docs/operator/federation.md`](docs/operator/federation.md).

Operator troubleshooting (no edges, SNMP timeouts, credential failures, slow cycles, snapshot issues): [`docs/operator/troubleshooting.md`](docs/operator/troubleshooting.md).

## Development

```bash
make test               # unit tests (no external deps)
make test-integration   # integration tests (in-process SNMP agents + federation mTLS)
make lint               # golangci-lint
make build              # binary → bin/topology-exporter
make docker             # container image
```

End-to-end tests require Docker and [containerlab](https://containerlab.dev/install/):

```bash
make e2e-image          # build the lightweight Alpine test node image (once)
make test-e2e           # e2e tests: lldp + snmp + binary exporter + federation spoke/hub
make test-e2e-srl       # e2e tests against Nokia SR Linux (x86-64, pulls srlinux:26.3.2)
```

## Test environments

Two reusable harness stacks live under `deploy/`:

| Path | Purpose |
| --- | --- |
| [`deploy/test-harness/`](deploy/test-harness/) | Single-shot test harness — exporter + Alloy shipping a fixed topology to Grafana Cloud, used to validate a build end-to-end. |
| [`deploy/long-running-test/`](deploy/long-running-test/) | Continuously-running validation lab. A mutator container rotates between four containerlab topologies every UTC hour (chain → cross-link → ring → CLOS) to exercise add/remove/swap reconciliation. Ships to Grafana Cloud with `tester_id=long-running-lab`. |

Curated dashboards for both harnesses live in [`dashboards/test-harness/`](dashboards/test-harness/) — see its [README](dashboards/test-harness/README.md) for what each shows and requirements (the topology Node Graph relies on Grafana SQL Expressions).

## Architecture

See [`docs/architecture.md`](docs/architecture.md) for the module layout and discovery cycle. Clean-room development rules: [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Stability and security

Vendor platform support: [`docs/supported-platforms.md`](docs/supported-platforms.md).

Stability promises and GA criteria: [`docs/operator/stability.md`](docs/operator/stability.md).

Upgrade runbook (per-minor-version breaking changes, backup steps, rollout order): [`docs/operator/upgrades.md`](docs/operator/upgrades.md).

SLO guidance (SLI definitions, multi-burn-rate alerts, copy-pasteable PromQL): [`docs/operator/slos.md`](docs/operator/slos.md).

Continuous fuzzing: the project's native Go fuzz harnesses run per-PR (short) and nightly (long) in CI, and are staged for Google OSS-Fuzz enrollment — see [`oss-fuzz/`](oss-fuzz/) for the integration files and submission steps.

To report a vulnerability privately: [`SECURITY.md`](SECURITY.md).

## License

GNU Affero General Public License v3.0 (AGPL-3.0). See [LICENSE](LICENSE).

## Related

- [`network-o11y-dev`](https://github.com/colinedwardwood/network-o11y-dev) — provisioning, dashboards, plugins, and packaging that consume this exporter.
