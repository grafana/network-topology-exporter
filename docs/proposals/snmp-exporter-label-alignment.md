# Proposal: SNMP Exporter Label Alignment

Status: implemented, with one exception. §4 (`src_if_index`/`dst_if_index`) and §5.1–§5.2 (`device_id`/`sys_name`) shipped as designed. §5.3's `management_ip` did not ship — it conflicts with the "no raw IP as a label value" invariant §7 item 5 already flags, and the decision was to drop it rather than carve out an exception; `sys_name` covers the join case management_ip was meant for. The `management_ip` references in §6's before/after example and §9 describe a field that doesn't exist in the shipped code.

## 1. Problem

`network_topology_edge_info` and `network_topology_device_info` identify devices and links but carry nothing that lets a PromQL query join a topology edge to the interface-level metrics Grafana Alloy's `prometheus.exporter.snmp` component (wrapping `prometheus/snmp_exporter`) scrapes from the same devices. An operator who wants "utilization on this LLDP-discovered link" today has no label in common between the two series.

Two sub-problems, addressed together because the second constrains the first:

1. Add an interface-identity label to `network_topology_edge_info` so an edge endpoint can be matched to a specific `ifIndex`-keyed row from snmp_exporter's `if_mib` module.
2. Pick a device-identity join key that is actually usable against what snmp_exporter emits by default — which requires knowing whether snmp_exporter exposes `sysName` as a label at all. That was an open question; §2 resolves it against the real source.

## 2. Resolved: does snmp_exporter expose `sysName` as a joinable label?

Checked against `prometheus/snmp_exporter` tag `v0.29.0`, commit `42f8f2a991516b39ed60ef67eb69263d817ecd52`, cloned read-only to a scratch directory outside any tracked repo (not GPL-licensed source — Apache-2.0 — so CONTRIBUTING.md's LD-09 spec-extraction rule doesn't formally apply, but the same discipline is followed here: no code or YAML table data copied into this repo, only the behavioral finding below, cited to file:line in the upstream tree).

**Finding: yes, but only as a standalone metric, never as a lookup joined onto any other metric.**

The default `snmp.yml`'s `system` module (`snmp.yml:42794`) walks `1.3.6.1.2.1.1` (SNMPv2-MIB system group) and declares `sysName` (`snmp.yml:42817-42820`) — along with `sysDescr`, `sysObjectID`, `sysUpTime`, `sysContact`, `sysLocation`, `sysServices` — as bare scalar metrics with **no `indexes:` and no `lookups:` block**. Confirmed there is no `labelname: sysName` anywhere else in the 45k-line file (`grep -c "labelname: sysName" snmp.yml` → `0`) — nothing wires `sysName` onto another metric's label set the way `ifDescr`/`ifName`/`ifAlias` are wired onto every `if_mib` row (`snmp.yml:22484-22499`, the `lookups:` block on the `ifIndex` and `ifType` metrics).

`collector/collector.go`'s generic string-metric path is what turns a label-less scalar `DisplayString`/`OctetString` into a label at all. `pduToSamples`'s default case (`collector/collector.go:628-660`) handles any metric whose type isn't numeric/enum/bits — the branch at `collector/collector.go:654-659`:

```go
// For strings we put the value as a label with the same name as the metric.
// If the name is already an index, we do not need to set it again.
if _, ok := labels[metric.Name]; !ok {
    labelnames = append(labelnames, metric.Name)
    labelvalues = append(labelvalues, pduValueAsString(pdu, metricType, metrics))
}
```

So `sysName` is emitted as its own metric family, with the metric's own name reused as the label name, value 1:

```
sysName{sysName="Sw-Core-01.lab.example.net"} 1
```

Consequences that matter for the join we want:

- The label name is fixed at `sysName` (mixed case, exactly the MIB object name) — Alloy passes labels through unmodified, and this repo cannot change that spelling upstream.
- The value is **exactly what the device returned** — no case-folding, no trimming beyond whatever gosnmp/gosnmp-decoding does. Real fleets return mixed-case hostnames (`Sw-Core-01`, `CORE1.example.net`).
- `sysName` is *not* a label on any `ifIndex`-keyed row (`ifHCInOctets`, `ifOperStatus`, etc.). Those rows carry only `ifIndex`/`ifDescr`/`ifName`/`ifAlias`. The only label every series scraped from one target shares — `sysName{sysName=...}=1`, `ifHCInOctets{ifIndex=...}=<counter>`, everything else — is Prometheus's own `instance` label, which is **added by the scrape config's relabeling**, not by snmp_exporter, and is normally set to the SNMP target address/hostname the operator configured (`__param_target__` → `instance`, the standard multi-target relabel pattern), not to `sysName`.

