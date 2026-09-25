# Kustomize manifests

Plain-YAML Kubernetes manifests for `network-topology-exporter`, for shops that
deploy with `kubectl apply -k`, Argo CD, or Flux rather than Helm.

These manifests **mirror the rendered output of the Helm chart** in
[`deploy/helm/topology-exporter`](../helm/topology-exporter). The two install
paths must stay in sync — see [Relationship to the Helm chart](#relationship-to-the-helm-chart).

## Layout

```
deploy/kustomize/
  base/                       # all shared resources (do not apply directly)
    serviceaccount.yaml
    configmap.yaml            # sample config (federation.role: standalone)
    secret.yaml               # PLACEHOLDER values only — replace before applying
    service.yaml
    networkpolicy.yaml
    deployment.yaml
    kustomization.yaml
  overlays/
    standalone/               # config.federation.role: standalone
    hub/                      # config.federation.role: hub
    spoke/                    # config.federation.role: spoke
  components/
    egress-networkpolicy/     # opt-in: lock down egress (DNS/SNMP/OTLP/hub)
    ha/                       # opt-in: native hub high-availability (#71)
```

## Hardening: opt-in egress NetworkPolicy

The base NetworkPolicy is **Ingress-only** — a compromised credential-bearing
SNMP scanner would have unrestricted outbound (lateral) reach. The
`components/egress-networkpolicy` component adds `Egress` to `policyTypes` with
allow-rules for DNS, SNMP polling, the OTLP collector, and hub federation.
Enable it from an overlay:

```yaml
# in overlays/<mode>/kustomization.yaml
components:
  - ../../components/egress-networkpolicy
```

With Egress enabled, **only the listed destinations are reachable**. The
component **fails safe (deny-until-edit)**: the SNMP/OTLP/hub example CIDRs ship
as `0.0.0.0/32` placeholders — a non-routable single address that matches no
real traffic — so applying this component **unedited DENIES all SNMP/OTLP/hub
egress** until you replace each `0.0.0.0/32` with your real CIDRs (and adjust
ports). The DNS rule is scoped to the `kube-system` namespace; change it if your
cluster DNS lives elsewhere. (Helm users set `networkPolicy.egress.*`.)

## HA (#71): opt-in native hub high-availability

By default the hub runs as a single replica. The `components/ha` component
turns it into a leader-elected HA hub — mirroring the Helm chart's
`federation.hub.ha.enabled=true` path. **It is opt-in and only valid for the
`hub` overlay.** Enable it by adding the component to the hub overlay's
`components:` list:

```yaml
# in overlays/hub/kustomization.yaml
components:
  - ../../components/ha
```

What the component adds/patches:

- **`Role` + `RoleBinding`** granting the `topology-exporter` ServiceAccount
  `get,create,update` on `coordination.k8s.io/leases` — the minimal verbs
  client-go leader election needs for the Lease that elects one leader.
- **A headless metrics `Service`** (`topology-exporter-metrics`, `clusterIP:
  None`, `publishNotReadyAddresses: true`, port 9100, all-pods selector). The
  primary push Service is readiness-gated to the leader only; that gate would
  otherwise hide follower `/metrics`, so Prometheus scrapes every replica via
  this Service.
- **Deployment env** `POD_NAME`/`POD_NAMESPACE` (downward API) — the election
  identity and Lease namespace.
- **`automountServiceAccountToken: true`** on the ServiceAccount (the base sets
  it `false` because the non-HA exporter never calls the API server).
- **Config:** re-writes `config.yaml` with `federation.hub.ha.enabled: true`
  plus the lease settings (`lease_name`, `lease_duration: 15s`,
  `renew_deadline: 10s`, `retry_period: 2s`). Because the component replaces the
  whole hub config, **keep `components/ha/configmap.yaml` in sync with
  `overlays/hub/configmap.yaml`** if the hub config changes.

### Caveats (mirror the Helm chart NOTES)

- **Set `replicaCount >= 2`.** The component does **not** bump replicas — HA
  needs at least 2 for failover; a single replica is degraded (no standby). Add
  a `replicas:` patch on the Deployment in your overlay.
- **Shared snapshot is opt-in and needs ReadWriteMany.** The default HA path
  uses **no** shared volume — a new leader cold-starts and rebuilds the graph
  from the spokes' next push (within one `discovery.interval`); zero data loss.
  To warm-start from the previous leader's snapshot, set
  `federation.hub.snapshot.shared: true` in the config **and** provide a
  `ReadWriteMany` (NFS/CephFS/EFS/Filestore) volume. **Most default
  StorageClasses are RWO** and will fail to attach to multiple replicas — do not
  enable `snapshot.shared` on an RWO volume.
- **RBAC is required** and is provided by this component (`Role`/`RoleBinding`);
  do not enable HA config without it or election will fail.

## Federation modes

Each overlay selects one federation mode via `config.federation.role`
(see `internal/config` — valid roles: `standalone`, `uncoordinated`, `spoke`,
`hub`). Apply exactly one overlay; never apply `base/` on its own.

| Overlay      | `federation.role` | What it does                                                                 |
|--------------|-------------------|------------------------------------------------------------------------------|
| `standalone` | `standalone`      | Single-instance discovery, no federation. The default.                       |
| `hub`        | `hub`             | Pure aggregator. Receives spoke pushes on a separate mTLS listener (`:9101`). |
| `spoke`      | `spoke`           | Local discovery + pushes its domain graph to a hub over mTLS.                |

## Apply

```sh
# Standalone (default single-instance mode)
kubectl apply -k deploy/kustomize/overlays/standalone

# Hub (aggregator)
kubectl apply -k deploy/kustomize/overlays/hub

# Spoke (pushes to a hub)
kubectl apply -k deploy/kustomize/overlays/spoke
```

Render without applying to inspect the result:

```sh
kubectl kustomize deploy/kustomize/overlays/standalone
```

## Customising config and secrets

- **Exporter config** lives in the `topology-exporter` ConfigMap
  (`config.yaml`). Edit `base/configmap.yaml` for cross-mode defaults, or the
  per-mode `overlays/<mode>/configmap.yaml` for that mode. The discovery scope,
  modules, credentials, and `targets` are environment-specific and intended to
  be edited.
- **Secrets** — `base/secret.yaml` ships with **placeholder values only**
  (`REPLACE_ME`). Never commit real secrets. Either replace the placeholders
  before applying, or (preferred) manage the Secret out-of-band
  (Sealed Secrets, External Secrets, SOPS, Vault) and drop `secret.yaml` from
  `base/kustomization.yaml`. Consume secret values in the Deployment via
  `env`/`valueFrom.secretKeyRef`.
- **Image / tag** — the `hub` overlay already points at a different image
  than `standalone`/`spoke`: `network-topology-hub` (built from
  `cmd/topology-hub` / `Dockerfile.hub`), not `network-topology-exporter`.
  Only the hub image links `k8s.io/client-go`, needed for the opt-in
  native-HA leader election (see `overlays/hub/kustomization.yaml`'s
  `images:` override). Pin the tag per environment:

  ```sh
  # standalone / spoke
  cd deploy/kustomize/overlays/standalone
  kustomize edit set image \
    ghcr.io/grafana/network-topology-exporter=ghcr.io/grafana/network-topology-exporter:1.0.0

  # hub
  cd deploy/kustomize/overlays/hub
  kustomize edit set image \
    ghcr.io/grafana/network-topology-exporter=ghcr.io/grafana/network-topology-hub:1.0.0
  ```

  (or edit `images:` in `base/kustomization.yaml` for the base tag, and
  `overlays/hub/kustomization.yaml` for the hub image/tag).

### Federation mTLS (hub and spoke)

The hub and spoke overlays reference TLS material at
`/etc/topology-exporter/pki/{ca.crt,tls.crt,tls.key}`. Provide it by creating a
TLS Secret and mounting it into the Deployment (add a `volumes`/`volumeMounts`
patch in the overlay):

```sh
kubectl create secret generic topology-exporter-pki \
  --from-file=ca.crt --from-file=tls.crt --from-file=tls.key
```

For the spoke, also set a unique `federation.spoke.spoke_id` and the correct
`hub_url` (the hub's Service DNS name on port `9101`).

## Relationship to the Helm chart

The Helm chart (`deploy/helm/topology-exporter`) is the source of truth for the
resource shape: names, labels, ports, securityContext, probes, and RBAC. These
Kustomize manifests reproduce the chart's **default** rendered output so the two
paths do not drift.

**When you change the chart, update these manifests to match.** Regenerate the
reference output and diff it:

```sh
helm template te deploy/helm/topology-exporter
```

The `kustomize-smoke` CI workflow only validates that the overlays *render*; it
does not detect drift from the chart, so keeping them in sync is a manual step
in any PR that touches the chart's rendered shape.
