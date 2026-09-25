# Proposal: split `internal/federation`'s hub into an optional module

Status: draft for review. Not implemented. No code in this repo has been
changed to produce this document.

## 0. Question this proposal answers

Given the other three specs in this series — label alignment, Alloy
integration, and Knowledge-Graph boundary stitching — should
`network-topology-exporter` split into a minimal "core" (standalone use,
Alloy embedding via the http_sd endpoint, Grafana-Cloud-native deployments
relying on KG for cross-boundary stitching) and an optional
"federation-hub" component, and if so, along what boundary?

**Recommendation, up front:** yes, but the boundary is narrower than
"remove `internal/federation`." Federation bundles three genuinely separable
concerns today — (1) the uncoordinated-mode boundary-observation metric, (2)
the spoke→hub push transport, (3) the hub's aggregation/matching/HA logic —
and only the third is expensive enough, and separable enough, to justify
carving out. (1) is already free-standing (§2.4). (2) is small, has no
heavy dependencies, and stays in the core binary so `role: spoke` keeps
working without pulling in the hub (§5). (3) — `hub.go` and its seven
sibling files, plus the Kubernetes leader-election code — is what actually
costs something today: it drags `k8s.io/client-go`'s full dependency tree
into **every** build of the one existing binary, regardless of
`federation.role` (§2.3, measured). Carving (3) into a second `cmd/`
binary removes that unconditionally, is a pure repackaging with no behavior
change, and can ship before any of the KG proposal's open questions are
resolved (§6). What it does *not* do is eliminate the operator-facing need
for `known_inter_domain_links` (§4) — that table's job has no proven
KG-native replacement yet, so "hub" or something functionally identical to
it has to keep shipping as long as operators rely on manual boundary-link
overrides.

## 1. Reconciling the prior three specs

Two things from the background brief for this document turned out not to
match what the prior specs actually concluded, checked directly against
their text rather than the paraphrase:

