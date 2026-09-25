# Review summary — snmp-exporter-label-alignment / alloy-integration / knowledge-graph-boundary-stitching / core-hub-split

Five reviews (adversarial, security, network-operations, Grafana-ecosystem-fit, software-architecture) ran against all four proposals as one bundle. This is the coordinator's triage: what's confirmed, what's disputed, and what it means for the plan.

## The one finding that changes the plan

**Adversarial review, finding 1 (verified, critical):** `grafana/asserts-adi` has a real, currently-maintained Knowledge Graph **Write API** (`KgWriteController`, `docs/kg-write-api.md`, last touched the day of this review) supporting arbitrary relation types and a genuine per-edge `properties` object (including confidence). Both the KG spec and the core-hub-split spec concluded no usable write path exists — but both only grepped `yoda/`, never `inference-engine/api-server/` or the top-level docs. That conclusion was wrong, and it was load-bearing: the KG spec's Tier-1-only scoping decision, and core-hub-split's rejection of a "push overrides via KG API" design, both rest on it.

The adversarial reviewer's finding 11 follows from this: a much smaller MVP is available — push directly to the KG Write API from the exporter's own uncoordinated-mode recording rule (or the exporter itself), instead of authoring a `PROPERTY_MATCH`/`METRICS` relationship rule at all. This would sidestep three separate problems other reviewers found independently (below) in one move, and nobody evaluated it because nobody read past `yoda/`.

This has since been revised — see "Resolution (revision completed)" below for what changed.

## Confirmed, independent findings (multiple reviewers converged)

