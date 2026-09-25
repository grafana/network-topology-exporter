# Getting started

A copy-pasteable, step-by-step walkthrough for a brand-new user. It covers
**both** operating modes:

- **Standalone** — one exporter discovers your network and exposes `/metrics`.
- **Hub-spoke** — several spoke exporters discover their local domains and push
  reconciled graphs to a central hub over mTLS; the hub aggregates everything.

Follow the numbered steps in order. Every command and config block can be
pasted as-is and then adapted to your environment.

> The authoritative, fully-commented config schema is
> [`config/example.yaml`](config/example.yaml). The examples below are the
> *minimal* valid configs — start here, then grow them using the example file.

---

## 1. Prerequisites

You need three things before the exporter can discover anything:

1. **A device that answers SNMP** — a switch, router, or anything that
   responds to SNMP and exposes an edge-discovery MIB (LLDP is the easiest:
   IEEE 802.1AB / `lldpRemTable`).
2. **The SNMP credentials for that device:**
   - SNMP v2c: the **community string** (e.g. `public`).
   - SNMP v3: the **username**, and (for authPriv) the auth and privacy keys.
3. **The management CIDR** your devices live in (e.g. `198.51.100.0/24`). The
   exporter only polls addresses inside `discovery.scope.cidr_allow_list`, and
   that list is **required** whenever you configure targets.

> **Credentials are never written in YAML.** SNMP secrets are read from
> environment variables. In the config you only name the variable
> (`community_env`, `username_env`, `priv_key_env`, …); the exporter reads the
> actual secret from that env var at runtime.

The binary has four flags:

| Flag | Default | Meaning |
|---|---|---|
| `--config.file` | `/etc/topology-exporter/config.yaml` | Path to the YAML config |
| `--web.listen-address` | `:9100` | Address for `/metrics`, `/readyz`, `/healthz` |
| `--log.level` | `info` | `debug \| info \| warn \| error` |
| `--version` | — | Print the version and exit |

Endpoints exposed on the listen address:

- `:9100/metrics` — Prometheus metrics (the topology graph).
- `:9100/readyz` — readiness: `503` during startup, `200` after the first
  discovery cycle completes.
- `:9100/healthz` — liveness: returns `last_cycle_at` and an aggregate
  `device_errors` count.
- `:9100/topology/yang` — the reconciled topology as an RFC 8345/8346
  YANG-JSON document. Only served when `output.yang.enabled: true`
  (see [`docs/operator/yang-topology.md`](docs/operator/yang-topology.md)).
- `:9100/admin/rediscover` — `POST` forces an out-of-cycle re-discovery of a
  target. Privileged: returns `403` unless `listen.web_config_file`
  authenticates the caller (basic auth or client certs).

(In hub-spoke mode the hub additionally listens on `:9101` for spoke pushes.)

---

## 2. Standalone mode

### 2.1 Write the smallest valid config

Save this as `config.yaml`. It enables SNMP transport (v2c) plus the LLDP edge
module, scopes discovery to one CIDR, and pulls the community string from the
`SNMP_COMMUNITY` env var. `federation.role` defaults to `standalone`, so the
federation block is omitted.

```yaml
discovery:
  interval: 60s                  # full discovery cycle every 60s (default)
  scope:
    cidr_allow_list:
      - "198.51.100.0/24"        # REQUIRED — only these IPs are polled
modules:
  snmp:
    enabled: true
    version: v2c
    community_env: SNMP_COMMUNITY   # community read from $SNMP_COMMUNITY, never inline
  lldp:
    enabled: true                   # at least one edge module
```

> You also need at least one target to poll. Add a `targets:` list (see
> `config/example.yaml`), e.g.:
>
> ```yaml
> targets:
>   - host: 198.51.100.10
> ```
>
> Every target host must fall inside `cidr_allow_list`.

Now pick **one** of the three run paths below.

### 2.2a Run path — binary

```bash
# 1. Build (or download a release binary)
go build -o bin/topology-exporter ./cmd/topology-exporter

# 2. Export the SNMP community (never put it in YAML)
export SNMP_COMMUNITY=public

# 3. Run, pointing at your config
./bin/topology-exporter --config.file=./config.yaml --log.level=info
```

The exporter now listens on `:9100`. Skip to **2.3 Verify**.

### 2.2b Run path — Docker

```bash
# 1. Run the published image, mounting your config and passing the community env
docker run --rm -p 9100:9100 \
  -e SNMP_COMMUNITY=public \
  -v "$PWD/config.yaml:/etc/topology-exporter/config.yaml:ro" \
  ghcr.io/grafana/network-topology-exporter:latest
```

The container reads `/etc/topology-exporter/config.yaml` by default (the
flag default), so no extra args are needed. Then go to **2.3 Verify**.