Net: correlating topology-exporter's device identity with snmp_exporter's interface metrics for the same box is a **two-hop join today, and it's a value join, not a free one**:

```promql
ifHCInOctets
* on(instance) group_left(sysName)
label_replace(sysName, "instance_dummy", "", "", "")  # sysName carries no `instance`-shaped join key of its own beyond the shared `instance` label
```
more precisely:
```promql
ifHCInOctets
* on(instance) group_left(sysName) sysName
```
joins interface counters to the device's raw-case sysName, and a *second* join is then needed to reach topology-exporter's edge/device series, because topology-exporter's device identity is not `instance`-shaped (see §3).

This resolves the open question: **sysName is a label, at label name `sysName`, joinable only via the shared `instance` label within snmp_exporter's own output, in original device case.** Everything in §3–§5 is designed around that fact: snmp_exporter does not give us a clean cross-metric `sysName` lookup.

## 3. Current state, re-verified against source

- `network_topology_device_info{device_id,vendor,model,os_version,site}` and `network_topology_edge_info{src_device,src_port,dst_device,dst_port,discovery_proto,link_kind,direction}` are declared in `internal/metrics/topology_collector.go:54-71` and populated in `Collect` at `internal/metrics/topology_collector.go:136-156`.
- `device_id` = `discovery.Device.ID`, documented at `internal/discovery/discovery.go:113` as "sysName (normalised lowercase); fallback: management IP". The lowercasing is `NormaliseName` (`internal/discovery/snmp/pdu.go:447-463`): strips control characters, `strings.ToLower(strings.TrimSpace(s))`, then truncates at a UTF-8 boundary to 255 bytes. The fallback assignment is `internal/discovery/snmp/snmp.go:514-517` (`dev := &discovery.Device{ID: p.IP.String(), ...}`), overwritten if `sysName` decodes to a non-empty string (`snmp.go:526-530`).
- **`discovery.Device` has no IP field.** Its full field set (`internal/discovery/discovery.go:112-120`) is `ID, Vendor, Model, OSVersion, Site, Uptime, Labels`. The management IP used as the `ID` fallback (`p.IP` at `snmp.go:511,515`) is in scope at construction time but is never persisted separately — once `sysName` resolves, the IP is gone. This matters for §5.
- `discovery.Edge.SrcPort`/`DstPort` are plain strings (`internal/discovery/discovery.go:288-289`). `ifIndex` is decoded and used transiently inside every walker, then discarded:
  - LLDP: `resolveLocalPort(k.portNum, locPorts)` (`internal/discovery/lldp/lldp.go:335,403-415`) — `portNum` is `lldpLocPortNum`, IF-MIB-index-equivalent per 802.1AB, used only to look up a name string, never stored on `Edge`.
  - CDP: `ifNames[k.ifIndex]` with `if%d` fallback (`internal/discovery/cdp/cdp.go:183-185`).
  - FDB: `ports[bridgePort]` → `ifIdx` → `ifNames[ifIdx]` (`internal/discovery/fdb/fdb.go:350,437,466,470`).
  - IS-IS: `circIfNames[circKey]` resolved via `isisISCircIfIndex` (`internal/discovery/isis/isis.go:109-126,188-199`), an **optional** enrichment — failure degrades `SrcPort` to empty rather than hard-failing the edge (`docs/architecture.md`'s IS-IS discovery-contract section; `network_topology_discovery_degraded_total{module="isis",reason="missing_srcport_mapping"}`).
  - Common resolution helper: `snmputil.WalkIfNamesWithFallback` (`internal/discovery/snmp/pdu.go:203-220`), ifXTable `ifName` with `ifTable.ifDescr` fallback.
- Confidence/Adjacency/PrecedenceRank/Metadata are deliberately excluded from `network_topology_edge_info` — this is issue #150, guarded by `TestEdgeInfoLabelSchemaStable` and `TestDeviceInfoLabelSchemaStable` in `internal/metrics/output_schema_guard_test.go:44-89`, and documented at length in the projection-divergence comments on `discovery.Device` (`discovery.go:105-111`) and `discovery.Edge` (`discovery.go:264-291`). The tests fail with an explicit message pointing back at #150 and at reviewing the OTLP/YANG projections — this is the existing, tested mechanism this proposal has to go through, not around.
- OTLP: `deviceAttrs` (`internal/output/otlp/otlp.go:108-124`) emits `attribute.String("device", sanitizeUTF8(dev.ID))` — key `"device"`, not `"device_id"` — plus `vendor`/`model`/`os_version`/`site`, each conditionally omitted when empty. `edgeAttrs` (`otlp.go:89-106`) emits the full Edge field set including `confidence`, `adjacency`, `precedence_rank`, and `network.topology.<key>` metadata pass-through, and uses attribute key `"proto"` (not `discovery_proto`). `docs/otlp-schema.md:20,38` confirms both as documented, current behavior. `docs/otlp-schema.md:44-48` states the OTLP versioning policy directly: *"Attribute removals or renames require a major version bump and CHANGELOG notice."*

**`Device` has no IP field.** This is the specific gap that makes an IP-based join impossible today, even though the IP is already in hand at discovery time; it matters for §5.

## 4. Decision: add `src_if_index` / `dst_if_index` to `network_topology_edge_info`

Add two labels, `src_if_index` and `dst_if_index`, carrying the stringified `ifIndex` for each endpoint, alongside the existing `src_port`/`dst_port`. Add corresponding `SrcIfIndex int` / `DstIfIndex int` fields to `discovery.Edge` (zero = unresolved), populated from the same value each walker already decodes and currently discards (§3): `k.portNum` in LLDP, `k.ifIndex` in CDP, `ifIdx` in FDB, the IS-IS circuit-to-ifIndex mapping. Emit `""` (empty label) when unresolved, exactly mirroring how `SrcPort` already degrades to empty today for IS-IS's optional enrichment failure (`missing_srcport_mapping`) — no new failure mode, no new degraded-mode plumbing required, the label just rides on the existing resolution outcome.

BGP/OSPF edges (`LinkKind: LinkKindIP`) are routing-protocol adjacencies, not L2 interface observations in the same sense; they may have an ifIndex, or none, depending on whether the vendor MIB row is interface-scoped. Recommendation: populate when resolvable, leave empty otherwise — treat it as optional metadata on those edges the same way port names already are.

**Why this doesn't reopen the #150 cardinality guard the same way Confidence/Adjacency/PrecedenceRank/Metadata would.** The fields #150 excluded vary independently of the edge's identity — the same physical link can flip `confidence`/`adjacency` between cycles, and `Metadata` is genuinely free-form per-observation data. `ifIndex` is a functional dependent of `(src_device, src_port)`: for a fixed device and a fixed interface name, the ifIndex is (almost always) constant. It contributes the same cardinality that `src_port` already contributes — it does not multiply the series count, it adds bytes to an existing series's label set. The guard test's failure message will fire regardless (`internal/metrics/output_schema_guard_test.go:67-71`); this document is the record for why that trip is expected and acceptable rather than a regression, and why the argument used for `Confidence` et al. does not apply here.

**Caveat, not resolvable without device-level testing:** IF-MIB `ifIndex` persistence across reboots/reloads is a SHOULD, not a MUST, in the IF-MIB spec; some platforms reassign `ifIndex` on reboot or after a software upgrade touching the interface table. A label pinned to a specific numeric `ifIndex` can go stale under a long-lived dashboard/recording rule in a way `ifName`/`ifDescr` mostly doesn't (names are far more stable in practice). This is not a new problem this proposal introduces — snmp_exporter's own `ifIndex`-keyed series have the identical exposure — but it means `src_if_index`/`dst_if_index` should be documented as "the join key snmp_exporter also uses, with the same caveats," not as more durable than `src_port`/`dst_port`.

Naming: `src_if_index`/`dst_if_index` (snake_case, matching this repo's convention: `discovery_proto`, `link_kind`) rather than mirroring snmp_exporter's literal `ifIndex` spelling. This doesn't save the operator a `label_replace` at query time regardless of which spelling is picked — PromQL vector matching (`on()`, `group_left()`) requires identical label *names* on both sides of a join, and no name choice here makes `src_if_index` and `ifIndex` collide automatically. A `label_replace` (or a recording rule that does it once) is required either way. This proposal picks repo-internal consistency over faux-compatibility with a name Alloy would never actually reproduce unmodified next to `src_device` in the same series regardless.

## 5. Decision: canonical device-identity join key

Three things currently claim to identify "the same device." They should collapse to one **between this repo's own two outputs** (Prometheus, OTLP); it cannot collapse into snmp_exporter's own label at all, because that label's name (`sysName`) is fixed by the upstream MIB-driven config and out of this repo's control, and because — per §2 — even matching *values* requires a two-hop join through `instance` that has nothing to do with what we name our own label. So the real deliverable here is: pick one name for our own device identity, and add the specific extra fields that make joining against snmp_exporter's actual (not idealized) output possible.

**5.1 — `device_id` vs `"device"` (Prometheus vs OTLP): standardize on `device_id`.**

Rename the OTLP attribute key from `"device"` to `"device_id"` (`otlp.go:110`). Justification: `device_id` is already the tested, documented Prometheus convention (`output_schema_guard_test.go:83`, `docs/metrics.md:9`); the divergence in OTLP has no stated rationale in the code — contrast with the Confidence/Adjacency/PrecedenceRank omission, which *is* explicitly justified (cardinality) at `discovery.go:105-111`. There is no equivalent justification on record for why OTLP says `"device"` instead of `"device_id"`; it reads as an unintentional naming drift, not a deliberate per-output decision like the field-set divergence the same comment block defends. Unifying the *name* here doesn't touch the deliberate field-set divergence (OTLP still carries the same value, still omits empty fields the way it does today) — it is a pure rename, isolated from the #150 decision.

This is still a breaking OTLP wire change under `docs/otlp-schema.md:44-48`'s own stated policy ("Attribute removals or renames require a major version bump and CHANGELOG notice") — see §7.

YANG output is left out of this unification. It doesn't use a flat attribute-name convention at all (RFC 8345 node/link identifiers), so "device_id vs device" doesn't apply to it as a rename question.

**5.2 — Add a case-preserving `sys_name` attribute, in parallel with `device_id`/`device` (not a replacement).**

This is the concrete answer to "how do you actually join against what snmp_exporter emits." `device_id`/`device` is `NormaliseName(sysName)` — lowercased. snmp_exporter's `sysName` label carries the device's exact original casing (§2). **PromQL has no case-folding function** — no `lower()`/`upper()`, `label_replace`'s regex substitution can rewrite label names/values structurally but cannot fold case. So for any device whose reported `sysName` isn't already all-lowercase (which, empirically, is most fleets that use mixed-case hostnames), `device_id` and snmp_exporter's `sysName` label value **cannot be matched by any PromQL query** — this is unbridgeable at query time, not merely an extra step.

Proposal: capture the case-preserving equivalent of `NormaliseName` — control-character strip, trim, 255-byte truncation, **no `ToLower`** — into a new field, and expose it as label `sys_name` on `network_topology_device_info` and attribute `sys_name` on the OTLP device gauge. This gives an operator an exact-string join against snmp_exporter's `sysName{sysName=...}` series with a `label_replace` for the label *name* only (unavoidable, not case-related) — no case massaging needed on either side.

This is additive, not a cardinality change on its own axis (1:1 with `device_id`, same reasoning as §4), but it does trip the same guard test for `network_topology_device_info` (`output_schema_guard_test.go:83-88`).

**5.3 — Add `management_ip`, unconditionally populated, independent of whether `sysName` resolved.**

Per §3, `Device` has no IP field today; the IP is discarded the instant `sysName` resolves. Many real deployments configure snmp_exporter's scrape targets by IP, in which case snmp_exporter's `instance` label *is* the management IP, and the cleanest join of all — one hop, `on(management_ip=instance)` after a trivial `label_replace` — becomes available with no case issues and no dependency on `sysName` having resolved at all (works even for devices topology-exporter only knows by IP). Add `ManagementIP string` to `discovery.Device`, set unconditionally from `p.IP.String()` at construction (`snmp.go:514-517`), independent of whether `ID` later gets overwritten by `sysName`. Expose as label `management_ip` on `network_topology_device_info` / OTLP device attributes.

**Which of `sys_name` or `management_ip` is the "right" join key is an operator-config question this proposal cannot resolve from the exporter's code alone** — it depends entirely on how the operator's snmp_exporter/Alloy scrape job is targeted (by IP vs. by hostname/FQDN) and on whether the operator runs the stock `snmp.yml` unmodified. Recommendation: ship both; document both join recipes in `docs/operator/`; let the operator pick based on their own scrape config. This recommendation is not verified against any specific deployment's actual Alloy scrape config; the two fields serve genuinely different deployment shapes, and neither clearly dominates.

## 6. Before / after

**`network_topology_edge_info` — before:**
```
network_topology_edge_info{direction="bidirectional",discovery_proto="lldp",dst_device="sw-acc-04",dst_port="Gi0/3",link_kind="ethernet",src_device="sw-core-01",src_port="Gi0/1"} 1
```

**`network_topology_edge_info` — after:**
```
network_topology_edge_info{direction="bidirectional",discovery_proto="lldp",dst_device="sw-acc-04",dst_if_index="10203",dst_port="Gi0/3",link_kind="ethernet",src_device="sw-core-01",src_if_index="10101",src_port="Gi0/1"} 1
```

**`network_topology_device_info` — before:**
```
network_topology_device_info{device_id="sw-core-01",model="C9300",os_version="17.6.4",site="lab",vendor="cisco"} 1
```

**`network_topology_device_info` — after:**
```
network_topology_device_info{device_id="sw-core-01",management_ip="10.20.30.1",model="C9300",os_version="17.6.4",site="lab",sys_name="Sw-Core-01",vendor="cisco"} 1
```

**OTLP device attributes — before:**
```
device="sw-core-01", vendor="cisco", model="C9300", os_version="17.6.4", site="lab"
```

**OTLP device attributes — after:**
```
device_id="sw-core-01", sys_name="Sw-Core-01", management_ip="10.20.30.1", vendor="cisco", model="C9300", os_version="17.6.4", site="lab"
```

**The join this unlocks, concretely** (assumes snmp_exporter scraped with `instance` = management IP, the `management_ip` path from §5.3):
```promql
ifHCInOctets
* on(instance) group_left(src_device, src_if_index)
label_replace(
  network_topology_edge_info,
  "instance", "$1", "management_ip", "(.*)"
)
```
(This still requires resolving `management_ip` per edge endpoint rather than per whole edge series — a recording rule that explodes the edge into two per-endpoint rows, or a dashboard-side `on(src_device_ip)`/`on(dst_device_ip)` pair of queries, is the realistic shape; the single-line example above is illustrative of the label mechanics, not a drop-in query.)

## 7. Wire-compatibility and migration

This repo has an established pattern for exactly this kind of change, documented in `docs/metrics.md`:

- **Issue #20**: widened `{status}` → `{status, reason}` on three metrics. Documented inline as "**Breaking change (issue #20)**," with the specific migration ("dashboards counting 'all failures' must use `sum by (status)(...)`") spelled out in the same table row.
- **Issue #27 / #31 / #15** (`network_topology_bgp_walker_outcome_total`): label *value* semantics changed (`no_peers` split, `v2_draft` removed at v1.3.0) — each documented with an explicit "Breaking change (issue #N[, vX.Y.Z])" note and a one-line migration instruction.
- **Issue #98**: the counterpart precedent for *non*-breaking — a new metric was added rather than an existing one relabeled, called out explicitly as "Additive and non-breaking: the BGP counter was **not** renamed."

This proposal's changes are closer to #20/#27/#31 than to #98: they change the label *schema* of two metrics that already exist and are presumably already graphed/alerted on. Recommendation, following the established pattern exactly:

1. File an issue (next number in the tracker's sequence at implementation time — not fabricated here) and reference it from the `docs/metrics.md` rows for `network_topology_edge_info` and `network_topology_device_info`, in the same "**Breaking change (issue #N)**" phrasing used for #20/#27/#31.
2. Update `output_schema_guard_test.go`'s `want` slices for both tests (`internal/metrics/output_schema_guard_test.go:62-65,83`) in the same PR — the guard is designed to force this, per its own failure message ("If intentional, update this test...").
3. Bump the minor version at minimum (additive labels, no removals, so this is not automatically a SemVer-major change by the project's own `CHANGELOG.md`/`docs/release.md` convention) — **unless** the OTLP `"device"` → `"device_id"` rename in §5.1 ships in the same release, in which case `docs/otlp-schema.md:44-48`'s own stated policy ("removals or renames require a major version bump") forces a major bump for that release regardless of how the Prometheus-side additions would have been classified alone. Recommendation: ship the OTLP rename in the same release as the Prometheus additions rather than splitting them — once the rename forces a major bump, one version bump with one migration note is cleaner than staging a minor-then-major sequence for a single coherent piece of work.
4. Because purely *additive* Prometheus labels do not break exact-match label selectors already in dashboards/alerts (`{src_device="x"}` still matches), the operator-facing risk is narrower than #20's — but it is not zero: any existing recording rule or alert using `without(...)`-style aggregation without an explicit label list, or any external system asserting an exact label *set* (this repo's own guard test is proof that such assertions are a real pattern, not hypothetical), breaks. Document this the way #20 documents its own migration: name the query shape that breaks and what to change it to (`sum by (...)` with the fields the query actually cares about, not `without()`).
5. The `sys_name`/`management_ip` additions to `network_topology_device_info` carry SNMP-response-derived free text (`sys_name`, exact device-reported casing) and a raw IP address (`management_ip`) into label values for the first time on this metric — `docs/metrics.md:3` states "No metric uses a raw IP address ... as a label value" as a repo-wide invariant. **`management_ip` as proposed violates that invariant as currently stated**, and either the invariant needs an explicit, documented exception for this one field (analogous to how `discovery.Device.ID`'s IP-fallback already puts an IP into `device_id` today when `sysName` is unavailable — so the invariant already has a de facto exception for the fallback case, just not a labeled/first-class one) or `management_ip` should be dropped from this proposal. This is a real conflict with an existing, explicitly stated design rule, not an editorial nit, and needs a decision from whoever owns that invariant (`internal/metrics/metrics.go:7`) before implementation.

## 8. Non-goals

- Not proposing any change to `docs/otlp-schema.md`'s Edge attribute set (`proto` vs `discovery_proto` naming) — out of scope for this device/interface-identity proposal; flagged as a similar, separate naming inconsistency a reviewer may reasonably ask "why not fix that too" — the answer is scope discipline, not that it's a non-issue.
- Not proposing changes to snmp_exporter or Alloy. Both are read-only inputs to this design; this repo has no ability to change what label name or casing upstream emits.
- Not proposing a change to YANG output labeling conventions (§5.1).
- Not proposing to remove or restructure `Confidence`/`Adjacency`/`PrecedenceRank`/`Metadata`'s Prometheus exclusion (#150) — this proposal's `ifIndex`/`sys_name`/`management_ip` additions are argued (§4, §5.2) to be a materially different cardinality case, not a rollback of #150's reasoning.

## 9. Open questions for reviewers

- **Assumption to double-check**: this proposal assumes the reader's Alloy/snmp_exporter deployment runs the stock `if_mib`/`system` modules from the default `snmp.yml` unmodified. A custom `generator.yml` could add a `lookups:` entry wiring `sysName` onto interface rows directly, which would remove the two-hop join problem entirely and might change which of §5.2/§5.3's fields is actually needed. §2's finding is about the upstream *default*, not about any specific operator's customized module set.
- **Assumption to double-check**: §5.3 assumes IP-keyed scrape targets are common enough to justify `management_ip`. If this deployment's SNMP scrape jobs are hostname/FQDN-keyed, `management_ip` buys nothing and `sys_name` (§5.2) is the only join path that matters — in which case §7 item 5's raw-IP-label conflict may be moot in practice (drop `management_ip`, keep `sys_name`, and the invariant conflict disappears).
- The `ifIndex` persistence caveat (§4) is stated from general IF-MIB knowledge, not from testing against this project's actual device fleet. Worth validating against whatever vendor/OS matrix `docs/supported-platforms.md` covers before treating `src_if_index`/`dst_if_index` as a long-term-stable join key in a shipped dashboard.
- §5.1's OTLP rename is presented as a clear win with no real counter-argument found in the code (no comment anywhere justifies `"device"` over `"device_id"`). An adversarial reviewer should specifically check whether any existing external consumer (a shipped Grafana dashboard, an alert rule, a customer integration) already depends on the attribute key `"device"` — if one exists, the "no rationale on record" argument for renaming still holds, but the migration cost this document estimates as low (one rename, documented, major-bump) could be understated.
- This document does not draft the actual recording rules/dashboard panels that would consume the new labels — §6's PromQL is illustrative of label mechanics only, explicitly marked as not a drop-in query. That is a follow-on implementation task, not a design-spec gap.
