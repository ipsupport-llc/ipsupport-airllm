# Helm chart + ArgoCD (P5) — design

**Goal:** Ship one production-grade, public-clean Helm chart that deploys the AirLLM
gateway + the DLP BERT sidecar pool to kubernetes, with **configurable autoscaling**
(HPA by default; KEDA opt-in, driven by the P3 saturation metric), secrets supplied
out-of-band by reference, the P2 observability wired in (ServiceMonitor + Grafana
dashboard), and a sample ArgoCD Application.

Sub-project **P5** of the deploy-as-product program (P1 auth ✅ · P2 observability ✅ ·
P3 BERT-scale ✅ · P4 standalone packaging · **P5 Helm/ArgoCD**). Spec/plan history in
`docs/superpowers/`.

## Decisions (locked with the operator)

- **App autoscaling:** HPA v2 on CPU + memory (min 2 / max 10, thresholds in values).
- **BERT autoscaling:** a switch `dlpBert.autoscaling.kind: hpa | keda | none`.
  - Default **`hpa`** (CPU) — works on any cluster, no extra operator.
  - **`keda`** — `ScaledObject` with a Prometheus trigger on the rate of
    `airllm_dlp_model_skipped_total{reason="all_busy"}` (the purpose-built
    "pool is dropping scans → add replicas" signal) **plus** a CPU trigger.
  - **`none`** — fixed `replicaCount`.
  - `minReplicaCount`/`maxReplicaCount` are values; **scale-to-zero** = set
    `minReplicaCount: 0` (KEDA only). Default `minReplicaCount: 1` (always warm).
- **Postgres + Redis:** external/managed; the chart only references an
  `existingSecret` for `DATABASE_URL`/`REDIS_URL` (nothing stateful in the chart).
- **Secrets:** secret-tool-agnostic — the chart **references an existing Secret by
  name**; Vault/VSO/ESO/SealedSecrets create it out-of-band. Nothing sensitive is
  templated into the chart.

## Chart layout

```
deploy/helm/airllm/
  Chart.yaml                # name: airllm; appVersion; no hard subchart deps
  values.yaml               # all knobs, placeholders only
  values.schema.json        # JSON Schema for values (lint + editor help)
  templates/
    _helpers.tpl            # name/fullname/labels/selectorLabels, secret-ref helper
    serviceaccount.yaml
    configmap-env.yaml      # non-secret env (AUTH_MODE, OIDC_* non-secret, ENV, …)
    app-deployment.yaml
    app-service.yaml
    app-ingress.yaml        # toggle: app.ingress.enabled
    app-hpa.yaml            # HPA v2: CPU + memory
    dlpbert-deployment.yaml # toggle: dlpBert.enabled
    dlpbert-service.yaml
    dlpbert-hpa.yaml        # rendered when dlpBert.autoscaling.kind == hpa
    dlpbert-scaledobject.yaml # rendered when == keda
    servicemonitor.yaml     # toggle: metrics.serviceMonitor.enabled
    grafana-dashboard-configmap.yaml # toggle: metrics.dashboards.enabled
    NOTES.txt               # prints the in-cluster Sidecar URL + next steps
  ci/                       # value permutations for `helm template` in CI
    hpa-values.yaml  keda-values.yaml  minimal-values.yaml  full-values.yaml
deploy/argocd/
  application.yaml          # sample ArgoCD Application (placeholders)
```

No bundled Postgres/Redis subcharts (external by decision). `Chart.yaml` declares no
required dependencies, so `helm template` works with zero network.

## values.yaml (shape; placeholders only)