### 2.2c Run path — Kubernetes (Kustomize)

The repo ships a Kustomize base plus a `standalone` overlay.

```bash
# 1. Apply the standalone overlay
kubectl apply -k deploy/kustomize/overlays/standalone
```

The base bundles a Deployment, Service, a ConfigMap (the config), and a Secret
(the community string). To use your own values:

- **Config goes in the ConfigMap** named `topology-exporter`, under the
  `config.yaml` key. Edit `deploy/kustomize/base/configmap.yaml` (or override
  it in the overlay) so its `config.yaml:` block matches section 2.1.
- **The community string goes in the Secret** named `topology-exporter`. Set
  the `SNMP_COMMUNITY` key (the deployment injects it as the `$SNMP_COMMUNITY`
  env var):

  ```bash
  kubectl create secret generic topology-exporter \
    --from-literal=SNMP_COMMUNITY=public \
    --dry-run=client -o yaml | kubectl apply -f -
  ```

The Deployment runs the binary with
`--config.file=/etc/topology-exporter/config.yaml` and mounts the ConfigMap
there.

> **Helm alternative.** A chart lives at `deploy/helm/topology-exporter`. Set
> the config and SNMP community via `values.yaml`, then
> `helm install topo deploy/helm/topology-exporter -f my-values.yaml`.

Then go to **2.3 Verify**.

### 2.3 Verify

```bash
# 1. Wait for readiness — 503 until the first cycle finishes, then 200
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:9100/readyz

# 2. Once /readyz is 200, confirm devices were discovered
curl -s http://localhost:9100/metrics | grep network_topology_discovery_devices_total

# 3. Confirm topology edges were found (one series per discovered link)
curl -s http://localhost:9100/metrics | grep network_topology_edge_info
```

You should see `network_topology_discovery_devices_total{status="success",reason="n/a"}`
greater than zero and at least one `network_topology_edge_info{...}` series.
If `device_errors` on `/healthz` is non-zero or no edges appear, see
[`docs/operator/troubleshooting.md`](docs/operator/troubleshooting.md).

---

## 3. Hub-spoke mode

**The model in two sentences:** each spoke runs full discovery on its own
network domain, reconciles its local graph, and pushes that graph to a central
hub over mutual TLS after every cycle. The hub does no SNMP discovery itself —
it aggregates and re-reconciles all spoke graphs and exposes the unified
topology as Prometheus metrics on `:9101`.

`federation.role` is one of `standalone | uncoordinated | spoke | hub`.

**Two separate binaries/images.** `standalone`, `uncoordinated`, and `spoke`
all run `cmd/topology-exporter` (`ghcr.io/grafana/network-topology-exporter`),
same as section 2 above. `hub` runs a **different** binary/image,
`cmd/topology-hub` (`ghcr.io/grafana/network-topology-hub`) — a separate
build so that only the hub links `k8s.io/client-go`, needed for the opt-in
native-HA leader election (§ Federation in [README.md](README.md)). Every
CLI flag, config key, and endpoint below is otherwise identical between the
two binaries.

### 3.1 Set up the mTLS certificates

Federation is mutual TLS using **PEM file paths** (this is *not* the
exporter-toolkit `web_config_file`). Both the hub and each spoke need three
files, placed at `/etc/topology-exporter/tls/`:

- `ca.pem` — the CA that signed both ends (shared).
- on the hub: `hub.pem` (server cert) + `hub-key.pem` (server key).
- on each spoke: `spoke.pem` (client cert) + `spoke-key.pem` (client key).

The hub verifies each spoke's client certificate against `ca.pem`; the spoke
verifies the hub's server certificate against `ca.pem`. Generate these with
your own PKI / `cfssl` / `openssl`; the exporter only consumes the PEM files.

### 3.2 Hub config

Save as `hub.yaml`. It is a standalone config plus a `federation` block in
`hub` role. (The hub still parses `discovery`/`modules`, but does no polling.)

```yaml
discovery:
  interval: 60s
  scope:
    cidr_allow_list:
      - "198.51.100.0/24"
modules:
  snmp:
    enabled: true
    version: v2c
    community_env: SNMP_COMMUNITY
  lldp:
    enabled: true
federation:
  role: hub
  hub:
    listen_addr: ":9101"
    tls_ca_cert: /etc/topology-exporter/tls/ca.pem
    tls_cert: /etc/topology-exporter/tls/hub.pem
    tls_key: /etc/topology-exporter/tls/hub-key.pem
```

### 3.3 Spoke config

Save as `spoke.yaml` on each spoke. Standalone config plus a `spoke`-role
`federation` block pointing at the hub's `/spoke/push` endpoint.