**The `sys_name` vs. neighbour-hint-normalisation discrepancy is real, and
the KG spec's finding is the one this document builds on.** The label
spec (`snmp-exporter-label-alignment.md` §5.2) proposes a new
case-preserving `sys_name` label so PromQL can join topology-exporter's
`device_id` against snmp_exporter's raw-case `sysName` label — a real gap,
but for a *different* join (this exporter's device identity against
snmp_exporter's). The KG spec (`knowledge-graph-boundary-stitching.md`
§4.5) traced the *actual* case-mismatch bug in cross-boundary stitching
end-to-end at the Go source level and found it is narrower: `reporting_device`
is always `dev.ID`, already lowercased by `NormaliseName`
(`internal/discovery/snmp/pdu.go:447-463`, confirmed again by the label
spec's own §3), but the neighbour-hint half of every boundary observation is
**not** normalised — `internal/discovery/lldp/lldp.go:362-363` and
`internal/discovery/cdp/cdp.go:197-198` pass the raw LLDP/CDP-decoded name
straight into `NewOutOfScopeNeighbour` with no case-folding
(`internal/discovery/snmp/scope.go:28-37`). `canonicalPair`
(`internal/metrics/topology_collector.go:194-199`) is a plain lexicographic
sort — no case-folding — so a boundary pair only matches today if the raw
neighbour hint happens to already be lowercase. The KG spec is explicit that
this is the narrower, correct fix and that `sys_name` is not load-bearing for
it: "apply `NormaliseName` to the neighbour-hint string... at each of the ~5
call sites... zero dependency on `sys_name` landing" (§4.5-4.6). I did not
re-derive this; I re-read both specs' own citations and confirm the KG
spec's finding is consistent with the label-alignment spec's own data (its
§3's `NormaliseName` citation and its §5.2's separate, correctly-scoped
`sys_name` justification for the snmp_exporter join). **These are not
competing fixes for the same problem** — `sys_name` solves the
exporter↔snmp_exporter join; the ~5-call-site `NormaliseName` fix solves the
exporter↔exporter cross-boundary join. Both should ship; neither
substitutes for the other. This document assumes the neighbour-hint fix is
in scope as cheap, independent, and prerequisite-free groundwork for
whichever stitching mechanism (Mimir recording rule, hub, or KG Tier 1) is
actually in use, since all three key off the same `peer_a`/`peer_b` string
match.

**Correction (post adversarial review): the "KG write API" premise this
paragraph originally rejected is real, and §4 below has been rewritten to
build on it.** The original text here said the premise "could not be
substantiated against the actual KG spec," because grepping the KG spec and
its companion YAML for `api` returned zero matches — but that only shows
neither document's *author* had found the API, not that it doesn't exist.
An adversarial review of this document bundle
(`docs/proposals/reviews/adversarial-review.md`, Finding 1, "[VERIFIED]")
found that `grafana/asserts-adi` has a real, currently-maintained Knowledge
Graph Write API (`KgWriteController`, `docs/kg-write-api.md` in that repo),
reachable at runtime (not a build-time artifact merged via PR), that both
this document and the KG spec missed because both authors' research was
scoped to `yoda/` and never reached `inference-engine/api-server/` or the
top-level `docs/`. `knowledge-graph-boundary-stitching.md` has been
rewritten around this API; §4 below now recommends the KG-write-API push
job it describes, in place of the local-injection alternative this section
originally proposed as the fallback once the write API was believed not to
exist.

## 2. What's actually in `internal/federation` today

### 2.1 File-by-file, with the concern each one serves

| File | LOC (src) | LOC (test) | Concern |
|---|---|---|---|
| `payload.go` | 31 | — | Wire type (`SpokePayload`) — shared by both sides |
| `spoke.go` | 267 | 493 (`spoke_test.go`) | Push transport: mTLS client, retry/backoff |
| `hub.go` | 163 | 90 (`hub_test.go`) | Hub struct, constructor, panic recovery, leader accessors |
| `hub_merge.go` | 270 | 485 | Combined-graph construction, OOS name-matching (Tier 1), `known_inter_domain_links` injection |
| `hub_publish.go` | 81 | 203 | Generation-fenced commit path |
| `hub_push.go` | 363 | 910 | `/spoke/push` HTTP handling, structured reject contract |
| `hub_server.go` | 76 | 283 | mTLS server bootstrap/lifecycle |
| `hub_snapshot.go` | 111 | 372 | LD-13 snapshot writer goroutine |
| `hub_validate.go` | 278 | 701 | Payload validation (label-safety, structural invariants) |
| `hub_eviction.go` | 86 | 209 | LD-18 silent-spoke eviction |
| `elector.go` | 21 | — | `LeaderElector` interface |
| `elector_k8s.go` | 169 | 40 (`elector_fake_test.go`) | Kubernetes Lease-based HA leader election |
| `ha_test.go`, `hub_panic_test.go`, `tracing_propagation_test.go` | — | 333+41+102 | Cross-cutting hub tests |

(`wc -l`, verified 2026-09-22.) Totals: package is 6,178 lines including
tests, 1,916 non-test. Of the non-test total, **1,618 lines (84%) are
hub-only** (`hub*.go` + `elector*.go`); **267 lines (14%) are spoke-only**
(`spoke.go`); **31 lines (2%) are the shared wire type**. Of the 4,262 test
lines, roughly 3,667 exercise hub-only code and roughly 595 exercise
spoke-only code (`spoke_test.go` + a share of `tracing_propagation_test.go`,
which tests both directions of the trace-context propagation and so isn't
cleanly attributable to one side).

### 2.2 The app-level coupling is already narrow

`internal/app` imports `internal/federation` in exactly three files:
`app.go`, `loop.go`, `spoke_pusher.go`. Confirmed by
`grep -rl 'internal/federation"' . | grep -v _test.go`. Nothing in
`internal/discovery`, `internal/graph`, or `internal/metrics` imports it —
`internal/metrics/reject_reason.go:9-10` states the dependency direction
explicitly the other way ("internal/federation already imports
internal/metrics; locating [RejectReason] here avoids a cycle").

Inside `internal/app`:

- `loop.go`'s federation-specific surface is `LoopConfig.Spoke
  *federation.Spoke` (one field), the `pusher := newSpokePusher(...)` /
  `go pusher.run(ctx)` block gated on `lc.Spoke != nil`
  (`loop.go:429-433`), and the `pusher.Enqueue(...)` call inside `publish`
  (`loop.go:280-288`) — roughly 15-20 lines out of 515.
- `spoke_pusher.go` (130 lines) is 100% federation-transport: an
  async, latest-only mailbox that decouples the spoke push from the
  discovery cycle so a slow hub can't stall or evict the spoke.
- `app.go`'s `switch cfg.Federation.Role` (`app.go:224-419`) is the one
  real branch point: `case config.RoleHub` (~150 lines, including the
  opt-in HA leader-election wiring) runs `federation.NewHub(...)` +
  `hub.Serve(ctx)` and **no local discovery loop at all** — confirmed by
  the function's own comment (`app.go:571-574`,
  `livenessMaxStale`: "a pure hub runs NO local discovery loop"). The
  `default:` branch (standalone/uncoordinated/spoke) builds
  `federation.NewSpoke(...)` only `if cfg.Federation.Role ==
  config.RoleSpoke` (`app.go:379-387`) and otherwise never touches the
  package.

This confirms the Alloy spec's characterization was right in spirit for a
different question ("Alloy integration is additive, not a rewrite of the
core") and extends it: **federation itself is already bolted onto the app
package at a small number of narrow seams**, not woven through the
discovery/reconcile core. That's the good news. The bad news is in §2.3.

### 2.3 Measured: today's one binary always links `k8s.io/client-go`, regardless of role

`elector_k8s.go` imports `k8s.io/client-go/tools/leaderelection`,
`k8s.io/client-go/kubernetes`, `k8s.io/client-go/rest`, and
`k8s.io/apimachinery/pkg/apis/meta/v1` (`elector_k8s.go:10-14`). `go.mod`
lists `k8s.io/client-go`, `k8s.io/apimachinery`, `k8s.io/api`,
`k8s.io/klog/v2`, `k8s.io/kube-openapi`, and four `sigs.k8s.io/*` modules as
direct/indirect requirements. I built the actual binary
(`go build ./cmd/topology-exporter`, go1.27.1, module toolchain go1.26.4,
2026-09-22) and checked linked symbols directly rather than reasoning about
it from the import graph:

```
$ go tool nm topology-exporter | grep -c "k8s.io/client-go"
18322
```

18,322 `k8s.io/client-go` symbols are linked into the binary today. This
is not conditional on `federation.role` — Go links whole packages, and
`app.go:271`'s `federation.NewK8sLeaseElector(...)` call site is reachable
in the static call graph unconditionally (it's guarded by a runtime `if
cfg.Federation.Hub.HA.Enabled` check, not a build tag), so the linker cannot
prove it dead. **Every deployment shape this proposal cares about —
standalone, Alloy-embedded via http_sd, Grafana-Cloud-native with
KG stitching — ships the full Kubernetes client library and its transitive
dependency graph today, whether or not it ever runs the hub, and whether or
not it runs on Kubernetes at all.** This is the single most concrete,
measurable justification for a binary split in this document: it's not
about LOC (1,618 lines is small), it's about a large, security-relevant
dependency graph (a Kubernetes API client with RBAC/lease semantics) being
present in every build for a code path 100% of core-only operators never
execute.

### 2.4 Also measured: the uncoordinated-mode marker metric has zero coupling to `internal/federation`

`internal/metrics/topology_collector.go`'s `emitBoundaryObs` field
(constructor `newTopologyCollector(emitBoundaryObs bool, ...)`,
`topology_collector.go:49`) is a plain bool. It's set once, at startup, by
`m := metrics.New(cfg.Federation.Role == config.RoleUncoordinated)`
(`app.go:83`) — a comparison against `config.Role`, not a call into
`internal/federation`. The `boundaryObsDesc` metric descriptor
(`topology_collector.go:78-85`) and `canonicalPair` (`topology_collector.go:191-199`)
live entirely in `internal/metrics`, consuming `discovery.OutOfScopeNeighbour`
(a type `internal/discovery` already needs for LD-11's out-of-scope logging,
independent of federation). **`internal/federation` is never imported by
`internal/metrics`.** "Uncoordinated" mode — emit
`network_topology_boundary_observation_info` so an external Mimir
recording rule or the KG Tier-1 rule can do the join — costs nothing to
keep in a minimal core: it's not federation code at all, just a config-role
comparison plus a metric descriptor that already exists for other reasons.

## 3. Decision: split along the `cmd/` boundary, not a build tag

### 3.1 A real Go constraint this proposal has to respect

Everything discussed above lives under `internal/`. Go enforces
`internal/` package visibility by import path: a package outside this
module's root cannot import
`github.com/grafana/network-topology-exporter/internal/anything`, full
stop, regardless of directory layout. So "a separately-built
federation-hub module" cannot mean a genuinely separate Go module (its own
`go.mod`, published/versioned independently) unless `internal/discovery`,
`internal/graph`, `internal/metrics`, and `internal/snapshot` were first
promoted out of `internal/` into an importable package path — a much larger
and unrelated change this document is not proposing. The achievable split
is: **two `cmd/` binaries built from the same module**, each importing only
the subset of `internal/federation` (split into two packages) it needs. Go's
linker only includes symbols reachable from a given `cmd/`'s `main()`, so
this achieves the §2.3 dependency-isolation goal without touching module
boundaries at all.

### 3.2 The split

- **`internal/federation`** (core, stays): `payload.go` (31 lines) +
  `spoke.go` (267 lines) + `spoke_test.go`/`tracing_propagation_test.go`'s
  spoke-side coverage. Depends only on stdlib `crypto/tls`/`net/http`,
  `internal/config`, `internal/loglimit`, `internal/metrics`,
  `internal/tracing` — no Kubernetes client anywhere in this subset (confirmed:
  `spoke.go`'s import block, `spoke.go:1-32`, has none). `cmd/topology-exporter`
  keeps importing this package for `role: standalone | uncoordinated | spoke`
  exactly as today.
- **`internal/federationhub`** (new package, moves): `hub.go`,
  `hub_merge.go`, `hub_publish.go`, `hub_push.go`, `hub_server.go`,
  `hub_snapshot.go`, `hub_validate.go`, `hub_eviction.go`, `elector.go`,
  `elector_k8s.go`, and their tests — 1,618 non-test / 3,667 test lines,
  moved verbatim (rename package clause, adjust the handful of internal
  cross-references; no behavior change). This package is the only place
  `k8s.io/client-go` is imported anywhere in the tree.
- **`cmd/topology-hub`** (new, ~150-190 lines, mostly relocated from
  `app.go`'s existing `case config.RoleHub` branch): constructs
  `federationhub.Hub`, loads the LD-13 snapshot, wires the optional HA
  elector, calls `hub.Serve(ctx)`. This binary is the only one that pulls in
  `k8s.io/client-go`.
- **`cmd/topology-exporter`** (existing, unchanged behavior for
  `standalone`/`uncoordinated`/`spoke`): `federation.role: hub` becomes a
  **startup-rejected** configuration in this binary (fail fast with a clear
  error pointing at `cmd/topology-hub`) rather than a silently-supported
  branch, so a config file written for the old single-binary world fails
  loudly instead of doing nothing.

### 3.3 Before/after picture

| | Before (today) | After |
|---|---|---|
| Binaries | 1 (`cmd/topology-exporter`, 21 lines, thin entrypoint into `internal/app`) | 2 (`cmd/topology-exporter` unchanged; `cmd/topology-hub` new, ~150-190 lines) |
| `k8s.io/client-go` linked into | Always (measured, §2.3) | Only `cmd/topology-hub` |
| `internal/federation` | One package, 6,178 lines (src+test), spoke+hub+wire-type mixed | Two packages: `internal/federation` (spoke+wire-type, 298 src / ~600 test) + `internal/federationhub` (hub+HA, 1,618 src / 3,667 test) |
| Roles supported by `cmd/topology-exporter` | standalone, uncoordinated, spoke, hub | standalone, uncoordinated, spoke (`hub` rejected at config load) |
| Roles supported by `cmd/topology-hub` | n/a | hub (only) |
| `internal/app`'s `RunDiscoveryLoop`/`cycle.go`/`device_walk.go`/discovery walkers/`internal/graph` | Unaffected either way (§2.2 confirms zero coupling) | Unaffected |
| Operator-visible change | — | Hub operators run a different container image/binary; `known_inter_domain_links` and `federation.hub.*`/`federation.spoke_timeout` config keys move with it (see §6 for migration) |

This is a pure repackaging along an already-existing seam (§2.2's finding
that federation is bolted on narrowly is what makes this cheap) — no
*behavior* change for any deployment shape (metrics, config schema, wire
protocol, and federation semantics are byte-identical before/after for
every role), and it removes the one concrete, measured cost (§2.3) from
every build that isn't `cmd/topology-hub`. The table's last row is the one
deployment-*mechanics* exception: hub operators must switch which
image/binary they run. That is an operator-visible packaging change, not a
behavior change — nothing the hub does, accepts, or emits differs.

## 4. Decision: does `known_inter_domain_links` have to stay in a shippable hub?

### 4.1 The architecture doc's own framing overstates what's implemented today

`docs/architecture.md`'s LD-19 says `known_inter_domain_links` is "a list of
... tuples the hub **and uncoordinated recording rule** treat as
authoritative stitching overrides" (emphasis added). I checked this against
the actual code: `grep -rln "KnownInterDomainLinks"` returns exactly four
files — `internal/config/types.go` (the struct), `internal/config/validate.go`
(shape validation, runs regardless of role — `validate.go:390-407`),
`internal/discovery/discovery.go` (type reference), and
`internal/federation/hub_merge.go` (the only place it's actually *injected*
into a graph, `hub_merge.go:196-215`). **There is no code path where an
uncoordinated-mode instance applies `known_inter_domain_links` to anything**
— an uncoordinated deployment's Mimir recording rule would have to
hand-encode the override itself, entirely outside this exporter, which
`docs/operator/federation.md` confirms is the actual guidance today: its own
troubleshooting section tells operators who hit an uncoordinated-mode naming
mismatch to "use `known_inter_domain_links` **in a hub deployment**
instead" (`docs/operator/federation.md:256`), and its hub/spoke
troubleshooting section repeats the same pointer (`:322`). The
architecture doc's LD-19 prose is aspirational about uncoordinated mode;
the shipped behavior is hub-only. This matters directly for this section:
**as of today, dropping hub-role support drops the only implemented
consumer of `known_inter_domain_links`**, not just one of two.

### 4.2 Correction: the "KG write API pusher" alternative does hold up

**This section originally concluded the opposite of what's below — kept
here, corrected, rather than silently rewritten, so the reasoning error is
visible.** The original argument was: no reference to a Knowledge Graph
write API could be found in the KG spec or its companion YAML, so even
setting that aside, such a tool would be pushing into schema territory
(a `CONNECTED_TO` relation type, a per-edge confidence property) the KG
spec had found doesn't exist in Asserts' ruleset — so the alternative was
speculative work, not recommended.

The premise was checking the wrong thing. "The KG spec didn't find a write
API" is a fact about that document's research, not about
`grafana/asserts-adi`. An adversarial review
(`docs/proposals/reviews/adversarial-review.md`, Finding 1, "[VERIFIED]")
read `grafana/asserts-adi` directly — specifically
`inference-engine/api-server/` and the top-level `docs/`, neither of which
either this document's or the KG spec's author had reached — and found a
real, currently-maintained Knowledge Graph **Write API**
(`KgWriteController`, documented in full at `docs/kg-write-api.md` in that
repo). It supports arbitrary relation types (`CONNECTED_TO` or anything
else, no PR or schema merge required — `allowUndeclared` defaults to `true`)
and a genuine per-instance `properties` object on every entity and
relationship write, which is exactly the per-edge confidence mechanism this
section said didn't exist. Both of this section's stated blockers were
factually wrong, not just optimistic.

`knowledge-graph-boundary-stitching.md` has been rewritten around this API
(its §3-§6). §4.4 below now recommends the push job that document describes
as the primary mechanism for `known_inter_domain_links`, not the §4.3
local-injection design this section originally fell back to once the write
API was believed unavailable.

### 4.3 A smaller, code-grounded alternative — kept for the record, no longer the recommendation

**Superseded by §4.4 below — kept here because it's still a legitimate
design and because the security review's critique of it (next paragraph)
is worth preserving, not because it's still recommended.** This was
originally proposed as the fallback once the KG write API was believed not
to exist (§4.2's original text). Now that it does, §4.4 recommends the
Write-API push job instead, for reasons including a real weakness in this
alternative that the security review caught: `security-review.md` Finding 3
found that having each side of a boundary independently inject its own
rank-0 override from its own local config, as described below, removes the
one cross-check today's hub still has (both tuples of a link visible in one
file an operator or reviewer can eyeball for a mismatch) — "a single
instance could unilaterally assert a `ConfidenceHigh`, rank-0... edge to an
arbitrary named device, and nothing downstream currently has any way to
tell that assertion apart from a genuinely mutual one." That's a real
regression relative to today's hub, not just a hypothetical one — see §4.4.

`hub_merge.go`'s injection of `known_inter_domain_links` is not
complicated: for each configured link, it calls `appendEdgePair` with
`discovery.ConfidenceHigh` and `PrecedenceRank: 0` (`hub_merge.go:209-214`)
— the same helper used for auto-matched Tier-1 edges, just with the highest
rank so it always wins `graph.Reconcile`. Nothing about that mechanism
requires a central hub. Both endpoints of a boundary link are named
explicitly in the config tuple (`local_device`/`local_port`/`remote_device`/
`remote_port`) — each of the two instances on either side of the boundary
already has, in its own config, everything it needs to synthesize *its own
half* of the override edge locally, the same way it already emits
`network_topology_boundary_observation_info` for auto-discovered
neighbours. Concretely: an uncoordinated (or standalone) instance whose
`known_inter_domain_links` names it as `local_device` or `remote_device`
could emit a `network_topology_edge_info`-shaped series for that link
directly from its own `/metrics`, with `direction=bidirectional`,
`discovery_proto="configured"` (the existing
`discovery.DiscoveryProtocolConfigured` constant, already used by
`hub_merge.go:212`), and rank 0 — no new component, no mTLS, no spoke
registry, no HA. Both sides of the boundary would declare the same tuple
redundantly (symmetric config, same as today's hub config), and each side's
own reconcile pass would just... reconcile it, the way rank-0 edges already
win today.

### 4.4 Recommendation (revised): push overrides through the KG Write API, not local injection

**Recommendation: use the KG-write-API push job described in
`knowledge-graph-boundary-stitching.md` §6 as the default answer for
`known_inter_domain_links` in Grafana-Cloud-native / no-hub deployments,
not the §4.3 local-injection design.** Keep `cmd/topology-hub` available for
operators who need the hub's other properties (centralized size-budget
enforcement across a whole domain set, HA, a single `/spoke/push` audit
point, Tier-1 auto-matching that spans instances neither side individually
knows about) — that part of this document's recommendation is unchanged.

Why the push job, not §4.3's local injection, now that both are genuinely
available:

- **It doesn't have §4.3's decentralization problem.** §4.3 has each side
  of a boundary independently injecting its own rank-0 override from its
  own local config — the exact thing security review Finding 3 flagged as
  removing today's hub's one cross-check (both tuples visible in one file).
  The push job is designed to run as **one job reading one
  `known_inter_domain_links` config** — the KG spec's §6 makes this an
  explicit operational requirement, not just a preference — so the
  centralized-review property of today's hub survives even without the hub
  running.
- **It gets a self-expiry property neither §4.3 nor today's hub has.** The
  Write API requires a `ttlSeconds` on every write
  (`EntityWriteRequestDto.java:47-57`, cited in the KG spec §3.2). If the
  push job re-reads the config every cycle and only refreshes the TTL for
  entries still present, a stale entry (device decommissioned, topology
  changed) ages out on its own instead of asserting a wrong link at rank 0
  indefinitely until a human notices and edits the config — the exact
  failure mode adversarial Finding 9 and security Finding 3 both raised
  against §4.3 (and, for that matter, against today's hub's own
  never-expiring injection).
- **It gets the same relation type and graph location as auto-matched
  Tier-1 edges**, closing this document's own Finding 2 (§4.3's local
  injection wrote to `network_topology_edge_info`, a metric the old KG rule
  never read — a silent gap between the override path and the graph the
  whole series exists to populate). The push job writes both cases as
  `CONNECTED_TO` between the same two `NetworkDevice` entities, distinguished
  only by the `properties.confidence`/`precedence_rank`/`source` values —
  there is no second metric family for a KG-side consumer to forget to
  wire up.
- **It requires no new component inside this exporter's binaries** — same
  footprint argument §4.3 made originally (no inbound listener, no
  client-certificate lifecycle, no push protocol), except the small new
  component is the push job itself (a new small standalone service/cron
  described in the KG spec §4.3, not a code change to `cmd/topology-exporter`
  or `cmd/topology-hub`).

What doesn't change from the original recommendation: the operator still
has to know both endpoints' identities and maintain the list — that burden
is inherent to the problem (LD-19's own text: "operators configure these
once for boundary ports with naming inconsistencies") and doesn't shrink
under any alternative considered here, including today's full hub. What
shrinks, and what changes, is which mechanism is trusted to turn that list
into a KG-visible edge, and whether that mechanism is at least as
trustworthy as today's hub — the push job's single-shared-config
requirement plus TTL-based expiry is the honest argument that it is, in a
way §4.3's design specifically was not.

## 5. Decision: sever spoke-push transport from hub matching logic

### 5.1 What `spoke.go` actually is

267 lines, pure transport: marshal `SpokePayload`, optionally gzip, POST
with mTLS + exponential-backoff retry (`spoke.go:131-206`), nothing else.
It contains **no matching logic** — `canonicalizeDeviceName`,
`buildCombinedGraph`, and the OOS reverse-key index all live in
`hub_merge.go`, on the receiving side. Confirmed by `spoke.go`'s import list
(`spoke.go:1-32`): no `internal/graph`, no reference to any hub type.

### 5.2 Under Grafana-Cloud-native, does spoke-push become unnecessary?

Yes, and for a reason independent of whether KG's Tier-1 rule is any good.
Spoke-push's entire job is: get this domain's graph to one place (the hub)
so a second `graph.Reconcile` pass can see both sides. In a
Grafana-Cloud-native deployment, every instance is already scraped by (or
already OTLP-pushes to) the same Mimir tenant — that's what
"Grafana-Cloud-native" means here, and it's the same premise the KG spec
itself relies on (§2: "KG's METRICS matcher can query all instances' series
at once... which is materially simpler than hub_merge.go's approach of
snapshotting per-spoke payloads into memory"). The centralization spoke-push
exists to provide is **already accomplished by remote-write**, with zero
custom transport. This holds whether the join on top of that
already-centralized data is done by a Mimir recording rule (uncoordinated
mode's existing design, LD-15), a KG Tier-1 rule, or, per §4.3, per-instance
local injection for the manual-override case. None of those three consumers
need `spoke.go`'s mTLS client or retry logic — they read from Mimir, which
already has the data. **Spoke-push is specifically a hub/spoke-mode
artifact; it has no purpose independent of a hub existing somewhere to
receive it.** Once a deployment has no hub (because it's GC-native and
using KG or a recording rule instead), `spoke.go` isn't a smaller version of
anything useful — it's simply unused. This is a stronger and cleaner
statement than "KG matching quality determines whether spoke-push survives":
the transport's fate tracks *whether a hub exists*, not *whether KG's
matching is trusted*. An operator who doesn't trust KG's Tier-1 rule at all
and wants hub_merge.go's exact guarantees still needs the hub *and*
spoke-push together (they're not independently useful in that world
either) — they just don't get to drop them.

### 5.3 Why `spoke.go` stays in core anyway (§3.2 restated)

Because it's small (267 lines), has no expensive dependencies (§3.2:
stdlib TLS only), and some real deployments will keep running hub/spoke
mode for the properties in §4.4 that neither uncoordinated mode nor KG
currently replicate (cross-instance auto-matching that spans domains
neither instance individually knows about, centralized size-budget
enforcement, HA). Moving it into `internal/federationhub` alongside the
matching logic would force every `role: spoke` deployment to also carry
the hub's Kubernetes dependency even though a spoke never runs an elector —
that would recreate exactly the §2.3 problem this proposal is trying to fix,
just shifted one hop over.

## 6. Phased rollout

The four things this document and the two prior ones (label alignment, KG)
touch have different amounts of risk and different dependencies on each
other. None of them depend on the KG rule being proven in production first
— that's a common misconception this section corrects explicitly.

1. **Ship now, independent of everything else: the neighbour-hint
   `NormaliseName` fix** (§1, KG spec §5's ~5 call sites — renumbered from
   §4.5 when that document was rewritten around the KG Write API; the fix
   itself is unchanged). Small, purely additive to matching correctness,
   benefits the existing Mimir recording rule, `hub_merge.go`'s own
   matching, and (per that section's own "what's new" note) is no longer
   even load-bearing for the KG push job specifically, since that job does
   its own case-folding in Go — it's still upstream groundwork worth
   shipping for the other two consumers. No dependency on this document's
   package split.
2. **Ship now, independent of everything else: the core/hub binary split**
   (§3). Pure repackaging, no *behavior* change (see §3.3's closing note),
   removes the measured `k8s.io/client-go` cost from every non-hub build
   immediately. Does not require the KG push job, the label-alignment
   additions, or any operator decision about which stitching mechanism to
   prefer — it's orthogonal to all of that. This is the one item in this
   document that can land first with the least review risk, because §3.3's
   before/after table has no *behavior* column that changes for any
   existing deployment shape — the sole operator-visible change is
   deployment mechanics (§3.3's last row: hub operators swap which
   image/binary they run), not a change in what the hub does.
3. **Ship the label-alignment additions** (`src_if_index`/`dst_if_index`,
   `device_id`/`device` OTLP rename, `sys_name`, `management_ip`) on the
   schedule that document already lays out (its own §7) — unrelated to
   federation, sequenced independently.
4. **Pilot the KG boundary-stitching push job** (KG spec §4, as sketched in
   `kg-boundary-pusher-sketch.md`) against a real Grafana-Cloud-native,
   no-hub deployment, comparing its confirmed-edge output against what a
   hub-mode deployment of the *same* topology would produce via
   `hub_merge.go`'s matching logic — the push job is designed to be the same
   algorithm (KG spec §4.2), so this is a validation that the port is
   faithful, not a comparison of two different designs. This is a
   validation step, not a blocker for steps 1-3.
5. **Only after step 4 shows real parity**, update operator-facing guidance
   (`docs/operator/federation.md`) to recommend the KG push job (KG spec §6,
   covering both Tier-1 auto-matching and `known_inter_domain_links`
   overrides through the same mechanism — see this document's own §4.4) as
   the default for new GC-native deployments, with `cmd/topology-hub`
   positioned as the path for on-prem/self-hosted Mimir users and for
   cross-domain auto-matching that spans instances no single side is aware
   of. This is a documentation/recommendation change, not a code
   deprecation — nothing in this document proposes removing hub support.
6. **Build and deploy the push job** (standalone service/cron, per KG spec
   §4.3) once step 4 validates it. Unlike the original §4.3 local-injection
   design this step used to name, the push job is recommended outright per
   this document's revised §4.4, not gated on "an operator asks for it" —
   it has no decentralization downside to wait out (§4.4's cross-check
   argument), so there's no reason to delay it behind adoption signal the
   way the superseded local-injection design was.

Steps 1-3 have no ordering dependency on each other or on 4-6. Step 4's
dependency on step 1 is weaker than it used to be: the push job does its
own case-folding in Go (KG spec §4.2/§5's "what's new" note), so step 4
doesn't strictly need step 1 to land first to be correct — it still helps
make the hub-mode comparison in step 4 cleaner if hub_merge.go and the push
job are working from a fleet with consistent naming, so shipping step 1
first is still the better order, just no longer a hard prerequisite. Steps
5-6 depend on step 4's outcome, not on steps 1-3 shipping first, though in
practice 1-3 will likely land first because they're lower-risk and don't
require a production pilot.

## 7. What doesn't shrink

Discovery walkers and graph reconciliation are, and remain, the bulk of
this codebase, and this proposal does nothing to them. Measured
(`wc -l`, non-test):

| Package | Non-test LOC |
|---|---|
| `internal/discovery` (all walkers + `snmp`/`arp` support) | 5,280 |
| `internal/app` (loop/cycle/device_walk/probe/rediscover, minus ~200 federation-coupled lines) | ~2,990 |
| `internal/config` | 1,274 |
| `internal/metrics` | 1,046 |
| `internal/output` (otlp + yang) | 720 |
| `internal/graph` | 575 |
| `internal/snapshot` | 365 |
| `internal/credentials` | 263 |
| `internal/events` | 82 |
| **Core subtotal** | **~12,595** |
| `internal/federation` (moves: hub-only, §3.2) | 1,618 |
| `internal/federation` (stays: spoke+payload, §3.2) | 298 |

Total non-test Go LOC in the repo: 15,932 (measured). The hub-only slice
this proposal actually carves out is **1,618 lines — about 10% of non-test
source**, plus roughly 190 lines of `app.go`'s hub branch that moves with
it, plus the hub-specific test suites (3,667 lines) that move with their
production code but were never part of the shipped binary's size to begin
with. Including tests, the total repo is 48,302 lines; the federation
package as a whole (hub + spoke + shared, src + test) is 6,178 lines, 12.8%
of that total — but that figure overstates what this proposal removes,
because §3.2 keeps the spoke side (298 src / ~600 test lines) in core. The
honest number for "what moves to the optional binary" is closer to **5,285
lines including tests, ~11% of the repo's total Go LOC, and roughly 10% of
non-test source** — a real but modest reduction in lines. The reduction
that actually matters operationally is the one in §2.3: the entire
`k8s.io/client-go` dependency graph (18,322 linked symbols, measured)
leaves every build except `cmd/topology-hub`. **Anyone expecting this
split to make the exporter "smaller" in a way a casual `wc -l` diff would
capture should recalibrate: the win is dependency-surface isolation for a
build most deployments never needed, not a smaller core codebase.** The
discovery/reconciliation core — the actual point of this exporter — is
untouched and remains roughly 79% of non-test source on its own.

## 8. Non-goals

- Not proposing to remove hub/spoke federation support. `cmd/topology-hub`
  ships every capability `role: hub` has today, verbatim.
- Not proposing changes to `hub_merge.go`'s matching logic, `hub_push.go`'s
  reject contract, or any other hub behavior — §3's split is a file-move
  plus package-rename, explicitly "no behavior change" per §3.2.
- Not proposing the §4.3 local-injection feature be built at all — §4.4
  (revised) recommends the KG-write-API push job instead; §4.3 is kept only
  as a documented, superseded alternative (see §4.3's own note on why).
  The push job itself is recommended per §6 step 6, gated only on step 4's
  validation, not on operator demand — unlike §4.3, it has no
  decentralization downside that would justify waiting for adoption
  signal first.
- Not resolving the KG spec's own open questions (bidirectional-traversal
  semantics, the `asserts_env`/`asserts_site` read-side grouping
  assumption, gateway-level rate limiting) — this document treats the KG
  push job's eventual production status as an input to §6's phasing, not
  something it re-litigates. (The `when:`-gate and Tier-2-matching
  questions the original KG spec raised no longer apply — the rewritten KG
  spec doesn't author a Yoda rule, so neither concept is in play.)
- Not proposing any change to `docs/architecture.md`'s LD-15-LD-20 numbering
  or content beyond flagging the §4.1 documentation/implementation mismatch
  for whoever owns that doc to correct.

## 9. Reviewer checklist (five review lenses this will get)

- **Security**: §2.3's `k8s.io/client-go` finding is the security-relevant
  one — every non-hub deployment today ships a Kubernetes API client
  (RBAC/Lease semantics, service-account token handling) it never uses.
  §3's split removes that from the attack surface of every binary except
  the one that actually needs it. Separately, confirm §4.3's proposed local
  override-injection doesn't reopen LD-20's mTLS-authenticated-push
  guarantee by a different door — it doesn't push anything cross-instance,
  it only emits a locally-configured, locally-signed-by-nothing metric from
  the instance's own `/metrics`, same trust model as every other metric
  this exporter already emits about itself.
- **Network-ops**: §6's phasing is written so a hub/spoke deployment in
  production today is unaffected by any of steps 1-3 shipping, and is only
  asked to change anything (which binary they run) if they explicitly
  choose to adopt the split in §3 — confirm the migration note in §3.3
  ("hub operators run a different container image/binary") is operationally
  acceptable, since it is a real deployment-shape change even though it's
  not a behavior change.
- **Grafana-ecosystem-fit**: §5's argument that spoke-push becomes
  unnecessary under Grafana-Cloud-native deployment is the load-bearing
  ecosystem claim in this document — it assumes every instance already
  lands in one Mimir tenant, the same assumption the KG spec makes (§2) and
  flags as unverified against real deployments (KG spec's own network-ops
  checklist item). Worth checking against actual Grafana Cloud customer
  topology-exporter deployments before treating §5.2 as settled.
- **Software-architecture**: §3.1's Go `internal/` visibility constraint is
  the reason this isn't a "separate module" in the Go-modules sense — worth
  independently confirming that constraint rather than taking it on faith,
  since it's the reason the recommended split is two `cmd/` binaries in one
  module rather than two independently-versioned modules.
- **Adversarial**: this bullet originally called §4's rejection of "a KG
  write-API pusher" the sharpest finding in the document, and predicted it
  was "the one most likely to be second-guessed." That prediction was
  correct, for the reason stated at §4.2 above: the rejection rested on a
  premise (no write API exists) that an actual adversarial review found to
  be false. §4.4 now recommends that exact alternative. Worth an
  adversarial pass, going forward, specifically on §4.3's local-injection
  alternative: does emitting a rank-0 override edge from each side's own
  `/metrics` independently (rather than from one central, mTLS-authenticated
  hub) create any new way for a compromised or misconfigured instance to
  assert a false high-confidence link about a device it doesn't otherwise
  observe? (Today's hub at least requires the operator's own config on the
  hub side; §4.3 would require the operator's own config on *each*
  instance, which is a smaller trust boundary in one sense — no central
  point of compromise — but means the assertion happens locally with no
  cross-check against the other side's config at all, unlike the hub, which
  at least has both tuples in one place to notice a mismatch.) This is
  flagged, not resolved, here.