```yaml
image:
  repository: ghcr.io/OWNER/ipsupport-airllm   # placeholder
  tag: ""                                       # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent
imagePullSecrets: []

# Secret-tool-agnostic: a Secret you create out-of-band (Vault/ESO/SealedSecrets).
# Keys consumed: database-url, redis-url, master-key, session-key,
# oidc-client-secret (oidc only), admin-password (optional bootstrap).
existingSecret: ""            # REQUIRED — chart fails fast if empty

config:                       # non-secret env → ConfigMap
  env: dev                    # ENV
  httpAddr: ":8080"
  authMode: local             # local | oidc
  adminUsername: admin
  captureBlobDir: /var/lib/airllm/captures
  oidc:                       # used when authMode: oidc (non-secret parts)
    issuer: ""
    clientId: ""
    redirectUrl: ""
    scopes: "openid,profile,email"
    rolesClaim: ""
    roleMap: ""

app:
  replicaCount: 2             # used only when autoscaling.enabled: false
  service: { type: ClusterIP, port: 8080 }
  ingress:
    enabled: false
    className: ""
    host: airllm.example.com  # placeholder
    tls: { enabled: false, secretName: "" }
    annotations: {}
  autoscaling:
    enabled: true
    minReplicas: 2
    maxReplicas: 10
    targetCPUUtilizationPercentage: 70
    targetMemoryUtilizationPercentage: 80
  resources:                  # sane placeholders, overridable
    requests: { cpu: 100m, memory: 128Mi }
    limits:   { cpu: "1",  memory: 512Mi }
  podSecurityContext: { runAsNonRoot: true, seccompProfile: { type: RuntimeDefault } }
  capture: { persistence: { enabled: false, size: 1Gi, storageClass: "" } } # else emptyDir

dlpBert:
  enabled: true
  image: { repository: ghcr.io/OWNER/ipsupport-airllm-dlp-bert, tag: "", pullPolicy: IfNotPresent }
  service: { headless: false, port: 8000 }   # headless → pool gets one endpoint per pod
  replicaCount: 1             # autoscaling.kind == none
  modelMaxConcurrency: 4      # surfaced in NOTES as the console value to set
  autoscaling:
    kind: hpa                 # hpa | keda | none
    minReplicaCount: 1        # set 0 for scale-to-zero (keda only)
    maxReplicaCount: 10
    cpu: { targetUtilizationPercentage: 70 }
    keda:
      prometheusServerAddress: http://prometheus-operated.monitoring.svc:9090  # placeholder
      skipRateThreshold: "1"  # replicas added when sum(rate(skipped_total{all_busy}[2m])) exceeds
      pollingInterval: 30
      cooldownPeriod: 300
  resources:
    requests: { cpu: 500m, memory: 1Gi }     # placeholder (torch model)
    limits:   { cpu: "2",  memory: 4Gi }
  # GPU example (commented in values): nvidia.com/gpu under resources.limits

metrics:
  serviceMonitor: { enabled: false, namespace: "", interval: 30s, labels: {} }
  dashboards: { enabled: false, labels: { grafana_dashboard: "1" } }
```

## Templates — behavior

- **`_helpers.tpl`** — standard `airllm.name/fullname/labels/selectorLabels`; a
  `secretRef` helper emitting `valueFrom.secretKeyRef{name: existingSecret, key}`.
  A `fail` guard: empty `existingSecret` → `{{ fail "existingSecret is required" }}`.
- **`configmap-env.yaml`** — all non-secret env from `config.*`. OIDC non-secret vars
  only when `authMode: oidc`.
- **`app-deployment.yaml`** — image, envFrom the ConfigMap, secret env via `secretRef`
  (DATABASE_URL, REDIS_URL, AIRLLM_MASTER_KEY, AIRLLM_SESSION_KEY, AIRLLM_ADMIN_PASSWORD,
  and OIDC_CLIENT_SECRET when oidc), `containerPort: 8080`, liveness `/healthz` +
  readiness `/readyz`, resources, podSecurityContext, capture volume (PVC if
  `capture.persistence.enabled` else `emptyDir`) mounted at `captureBlobDir`. Replicas
  omitted when `app.autoscaling.enabled` (HPA owns it).
- **`app-service.yaml`** — ClusterIP on `service.port`.
- **`app-ingress.yaml`** — only when `app.ingress.enabled`; className/host/TLS/annotations
  from values; networking.k8s.io/v1.
- **`app-hpa.yaml`** — autoscaling/v2 HPA, CPU + memory `Utilization` metrics, min/max
  from values. Rendered when `app.autoscaling.enabled`.
- **`dlpbert-deployment.yaml`** — only when `dlpBert.enabled`; sidecar image, `containerPort:
  8000`, resources, probes (TCP/HTTP on 8000). Replicas omitted when `autoscaling.kind` is
  `hpa`/`keda` (the scaler owns it), set to `replicaCount` when `none`.
- **`dlpbert-service.yaml`** — `clusterIP: None` when `service.headless` else ClusterIP on
  port 8000. (Both work with the pool; headless = one pool endpoint per pod, normal =
  kube-proxy LB across pods.)
- **`dlpbert-hpa.yaml`** — autoscaling/v2 HPA on CPU; rendered only when
  `autoscaling.kind == hpa`.
- **`dlpbert-scaledobject.yaml`** — keda.sh/v1alpha1 `ScaledObject`; rendered only when
  `autoscaling.kind == keda`. Triggers: (1) `prometheus` on
  `sum(rate(airllm_dlp_model_skipped_total{reason="all_busy"}[2m]))` ≥ `skipRateThreshold`;
  (2) `cpu` Utilization. `minReplicaCount`/`maxReplicaCount` from values
  (0 = scale-to-zero). `pollingInterval`/`cooldownPeriod` from values.
