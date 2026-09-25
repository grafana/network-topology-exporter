# OTLP Payload Schema

Version: 1.1 (pending release — additive only; last stable at 1.0, v1.0.0). `device` is deprecated (see below) but still emitted, so this is not a breaking version bump.
Semantic conventions: OpenTelemetry semconv v1.26.0 (the version the exporter is built against). Note: the exporter does not set a resource Schema URL on the wire, so emitted payloads carry no `schema_url` field.

## Resource attributes

| Attribute | Type | Description |
|---|---|---|
| `service.name` | string | Always `"network-topology-exporter"` |
| `service.version` | string | Binary version (semver) |
| `service.instance.id` | string | Hostname of the exporter instance |

## Edge metrics — `network_topology_edge_info`

One gauge data point per edge, value `1.0`. Attributes:

| Attribute | Type | Description |
|---|---|---|
| `src_device` | string | Source device sysName |
| `src_port` | string | Source interface name (empty if unknown) |
| `src_if_index` | string | Source IF-MIB ifIndex (stringified). Omitted, not `"0"`, when unresolved — new in v1.1, see docs/proposals/snmp-exporter-label-alignment.md §4. |
| `dst_device` | string | Destination device sysName or IP |
| `dst_port` | string | Destination interface name (empty if unknown) |
| `dst_if_index` | string | Destination IF-MIB ifIndex (stringified). Omitted when unresolved — same as `src_if_index`. |
| `proto` | string | Discovery protocol: `lldp`, `cdp`, `bgp`, `ospf`, `isis`, `mpls_te`, `fdb`, `configured` |
| `link_kind` | string | Transport type: `ethernet`, `ip`, `mpls-te`, `logical`, etc. |
| `direction` | string | `unidirectional` or `bidirectional` |
| `confidence` | string | `low`, `medium`, or `high` |
| `adjacency` | string | `direct`, `indirect`, or `unknown` |
| `precedence_rank` | string | Integer rank (stringified) used by the hub to break ties between equivalent edges; lower wins |
| `network.topology.<key>` | string | Per-edge metadata pass-through. Each entry of `Edge.Metadata` becomes one attribute, prefixed with `network.topology.`. Example: `network.topology.bgp.remote_as`, `network.topology.mpls_te.admin_status`. Keys are operator/discovery-module supplied; values are size-capped and UTF-8 sanitised. |

## Device metrics — `network_topology_device_info`

One gauge data point per device, value `1.0`. Attributes:

| Attribute | Type | Description |
|---|---|---|
| `device_id` | string | Device sysName (lowercased) or management IP. New in v1.1 (issue #227) — matches the Prometheus `device_id` label name; no rationale was on record for `device` and the Prometheus label using different key names. Same value as the deprecated `device` attribute below. |
| `device` | string | **Deprecated (issue #227, v1.1).** Same value as `device_id`; kept only for one release's overlap window so consumers reading `device` keep working while they migrate to `device_id`. Emitted alongside `device_id`, not replaced by it, in this release. Scheduled for removal in the next major version — migrate to `device_id` now. |
| `sys_name` | string | Device sysName, case-preserving (omitted when empty). New in v1.1 — the join key against Alloy/`prometheus.exporter.snmp`'s own `sysName` label, which is case-sensitive and cannot be matched against the lowercased `device_id`. See docs/proposals/snmp-exporter-label-alignment.md. |
| `vendor` | string | Vendor name (omitted when empty) |
| `model` | string | Hardware model (omitted when empty) |
| `os_version` | string | OS version string (omitted when empty) |
| `site` | string | Site / location label (omitted when empty) |

## Versioning policy

Attribute names are stable within a major version.
New optional attributes may be added in minor releases.
Attribute removals or renames require a major version bump and CHANGELOG notice.
