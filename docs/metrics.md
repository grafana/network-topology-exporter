# Metrics Reference

All metrics use the `network_` prefix. No metric uses a raw IP address or free-form text as a label value. Abbreviations (OOS, MIB, PDU, …) are defined in the [glossary](glossary.md).

## Topology inventory

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `network_topology_device_info` | gauge (always 1) | `device_id`, `vendor`, `model`, `os_version`, `site`, `sys_name` | One series per discovered device. Absent from `/metrics` when device is no longer in graph. **Breaking change (issue #227):** added `sys_name` — the case-preserving sysName, alongside the existing lowercased `device_id`. Per `docs/operator/stability.md`'s stability policy, adding a label widens the series fingerprint and is itself a breaking change to this metric's frozen label set, regardless of the new label being optional/omittable. Added specifically as a join key against Alloy/`prometheus.exporter.snmp`'s own `sysName` label, which snmp_exporter passes through in the device's original case; PromQL has no `lower()`, so `device_id` alone cannot match it for any device with a mixed-case hostname. Empty when sysName didn't resolve — this also happens for an up-to-date device mid-rolling-upgrade if the value came from a not-yet-upgraded federation spoke, not only for a genuinely IP-identified device; check whether `device_id` itself looks like an IP to tell the two apart. See docs/proposals/snmp-exporter-label-alignment.md. |
| `network_topology_device_uptime_seconds` | gauge | `device_id` | From SNMP sysUpTime. |
| `network_topology_edge_info` | gauge (always 1) | `src_device`, `src_port`, `src_if_index`, `dst_device`, `dst_port`, `dst_if_index`, `discovery_proto`, `link_kind`, `direction` | One series per edge. Absent from `/metrics` when edge is removed. **Breaking change (issue #227):** added `src_if_index`/`dst_if_index` — the IF-MIB ifIndex for each endpoint, when the discovery protocol resolves one. Per `docs/operator/stability.md`'s stability policy, adding a label widens the series fingerprint and is itself a breaking change to this metric's frozen label set, regardless of the new labels being empty/omittable in the common case. Empty when unresolved (LLDP/CDP/FDB/IS-IS resolve only the local/reporting side today; BGP/OSPF/MPLS-TE never resolve one — those are routing-protocol adjacencies with no interface-scoped MIB row). This is the join key against snmp_exporter's `ifIndex`-keyed `if_mib` rows. See docs/proposals/snmp-exporter-label-alignment.md §4. |
| `network_topology_graph_devices_total` | gauge | (none) | Current number of devices in the active reconciled topology graph. |
| `network_topology_graph_edges_total` | gauge | (none) | Current number of reconciled edges in the active topology graph. |
| `network_topology_change_total` | counter | `change_kind` (added\|removed\|updated), `discovery_proto` | Resets on restart. Use `increase()` not `rate()` for alerting on sparse events. |
| `network_topology_out_of_scope_neighbours_total` | gauge | (none) | Count of distinct neighbours seen this cycle whose IPs fall outside the CIDR allow-list. Detail (device, port, hint) is in log lines. |

## Conflict detection

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `network_topology_conflict_total` | counter | `conflict_type` (`neighbour_disagreement`) | Source disagreements detected during reconciliation. Resets on restart. Only `neighbour_disagreement` is currently emitted (two protocols name different neighbours for the same local port). A sustained rate may indicate stale LLDP/CDP cache or protocol misconfiguration on the local device. |

## Graph freshness

| Metric | Type | Notes |
|--------|------|-------|
| `network_topology_graph_stale` | gauge (0/1) | 1 while serving snapshot on startup; 0 after first live cycle. Alert: `network_topology_graph_stale == 1 for 15m` |
| `network_topology_snapshot_last_written_timestamp_seconds` | gauge | Alert: absent or stopped advancing after two cycle intervals. |
| `network_topology_snapshot_loaded_devices_total` | gauge | Sanity check at startup; compare to expected fleet size. |
| `network_topology_snapshot_drops_total` | counter | Snapshot writes dropped because the persistence pipeline could not absorb them. `reason` ∈ {`queue_full`, `write_in_flight`}. Issue #42. Alert: `rate(...) > 0 for 5m` — persistent drops mean on-disk state is going stale; restart will cold-start from a worse position the longer the stall lasts. |
| `network_topology_snapshot_queue_depth` | gauge | Current number of snapshots queued for writing. A persistently non-zero value means snapshot writes are slower than the discovery cycle produces them — the precursor to `snapshot_drops_total`. |

## Discovery cycle health

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `network_topology_discovery_devices_total` | gauge | `status` (success\|failed), `reason` | Resets each cycle to reflect the most recent outcome. `reason` partitions `status="failed"` rows into `unreachable`, `auth_failed`, `timeout`, `snmp_error`, `mib_unsupported`, `dns_failed`, `outside_allow_list`, `no_credentials`, `budget_expired`, `panic`. `status="success"` rows carry `reason="n/a"`. **Breaking change (issue #20):** the prior unpartitioned `status` label has been widened. Dashboards counting "all failures" must use `sum by (status) (network_topology_discovery_devices_total)`; alerts on a specific cause should select the `reason` value directly. The pre-#20 `status="timeout"` row has been promoted to `status="failed", reason="timeout"`. |
| `network_topology_discovery_cycle_duration_seconds` | histogram | (none) | Buckets: 0.5s–256s (exponential, factor 2). Alert when p99 approaches `discovery.interval`. |
| `network_topology_discovery_module_duration_seconds` | histogram | `module` | Per-module time within a cycle. Valid values: snmp, lldp, cdp, bgp, ospf, fdb, isis, mpls_te. |
| `network_topology_snmp_walks_total` | counter | `status` (ok\|timeout\|error), `reason` | Aggregate across all devices. `reason` partitions `status="error"` rows into `unreachable`, `auth_failed`, `snmp_error`, `mib_unsupported`, `decode_error`, `module_error`. `status="ok"` and `status="timeout"` rows carry `reason="n/a"` (the status itself is the reason). **Breaking change (issue #20):** label set widened from `{status}` to `{status, reason}`. Migrate `network_topology_snmp_walks_total{status="error"}` queries to `sum by (status)(network_topology_snmp_walks_total)` or pick a specific reason. |
| `network_topology_discovery_decode_issues_total` | counter | `module`, `oid`, `reason` | SNMP decode anomalies (for example, invalid integer type in a walked column). Non-zero sustained rate indicates schema/vendor drift or corrupt agent responses. As of #99 the `lldp`, `cdp`, `ospf`, and `fdb` modules also report per-row decode rejections here (previously a silent `continue`/`slog.Debug`): `lldp` (`oid="1.0.8802.1.1.2.1.4.1"`) with `reason ∈ {chassis_subtype_invalid, port_subtype_invalid, chassis_mac_bad_length, port_mac_bad_length, chassis_addr_malformed}`; `cdp` (`oid="1.3.6.1.4.1.9.9.23.1.2.1"`) with `reason ∈ {index_unparseable, empty_device_id}`; `ospf` (`oid="1.3.6.1.2.1.14.10"`) with `reason ∈ {oid_suffix_malformed, nbr_ip_undecodable}`; `fdb` (`oid="1.3.6.1.2.1.17.1.4"`) with `reason ∈ {bridge_port_index_invalid, ifindex_unmapped}`. |
| `network_topology_discovery_quarantined_rows_total` | counter | `module`, `oid`, `reason` | Required/optional table rows dropped from processing due to decode anomalies. A rising rate with stable hard-fail counts indicates resilient degradation. |
| `network_topology_discovery_degraded_total` | counter | `module`, `reason` | Module runs that completed in degraded mode (for example, missing optional metadata table). As of #100 the `fdb` module contributes `reason="qbridge_walk_failed"` (the Q-BRIDGE walk errored AND the B-MIB walk produced no usable entries — guarded so it stays quiet when the B-MIB already had entries and for legitimate B-MIB-only devices with a clean empty Q-BRIDGE walk) and `reason="vlan_walk_failed"` (a per-VLAN community walk failed, labelled by reason only, not by VLAN id). As of #102 the `isis` module contributes `reason="unsupported_ip_version"` (IPv6 IS-IS adjacency rows were skipped; IPv4 edges unaffected). These three are emitted via a direct module sink so they fire even when the walk produced zero edges. |
| `network_topology_discovery_hard_fail_total` | counter | `module`, `reason` | Hard failures where required discovery policy was breached (for example, `required_table_no_valid_rows`, `required_table_invalid_ratio_exceeded`). |
| `network_topology_credential_trials_total` | counter | `status` (ok\|failed) | Auth trial attempts against polled devices. A sustained `failed` rate on a fresh deployment may indicate auth lockout. |
| `network_topology_walker_outcome_total` | counter | `walker` (lldp\|cdp\|ospf\|fdb), `outcome` (edges\|mib_unimplemented\|no_neighbours\|walker_drift\|error) | Per-walker outcome accounting for the non-BGP discovery walkers — the generic sibling of `network_topology_bgp_walker_outcome_total`, added in #98. Additive and non-breaking: the BGP counter was **not** renamed and the two coexist. `outcome=edges` means the walker produced ≥1 edge; `mib_unimplemented` means the base table returned zero PDUs (the device does not implement the table — expected on non-applicable devices, **do not alert**); `no_neighbours` means PDUs arrived AND at least one row decoded cleanly but zero usable edges resulted (protocol up, nothing to report); `walker_drift` means PDUs arrived but EVERY row was rejected by the decoder (the MIB is implemented but our decoder doesn't match this firmware — page-level signal); `error` means the walk itself errored (emitted alongside `network_topology_discovery_hard_fail_total`). Mirrors the BGP four-bucket semantics. |
| `network_topology_bgp_walker_outcome_total` | counter | `walker` (vendor_cisco\|vendor_arista\|vendor_juniper\|vendor_nokia\|rfc4273), `outcome` (edges\|no_peers\|mib_unimplemented\|walker_drift\|error\|malformed_index) | Per-walker outcome accounting for the BGP module's fallback chain. `outcome=edges` means the walker produced rows; `mib_unimplemented` means `BulkWalk` returned zero PDUs — the device does not implement the table (expected for non-BGP devices, do not alert); `no_peers` means PDUs arrived AND at least one row decoded cleanly but no peer reached `bgpStateEstablished` (BGP is configured but down — operationally distinct from `mib_unimplemented` and the correct signal for "BGP broken" alerts); `walker_drift` means PDUs arrived but EVERY row was rejected by the vendor decoder (the device DOES implement the MIB but our walker doesn't match — page-level signal that the walker is broken on this vendor's MIB); `error` means the SNMP walk failed and the next walker was tried; `malformed_index` means a row was dropped because its index couldn't be decoded under the spec's per-vendor decoder. A non-zero `malformed_index` rate on a known vendor is a signal that the column numbers or index format in `internal/discovery/bgp/bgp_vendor.go` may be wrong for that vendor's OS version — the documented failure mode this counter exists to surface. **Breaking change (issue #27):** the all-rows-malformed sub-case of the prior `no_peers` semantics was hoisted to its own `walker_drift` outcome. Operator alerts on `outcome="no_peers"` that expected to fire on "every row was decoder-rejected" must migrate to `outcome="walker_drift"`. **Earlier breaking change (issue #31, v1.3.0):** the prior `walker="v2_draft"` label has been removed (the IETF draft OID at 1.3.6.1.3.5 is not implemented by any vendor tested). The new `walker="vendor_arista"` label covers Arista cEOS via its enterprise BGP4V2 MIB at 1.3.6.1.4.1.30065.4. Operators alerting on `walker="v2_draft"` should migrate to `walker="vendor_arista"` or `walker="rfc4273"` depending on the device fleet. **Earlier breaking change (issue #15):** the prior `empty` outcome was split into `no_peers` and `mib_unimplemented`. |
| `network_topology_system_walk_anomaly_total` | counter | `reason` (empty_sysname\|unknown_vendor) | Content anomalies in an otherwise-successful per-device SNMP system GET, added in #101. Closed two-value set, low cardinality — there is **no** device/IP/OID label. `empty_sysname` means the system GET succeeded but `sysName` was empty/garbage, so the device ID falls back to the management IP. `unknown_vendor` means the `sysObjectID` did not resolve to a known vendor (Vendor stays `"unknown"`), so the vendor-specific BGP4-V2 walker is skipped — BGP still falls through to the observable RFC 4273 path, so this signal flags *which* devices were never tried against a vendor table rather than indicating a BGP failure. |
| `network_topology_module_last_status` | gauge | `module` | Status of each discovery module in the most recent cycle: `0` = ok, `1` = degraded (partial results), `2` = failed (no results). The point-in-time companion to the `degraded`/`hard_fail` counters — alert on `network_topology_module_last_status >= 1` for a sustained period. |
| `network_topology_cycle_budget_skips_total` | counter | (none) | Targets skipped in a discovery cycle because the cycle budget deadline expired before their goroutine could start. Non-zero means the fleet does not fit in `discovery.interval` — see `docs/operator/scale.md`. |
| `network_topology_fdb_suppressed_macs_total` | counter | (none) | FDB MAC peers dropped because no LLDP chassis-MAC correlation was found. These passed the single-learned-MAC filter but could not be resolved to a device identity — enable `modules.arp.enabled` or expect host MACs on access ports. |
| `network_topology_snmp_rate_limit_wait_seconds` | histogram | (none) | Time spent blocked on the per-target SNMP rate limiter before issuing a walk (issue #72). Non-zero observations mean `per_target_pdu_rate_per_second` is biting; sustained waits stretch the cycle. |

## SNMP session pool

All four series are zero unless `discovery.snmp.session_pool.enabled` (issue #83).

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `network_topology_snmp_session_pool_size` | gauge | (none) | Current number of pooled SNMP sessions — one per (target, credential profile). |
| `network_topology_snmp_session_pool_hits_total` | counter | (none) | Checkouts that reused a live pooled session. |
| `network_topology_snmp_session_pool_misses_total` | counter | (none) | Checkouts that opened a fresh session (cold key or in-use fallback). |
| `network_topology_snmp_session_pool_evictions_total` | counter | `reason` (idle\|credential_rotation\|connection_error) | Sessions closed and removed from the pool. `connection_error` evictions mean a transport-level walk failure closed the session so the next acquire dials fresh. |

## Admin endpoints

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `network_topology_admin_rediscovery_total` | counter | `outcome` (success\|timeout\|auth_failure\|out_of_scope\|error) | Per-target outcomes of forced out-of-cycle re-discovery via `POST /admin/rediscover` (issue #73). The endpoint only operates when `listen.web_config_file` actually authenticates the caller. |

## Cardinality budget

Dominant series: `network_topology_edge_info`. Each unique `(src_device, src_port, dst_device, dst_port, discovery_proto, link_kind, direction)` tuple is one series.

For a 500-device network with an average of 4 links per device and 2 protocols reporting each link:
- Edges: ~1000 physical links × 2 protocols = ~2000 edge series
- Devices: ~500 device_info series + ~500 uptime series = ~1000
- Total topology series: ~3000

This is well within Prometheus defaults. If LLDP and CDP both report the same link, the reconciler collapses them to the highest-precedence source before metric emission — duplicate edges do not multiply series.

## OTLP output

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `network_topology_otlp_push_total` | counter | `status` (ok\|error\|dropped), `reason` | Incremented after each OTLP push attempt (both `/v1/metrics` and `/v1/logs` share this counter). Only present when `output.otlp.enabled: true`. `reason` partitions `status="error"` into `timeout`, `tls_error`, `http_4xx`, `http_5xx`, `payload_rejected`, `network` (the catch-all for transport faults — DNS, connection refused, EOF). `status="ok"` and `status="dropped"` rows carry `reason="n/a"`. `dropped` means the per-process push concurrency limit (`maxOTLPPushConcurrency = 4`) was saturated and the push was discarded without contacting the receiver — see operator troubleshooting "OTLP push saturation". **Breaking change (issue #20):** label set widened from `{status}` to `{status, reason}`. Migrate `{status="error"}` queries to `sum by (status)(...)` or branch on the new `reason` for triage (`reason="tls_error"` → cert problem; `reason="http_5xx"` → receiver crashing; `reason="network"` → collector down). Mirrors the partition that landed for `network_topology_graph_updates_rejected_total` in D29. |

## Process self-observability

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `network_topology_metrics_render_duration_seconds` | histogram | (none) | Wall time to render one `/metrics` scrape response. Alert at p99 against the scraper's `scrape_timeout`. See `docs/operator/scale.md` for guidance. |
| `network_topology_metrics_payload_bytes` | histogram | (none) | Response body size of one `/metrics` scrape, in bytes. Tracks growth as the topology scales; useful for capacity planning. See `docs/operator/scale.md`. |
| `network_topology_last_scrape_duration_seconds` | gauge | (none) | Time taken to render all topology metrics at the most recent scrape — the point-in-time companion to the render-duration histogram. |
| `network_topology_last_scrape_samples_total` | gauge | (none) | Number of metric samples emitted at the most recent scrape. Watch this against the cardinality budget below as the topology grows. |
| `network_topology_goroutines` | gauge | (none) | Current number of live goroutines in the process. A monotonic climb across cycles indicates a leak. |
| `network_topology_panics_total` | counter | `site` | Panics recovered in background goroutines, by recovery site. Any non-zero value indicates a bug — alert on increase and file an issue. |

## Federation

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `network_topology_boundary_observation_info` | gauge (always 1) | `peer_a`, `peer_b`, `reporting_device`, `src_port`, `proto` | Uncoordinated mode only. One series per out-of-scope neighbour, reset each cycle. `peer_a` is always the alphabetically-smaller endpoint. Recording rule: `count by(peer_a, peer_b, proto)(...) == 2` fires when both sides have reported, producing a confirmed cross-boundary edge. All three name labels are case-folded to lowercase at emission (PromQL has no `lower()`, so a naive recording rule needs pre-folded values to pair a mixed-case fleet correctly) — the underlying discovered neighbour hint stays in its original case internally and in the federation wire payload; only what's emitted here is folded. See docs/proposals/knowledge-graph-boundary-stitching.md §5. |
| `network_topology_federation_spoke_up` | gauge (0/1) | `spoke_id` | Hub mode only. 1 while a spoke has pushed within `federation.spoke_timeout`; drops to 0 after eviction. Alert: `network_topology_federation_spoke_up == 0` |
| `network_topology_federation_spoke_last_push_timestamp_seconds` | gauge | `spoke_id` | Hub mode only. Unix timestamp in seconds of the most recent push from each spoke. Alert: `time() - network_topology_federation_spoke_last_push_timestamp_seconds > federation.spoke_timeout` |
| `network_topology_federation_spoke_push_failures_total` | counter | (none) | Spoke mode only. Incremented each time a push attempt exhausts all retries. A non-zero rate means the hub is not receiving topology data from this spoke. |
| `network_topology_federation_spoke_push_last_success_unix` | gauge | (none) | Spoke mode only. Unix timestamp (seconds) of this spoke's most recent successful push to the hub — the spoke-side counterpart of the hub's `..._last_push_timestamp_seconds`. Alert: `time() - value` exceeding a few `discovery.interval`s. |
| `network_topology_federation_spoke_push_drops_total` | counter | `reason` (superseded\|shutdown) | Spoke mode only. Payloads dropped by the async pusher's latest-only mailbox. `superseded` is normal under hub slowness (a newer graph replaced a queued one — only the freshest snapshot matters); `shutdown` counts payloads abandoned at process exit. |
| `network_topology_federation_spoke_push_queue_depth` | gauge | (none) | Spoke mode only. Depth of the spoke async-push mailbox (0 or 1 by design — the mailbox holds only the latest unsent graph). |
| `network_topology_hub_oos_unmatched_total` | gauge | (none) | Hub mode only. Count of out-of-scope hints received this cycle that could not be matched to any known device name via `normalizeDeviceName`. A value > 0 indicates vendor chassis-id encoding differences; add static `known_inter_domain_links` entries as the reliable workaround. |
| `network_topology_graph_updates_rejected_total` | counter | `reason` (size_budget_exceeded\|invalid_label_key\|invalid_label_value\|structural_invalid\|stale_generation) | Combined-graph updates rejected at publish time, partitioned by reason. `size_budget_exceeded` means the graph exceeded `MaxGraphEdges` or `MaxGraphDevices` (either federation hub or local standalone). `invalid_label_key` / `invalid_label_value` / `structural_invalid` are hub-mode only and mean a spoke-supplied payload failed validation — label safety (`invalid_label_*`) or structural invariants such as empty identifiers, duplicate device IDs, self-edges, or oversize fields (`structural_invalid`). `stale_generation` means the spoke pushed an older graph generation than the hub already holds. See `docs/operator/federation.md` for the on-wire reject contract. Alert: `rate(network_topology_graph_updates_rejected_total[5m]) > 0`, with per-reason breakdown for triage. |

## Recommended alerts

```promql
# Graph stopped refreshing
network_topology_graph_stale == 1

# Discovery cycle slower than configured interval (using 60s as example)
histogram_quantile(0.99, rate(network_topology_discovery_cycle_duration_seconds_bucket[10m])) > 60

# Snapshot stopped being written (dead man's switch)
time() - network_topology_snapshot_last_written_timestamp_seconds > 300

# Sustained credential failures (possible lockout)
increase(network_topology_credential_trials_total{status="failed"}[10m]) > 20

# SNMP decode anomalies (schema drift or bad agent data)
increase(network_topology_discovery_decode_issues_total[10m]) > 0

# Discovery degraded mode entered
increase(network_topology_discovery_degraded_total[10m]) > 0

# Hard failures in required discovery stages
increase(network_topology_discovery_hard_fail_total[10m]) > 0

# Federation spoke not pushing (hub mode)
network_topology_federation_spoke_up == 0
```