- **`servicemonitor.yaml`** — monitoring.coreos.com/v1; only when
  `metrics.serviceMonitor.enabled`; scrapes the app Service `/metrics` at `interval`.
- **`grafana-dashboard-configmap.yaml`** — only when `metrics.dashboards.enabled`; embeds
  the P2 dashboard via `.Files.Get "files/airllm-overview.json"`, labeled
  `grafana_dashboard: "1"` for the Grafana sidecar to auto-import. Helm's `.Files.Get`
  can only read files **inside the chart**, so the dashboard JSON is **vendored** at
  `deploy/helm/airllm/files/airllm-overview.json` (a copy of
  `deploy/grafana/dashboards/airllm-overview.json`; the plan copies it and notes the two
  must stay in sync, or a later step makes the chart the single source).
- **`NOTES.txt`** — prints: the app URL (Ingress host or port-forward), the **in-cluster
  Sidecar URL** `http://<release>-airllm-dlp-bert:8000` and the suggested
  `model_max_concurrency` to set under Admin → DLP, and which autoscaler is active.

## ArgoCD

`deploy/argocd/application.yaml` — a sample `argoproj.io/v1alpha1 Application`: placeholder
`repoURL`, `path: deploy/helm/airllm`, `targetRevision`, `destination.namespace`, and
`syncPolicy.automated` (prune + selfHeal) behind comments so it's copy-then-edit, not
apply-as-is. Documented in `docs/operations.md`.

## DLP model_url wiring

DLP config (including the sidecar URL) is runtime state in Postgres, set via the admin
console — the chart cannot pre-seed it without an app-side change. P5 therefore surfaces
the in-cluster Sidecar URL in `NOTES.txt` for the operator to paste into the console.
**Out of scope (noted as a follow-up):** an optional app-side env (e.g.
`AIRLLM_DLP_MODEL_URL`) that seeds the DLP `model_url` on first boot so the chart can wire
it automatically.

## Public-clean

values.yaml carries placeholders only: `ghcr.io/OWNER/...` image repos, `airllm.example.com`
host, `prometheus-operated.monitoring.svc` (a conventional in-cluster name, not an
environment value), empty `existingSecret`/ingress/OIDC. No real hosts, IPs, secrets,
emails, or org names anywhere. The ArgoCD Application uses placeholder repoURL/destination.

## Testing (no cluster required)

The chart is verified by **rendering**, not by standing up a cluster (respects the
operator's "no local kubectl console-ops" preference). **helm v3 is available locally**, so
the controller runs these as the gate (pure template rendering, no cluster contact):
- `helm lint deploy/helm/airllm` clean for each `ci/*` values file (with
  `values.schema.json` validating values).
- `helm template` over every permutation in `ci/`:
  `hpa-values` (default), `keda-values` (ScaledObject renders, no HPA), `minimal-values`
  (`dlpBert.enabled: false`, autoscaling off, no ingress/SM/dashboard), `full-values`
  (ingress + ServiceMonitor + dashboard + oidc) — each renders without error and
  omits/includes the right objects.
- Spot assertions on rendered output (grep): secret env uses `secretKeyRef` (never a literal
  value); the ScaledObject query is exactly the `all_busy` skip-rate expression; the app HPA
  has both CPU and memory metrics; `existingSecret: ""` makes templating **fail** with the
  required-message.
- **`kubeconform`** schema validation is recommended but **not installed locally** (no
  cluster CRDs either); it's documented as a follow-up/CI check, not a P5 gate.
- The repo has **no CI** (`.github/workflows` absent); P5 adds a `make helm-lint` target
  wrapping the lint+template commands above. Adding a GitHub Actions workflow is out of
  scope (no CI convention to extend).

## Components / files

Create: the `deploy/helm/airllm/**` tree above (incl. `files/airllm-overview.json`
vendored from `deploy/grafana/dashboards/`, and `ci/*` value files),
`deploy/argocd/application.yaml`. Modify: `Makefile` (`helm-lint` target wrapping the
lint+template gate), `docs/operations.md` (a "Kubernetes (Helm) + ArgoCD" section with
install commands, autoscaling modes, the KEDA prereq, and the Sidecar-URL step),
`docs/configuration.md` (chart values cross-reference), `README.md` (one line: k8s via the
chart). No CI workflow (none exists in the repo).

## Out of scope (P5)

- Auto-seeding DLP `model_url` from the chart (needs the app-side env bootstrap above).
- Bundled Postgres/Redis subcharts (external by decision).
- prometheus-adapter install/wiring (the KEDA path covers metric-driven scaling; an HPA
  custom-metric block is documented but the adapter itself is the cluster's responsibility).
- A published chart repo / OCI artifact (the chart lives in-repo; packaging/publishing later).