- **The draft KG rule's "confirmed" match is actually broken** (security, HIGH; independently load-bearing for adversarial's case too): the Tier-1 match groups by `peer_a,peer_b,proto,asserts_env` but not `reporting_device` — a single device reporting the same out-of-scope neighbor on two different ports satisfies "both sides confirmed" without a second device ever being involved. This directly contradicts the KG spec's claim of matching Federation hub's correctness (hub's real reverse-key check requires two *distinct* reporting devices). Exploitable accidentally (any dual-homed neighbor) or adversarially (one rogue device, two links in, forged LLDP identity).
- **core-hub-split's own recommended pairing doesn't wire together** (adversarial, verified): its §4.3 manual-override injection writes to `network_topology_edge_info`; the KG rule only reads `network_topology_boundary_observation_info`. A manually configured override would work locally and never reach the Knowledge Graph — a silent gap.
- **The `when:` gate may never fire for anyone** (Grafana-ecosystem-fit + adversarial, both independently): the draft rule's `when: [network_topology_exporter]` gate needs a companion vendor-detection file (pattern: `3po/.../vendor/*.yml`) that doesn't exist and isn't identified as a required artifact anywhere in the spec.
- **No bidirectional-link precedent exists in Asserts' ruleset**, and whether the graph backend even traverses a stored one-directional relation both ways is unverified (KG spec's own open question, sharpened by adversarial with a citation to asserts-adi's Cypher test code suggesting a directional lean).
- **Two separate places in this repo's own `docs/architecture.md` overstate already-implemented behavior** that the code doesn't actually do (adversarial, LD-15's normalization claim; core-hub-split's independent catch on LD-19) — a pre-existing doc-accuracy problem this project surfaced, unrelated to whether any of the four proposals ship.
- **Alloy's `discovery.*` component restart is worse than "stale" for a slow discoverer — it can flap to an empty target list** (adversarial, verified against real Alloy source: a 5s ticker + `haveUpdates` default can emit empty before a slow discoverer repopulates). Topology-exporter's 60s+ BFS crawl is exactly the slow case this hits. The Alloy spec designed around staleness, not flap-to-zero.
- **Dynamic, LLDP/CDP-driven SNMP polling scope fights real NOC practice** (network-ops): curated/CMDB-driven inventory is deliberate, for change-management and audit reasons — BFS-driven scope creates the worst-case gap the Alloy spec itself names (a device losing monitoring coverage exactly when its last link dies). Should ship as a drift-detection signal, not a live scrape authority.
- **`management_ip` conflicts with a real, currently-enforced repo invariant** ("no raw IP as a label value," `docs/metrics.md`) — confirmed cardinality-motivated as written, not security-motivated (security review), but still unresolved by any of the four docs (architecture review, independently).
- **Never fully decommission the Federation hub on first KG parity result** (network-ops) — there's no un-adoption path once it's gone; keep it running with a diff-alert during a trust-building window.
- **The core-hub-split's real win is dependency-surface isolation, not "smaller for Alloy"** (network-ops + Grafana-ecosystem, converging independently): the measured 18,322 `k8s.io/client-go` symbols removed from every non-hub build is a security/footprint win for every self-hosted operator, regardless of Alloy. Worth reframing the headline rationale.
- **The `cmd/` binary-split alternative (single binary + mode flag, build tags) was never explicitly weighed** (Grafana-ecosystem + architecture, independently) — Loki/Mimir/Tempo and Alloy itself use that pattern for the same class of problem. The two-binary design likely still wins, but the doc should show the comparison, not skip it.

## Lower-severity / good-to-know

- OTLP has no schema-pinning test equivalent to the Prometheus side's `output_schema_guard_test.go`, so the proposed `device`→`device_id` rename has no automated guard (architecture).
- The `management_ip`/OTLP-rename/additive-label changes should ship on separate timelines with a compatibility window, not as one release (network-ops, architecture).
- The KG rule YAML's comment density (57%, vs. 0–15% in real production rule files) reads as written-from-secondhand-description rather than in-house — a style/trim issue, not a correctness one (Grafana-ecosystem).
- A hand-rolled `http_sd` endpoint needs to actually return `Content-Type: application/json`, or upstream Alloy's `discovery.http` silently rejects it (Grafana-ecosystem).
- No hub↔spoke version-skew/compatibility policy is documented — pre-existing gap, made newly visible by the split, not introduced by it (network-ops).

## What reviewers signed off on cleanly

- The `src_if_index`/`dst_if_index` interface-identity join and the `device`→`device_id` OTLP rename are well-justified and ecosystem-appropriate (architecture, Grafana-ecosystem).
- The core/hub split's dependency measurement methodology (built the binary, ran `go tool nm`) is real and its LOC accounting is honest about what doesn't shrink (architecture, network-ops).
- The reconciliation between the label-alignment spec's `sys_name` proposal and the KG spec's `NormaliseName`-based fix is genuinely clean — they're two different, non-conflicting fixes for two different joins, not a contradiction (architecture, independently re-derived).
- Credential handling and the mTLS trust boundary are unaffected by the core/hub split (security, verified clean — a real non-finding, not an oversight).

## Resolution (revision completed)

`knowledge-graph-boundary-stitching.md` has been fully rewritten around pushing to the real KG Write API (`docs/kg-write-api.md`, `KgWriteController`/`KgWriteService` in `asserts-adi`, confirmed by direct read — `allowUndeclared=true`, MERGE-upsert entity/relationship identity, a real per-write `properties` object, mandatory self-expiring `ttlSeconds`) instead of authoring a Yoda relationship rule. The replacement design is a small push job that ports `internal/federation/hub_merge.go`'s actual distinct-reporting-device matching logic in Go against raw `network_topology_boundary_observation_info` samples — which directly fixes the security-critical Tier-1 bug (it's no longer a PromQL grouping bug, because the job isn't doing PromQL aggregation for the match at all) and sidesteps the `when:`-gate risk entirely (no Yoda rule exists to gate). `docs/proposals/relationships_network_topology.yml` was deleted and replaced with `docs/proposals/kg-boundary-pusher-sketch.md` (Go pseudocode + a validated example `/graph` batch payload). `core-hub-split.md` §4 was updated in place — the old "write API doesn't hold up" conclusion is kept but marked corrected (not silently deleted), and manual overrides (`known_inter_domain_links`) now route through the same push job, closing the gap where they never reached the graph.

**Still genuinely open, stated honestly rather than papered over**: whether the Asserts graph backend traverses a one-directional relationship both ways (the design pushes both directions explicitly as a hedge, matching what `hub_merge.go` already does, but the underlying backend-semantics question is unresolved); whether any gateway-level rate limit exists on the Write API (nothing found in the app-level code, infra layer unchecked); and where the extracted matching logic and override config should live as a follow-on implementation decision.

## Where this leaves you

Nine-plus documents in `docs/proposals/` (four specs, five reviews, this summary, plus the pusher sketch) — all uncommitted, nothing pushed anywhere. This is a reviewed design, not yet-approved code. Recommended reading order: this summary → the four specs → `kg-boundary-pusher-sketch.md` → the five individual reviews if you want the full reasoning behind any specific finding.