```yaml
discovery:
  interval: 60s
  scope:
    cidr_allow_list:
      - "198.51.100.0/24"
modules:
  snmp:
    enabled: true
    version: v2c
    community_env: SNMP_COMMUNITY
  lldp:
    enabled: true
federation:
  role: spoke
  spoke:
    spoke_id: dc1-spoke-a
    hub_url: https://hub.internal:9101   # base URL only — the exporter appends the /spoke/push path
    tls_ca_cert: /etc/topology-exporter/tls/ca.pem
    tls_cert: /etc/topology-exporter/tls/spoke.pem
    tls_key: /etc/topology-exporter/tls/spoke-key.pem
```

> **Timing constraint.** `federation.spoke_timeout` must be at least
> `2 × discovery.interval` or the hub will evict spokes spuriously. The
> default `spoke_timeout` is `3 × discovery.interval`, so leaving it unset is
> safe.

### 3.4 Run the hub, then the spokes

On the **hub** host (PEMs already at `/etc/topology-exporter/tls/`), build
and run `cmd/topology-hub` — **not** `cmd/topology-exporter`:

```bash
go build -o bin/topology-hub ./cmd/topology-hub
export SNMP_COMMUNITY=public
./bin/topology-hub --config.file=./hub.yaml
# hub /metrics on :9100; spoke push endpoint on :9101
```

(`cmd/topology-exporter` rejects `federation.role: hub` at startup with a
message pointing back here; `cmd/topology-hub` likewise rejects any role
other than `hub`.) Via Docker, use the `network-topology-hub` image instead
of `network-topology-exporter`:

```bash
docker build -f Dockerfile.hub -t network-topology-hub:dev .
docker run --rm -p 9100:9100 -p 9101:9101 \
  -v $PWD/hub.yaml:/etc/topology-exporter/config.yaml:ro \
  -v /etc/topology-exporter/tls:/etc/topology-exporter/tls:ro \
  network-topology-hub:dev
```

On each **spoke** host — this one *is* `cmd/topology-exporter`, same binary
as standalone mode:

```bash
export SNMP_COMMUNITY=public
./bin/topology-exporter --config.file=./spoke.yaml
```

In Kubernetes, use the `hub` and `spoke` Kustomize overlays
(`deploy/kustomize/overlays/hub`, `.../spoke`) — the `hub` overlay already
points at the `network-topology-hub` image, no edit needed; mount the PEM
files via a Secret and **expose `:9101` on the hub** (Service/ingress) so
spokes can reach `hub_url`. The Helm chart does the equivalent image
selection automatically based on `config.federation.role`.

### 3.5 Verify

On the **hub** — confirm each spoke is up and pushing:

```bash
# spoke_up = 1 per healthy spoke
curl -s http://hub.internal:9100/metrics | grep network_topology_federation_spoke_up

# last successful push timestamp per spoke (should advance every cycle)
curl -s http://hub.internal:9100/metrics | grep network_topology_federation_spoke_last_push_timestamp_seconds
```

On each **spoke** — confirm pushes are succeeding:

```bash
# should stay at 0
curl -s http://localhost:9100/metrics | grep network_topology_federation_spoke_push_failures_total
```

#### Spoke-push response codes

When a spoke POSTs to the hub's `/spoke/push`, the hub replies with:

| Code | Meaning | Spoke action |
|---|---|---|
| `204` | Success — payload accepted | Nothing; next cycle pushes again |
| `400` | Malformed payload | Fatal for this payload (a retry fails identically) |
| `403` | mTLS / identity failure (cert or `spoke_id` mismatch) | Fix certs / `spoke_id` |
| `409` | Stale generation (older than the hub already holds) | Next cycle's fresher data wins |
| `413` | Payload too large — over the 32 MiB body cap (wire or gzip-decompressed) or the hub's device/edge cap | Reduce graph size / raise hub caps |
| `415` | Unsupported `Content-Encoding` (hub accepts `gzip` or identity) | Set `federation.spoke.compression` to `gzip` or `none` |
| `429` | Rate-limited (`min_push_interval` not yet elapsed) | Back off; next cycle |
| `503` | Transient internal hub failure | Retry next cycle |

A rising `spoke_push_failures_total` on a spoke usually maps to a `403`
(certificate problem), `413` (too-large graph), or `503` (hub unhealthy).

---

## 4. Next steps

- [`README.md`](README.md) — project overview, emitted signals, module list.
- [`config/example.yaml`](config/example.yaml) — the **authoritative** config
  schema, every key documented inline.
- [`docs/operator/federation.md`](docs/operator/federation.md) — the full
  hub-spoke contract, eviction, and the on-wire push schema.
- [`docs/operator/troubleshooting.md`](docs/operator/troubleshooting.md) — what
  to check when discovery or federation isn't behaving.
