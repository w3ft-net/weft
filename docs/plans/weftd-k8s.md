# Plan: weftd on Kubernetes

A DaemonSet deployment of weftd that tails every container's stdout/stderr on a node, enriches rows with k8s metadata (pod, namespace, container, node, labels), and ships them through the existing weftd gRPC ingest. Helm chart is the install vector.

## Why add this

A material slice of customers run on Kubernetes. Today the manual answer for them is "install weftd on each node, configure path globs against `/var/log/containers/*.log`" — which works but is unidiomatic and gives them rows without pod/namespace metadata, which is exactly the cardinality they need to debug.

Every comparable agent (Datadog Agent, Fluent Bit, Vector, OTel Collector) ships a first-class k8s integration. We need one. The weftd code already does most of the work; what's missing is the metadata enrichment, the DaemonSet packaging, and the Helm chart.

## Architecture

```
              Kubernetes Node
              ┌─────────────────────────────────┐
              │  Pod containers                 │
              │   ↓ stdout/stderr               │
              │  Container runtime              │
              │   ↓                             │
              │  /var/log/pods/<ns>_<pod>_<uid>/<container>/N.log
              │   ↑                             │
              │  weftd DaemonSet pod            │
              │   • tails /var/log/pods/* via   │
              │     glob + fsnotify             │
              │   • parses pod identity from    │
              │     filename                    │
              │   • optionally augments with    │
              │     labels via API server       │
              │   • streams to coordinator      │
              └─────────────────────────────────┘
                    │ gRPC IngestStream
                    ▼
                Coordinator
```

The k8s log directory layout has been stable across container runtimes (Docker / containerd / cri-o all write here through the kubelet):

```
/var/log/pods/<namespace>_<pod-name>_<pod-uid>/<container-name>/N.log
```

Filename gives us k8s-namespace, pod, pod-uid, container — no API call required for those. Labels and annotations require an API call; we make that opt-in (no ClusterRole binding) so customers can run a label-less mode if they prefer minimal RBAC.

## What weftd needs

Most of weftd works as-is. The four k8s-derived dimensions (`cluster`, `k8s_namespace`, `pod`, `container`) are real columns defined by `docs/plans/ingest-dimensions.md`; this plan populates them. New pieces:

1. **k8s-aware glob source.** `cmd/weftd/main.go` already has `watchGlob`. A k8s mode sets `path: /var/log/pods/*/*/*.log`, parses the directory structure for identity, and stamps the `k8s_namespace` / `pod` / `container` columns on every row from the matched files.
2. **Identity enrichment from filename.** Stamps the four reserved k8s columns: `cluster` (from a static value baked into the chart or the kubelet downward API), `k8s_namespace` / `pod` / `container` (from the `/var/log/pods/<ns>_<pod>_<uid>/<container>/N.log` layout), plus `host` (from `KUBE_NODE_NAME` injected via the DaemonSet's downward-API config — populates the existing `instance` column).
3. **Optional label enrichment.** When the agent has API-server access, weftd queries each pod's labels on first sight, caches them, and refreshes on pod-watch events. Labels are *not* promoted to columns (the reserved set is closed); they ride in payload JSON and are queryable via `jq '.label_app'`. Customers who specifically need a label as a fast-path filter rotate it through the existing `app` column via the label-override (next item).
4. **Container-runtime-format adaptation.** Container runtime writes either `<timestamp> stdout F <line>` (CRI) or raw application output (older Docker). The CRI prefix gets stripped before parsing; this is what every k8s-aware agent does.
5. **App-name strategy.** Default `app` value comes from the container name (e.g., `api`). Configurable via a label-driven override (`weft.io/app: my-app`) so apps can opt out of the container-derived naming. Customer namespace and env still come from the agent's token, not from k8s metadata.

## Helm chart

```yaml
# values.yaml
weftd:
  endpoint: w3ft.net:443
  token: wft_a1b2c3...
  hostGroup: ""                      # optional, for multi-cluster setups
  metrics:
    enabled: true                    # node-level /proc metrics

k8s:
  cluster: my-prod-cluster           # stamps the cluster column on every row
  metadataMode: full                 # "full" | "filename-only" | "off"
  refreshInterval: 5m
  excludeNamespaces:                 # don't tail control plane noise
    - kube-system
    - kube-public
  excludePodSelector: {}             # label selector for opt-out
  hostPath: /var/log/pods            # standard; override only for non-standard distros
```

`hostGroup:` is removed — see `docs/plans/ingest-dimensions.md` for the cleanup. Per-host filtering uses the `$HOST` column directly.

Chart resources:

- `DaemonSet` with the weftd container.
- `ServiceAccount` + `ClusterRole` (read-only on `pods` and `namespaces`) + `ClusterRoleBinding`.
- `Secret` for the token, populated at install time.
- `ConfigMap` rendered from values for runtime config.
- Tolerations to schedule on tainted nodes by default (control plane, GPU, etc.). Customer overrides via values.

`metadataMode`:

- **`full`** — DaemonSet has API access, fetches labels, full enrichment.
- **`filename-only`** — no ClusterRole binding; pod/namespace/container come from log filename, labels are absent. Lower-RBAC mode for security-conscious operators.
- **`off`** — no enrichment at all; pod and namespace are not stamped on rows. Mostly a debug knob.

## Components to build

1. **`weftd/k8s/`** — new package:
   - `Identity` parser for the `/var/log/pods/<ns>_<pod>_<uid>/<container>/N.log` filename layout.
   - `LabelCache` — pod-watch via the official client-go, lazy fetch on cache miss, TTL refresh.
   - `CRIPrefixStripper` — handles the `<timestamp> stdout F <line>` and `<timestamp> stderr P <line>` framings.
2. **`cmd/weftd/main.go`** — new source kind `kind: kubernetes` that pulls the layout above and invokes the enricher. Mutually exclusive with `path:` and `unit:` — picking k8s mode means weftd does k8s, not arbitrary file tailing.
3. **Container image** — multi-arch (amd64 + arm64) image published to a public registry on every weftd release. Existing release pipeline (`.kevcons/ext/release.lua`) extends to publish images.
4. **Helm chart** — published to `https://w3ft.net/charts/` (or via `helm repo add weft https://charts.w3ft.net`). Versioned alongside weftd.
5. **Docs** — `docs/manual/weftd-k8s.md` with installation walkthrough, the three `metadataMode` modes, RBAC explanation, and example queries against k8s-stamped rows (per-pod alerting, per-namespace error rates).
6. **Tests** — kind-based or k3d-based integration test in CI: spin up a local cluster, install the chart, deploy a pod that logs, assert rows arrive with the expected dimensions.

## Sequencing

This plan depends on `docs/plans/ingest-dimensions.md` (we want pod/namespace/etc. as dimensions, not as ad-hoc payload keys). Ship dimensions first; this plan slots in cleanly behind it.

## Estimated effort

- Filename identity parser + tests: **0.5 day**
- CRI prefix stripper: **0.25 day**
- Pod-watch label cache (client-go integration): **1.5 days**
- Wire as new `kind: kubernetes` source in main.go: **0.75 day**
- Container image build + multi-arch publish: **0.75 day**
- Helm chart (DaemonSet, RBAC, ConfigMap, Secret): **1.5 days**
- Docs + per-mode walkthrough: **1 day**
- Integration test (kind/k3d in CI): **1 day**

**Total: ~7 days**.

## Non-goals

- **No sidecar pattern.** DaemonSet only. Sidecar deployments add per-app config, are harder to maintain, and don't compose well with weftd's existing single-process architecture.
- **No Kubernetes-events ingestion.** Cluster events (`kubectl get events`) are a separate signal with a separate format; not in v1.
- **No metrics-server-style metric collection.** weftd's existing `/proc` metrics work per-node; per-pod CPU/memory needs cAdvisor scraping which is a different module. Defer.
- **No Operator pattern.** A Helm chart is the install vector. We are not building a CRD-driven operator that watches `Weft` resources and reconciles state. Helm's "install once, upgrade with `helm upgrade`" covers the realistic operational lifecycle.
- **No multi-cluster aggregation in a single weft tenant for v1.** Each cluster installs its own DaemonSet with its own token. Distinguishing clusters in queries uses `host_group` (manual) until/unless we ship a cluster-id-discovery mechanism.
