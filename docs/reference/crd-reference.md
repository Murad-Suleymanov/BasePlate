# CRD Reference

Complete specification of the `BirService` custom resource. This is the contract the operator reconciles against; tenant `values.yaml` files are templated into a `BirService` CR by the `birservice` Helm chart.

## Overview

| Property | Value |
|---|---|
| **API Group** | `deploy.easydeploy.io` |
| **Version** | `v1alpha1` |
| **Kind** | `BirService` |
| **Plural** | `birservices` |
| **Short Name** | `bs` |
| **Scope** | Namespaced |

## Usage

```bash
kubectl -n dev get birservices
kubectl -n dev get bs
kubectl -n dev describe bs echo
kubectl -n dev get bs echo -o yaml
kubectl explain birservices.spec.singleton   # field-level docs
```

## Printer Columns

| Column | JSON Path |
|---|---|
| Repo | `.spec.repo` |
| Image | `.spec.image` |
| Port | `.spec.port` |
| Hostname | `.spec.hostname` |
| Available | `.status.availableReplicas` |
| Build | `.status.buildStatus` |

## Spec — Top-Level Fields

| Field | Type | Default | Notes |
|---|---|---|---|
| `image` | string | — | Pre-built image. Exclusive with `repo`. |
| `repo` | string | — | Container repo or git URL. Exclusive with `image`. |
| `tag` | string | repo default branch / `latest` | Git ref or image tag. |
| `dockerfile` | string | `Dockerfile` | Path inside repo. |
| `imageTag` | string | — | Rollback pin to a specific build SHA. |
| `injectPipeline` | bool | `false` | Inject GH Actions build workflow into repo (GitHub only). |
| `port` | int32 (1–65535) | auto-detect → 8080 | Service port. |
| `containerPort` | int32 (1–65535) | same as `port` | Container port if different. |
| `hostname` | string | `<name>-<ns>.<baseDomain>` | External DNS name. |
| `expose` | bool | `true` | `false` = ClusterIP-only, no HTTPRoute/DNS. |
| `replicas` | int32 (≥0) | `1` | Fixed count. Wins over HPA when set. |
| `hpa` | object | — | `minReplicas` + `maxReplicas` (both required). |
| `resources` | object | requests cpu=75m mem=200Mi; limits 2× | Container resource block. |
| `readinessProbe` | object | TCP default | `path` (required) + `port`. |
| `livenessProbe` | object | omitted | `path` (required) + `port`. |
| `metrics` | bool / object | `true` (enabled at `/metrics`) | ServiceMonitor toggle. |
| `traffic` | object | — | Presence enables Istio ambient mesh. |
| `canary` | object | — | Weighted canary rollout. |
| `singleton` | bool | `false` | Recreate strategy + no PDB. |
| `maxDown` | int32 (≥0) | `floor(replicas/2)` | PDB `maxUnavailable` count. |
| `shutdown` | object | preStop=15s, drain=5s | Graceful termination. |

## Spec — Nested Objects

### `spec.hpa`

```yaml
hpa:
  minReplicas: 2     # int32, ≥1 enables HPA; both must be set
  maxReplicas: 5     # int32, ≥1
```

Both fields required together to take effect. If `replicas` is set anywhere on the spec, HPA is *not* created (the lint warns).

### `spec.resources`

```yaml
resources:
  requests:
    memory: "200Mi"
    cpu: "75m"
  limits:
    memory: "400Mi"
    cpu: "150m"
```

Defaults applied per field when omitted: requests `cpu=75m`, `memory=200Mi`; limits = 2× the *resolved* requests.

### `spec.readinessProbe` / `spec.livenessProbe`

```yaml
readinessProbe:
  path: /healthz       # required
  port: 8080           # optional, defaults to containerPort
```

When `readinessProbe` is omitted, the operator inserts a default TCP probe on `containerPort` (initialDelay=3s, period=5s, failureThreshold=3) so rolling updates are zero-downtime by default. HTTP probes (when declared) use initialDelay=5s, period=5s for readiness; 15s/10s for liveness.

### `spec.metrics`

Three accepted forms:

```yaml
metrics: true                            # ServiceMonitor at /metrics
metrics: false                           # no ServiceMonitor
metrics:
  enabled: true
  path: /actuator/prometheus             # custom path
```

### `spec.traffic`

Presence implies mesh intent (Istio ambient). The operator labels the namespace `istio.io/dataplane-mode=ambient`, ensures a waypoint Gateway, and creates DestinationRule + (optionally) EnvoyFilter.

```yaml
traffic:
  provider: istio                        # empty or "istio"
  rateLimit:
    enabled: true
    mode: local                          # only "local" implemented
    local:
      requestsPerSecond: 100             # int32, ≥1
      burst: 20                          # int32, ≥0
  ejectUnhealthy: true                   # bool, default true
  latencyAware: false                    # bool, default false (round-robin)
```

`ejectUnhealthy: true` (or omitted) creates a DestinationRule with platform outlier-detection defaults:

| Setting | Value |
|---|---|
| `consecutive5xxErrors` | 5 |
| `interval` | 10s |
| `baseEjectionTime` | 30s |
| `maxEjectionPercent` | 50 |

`latencyAware: true` sets `trafficPolicy.loadBalancer.simple = LEAST_REQUEST` in the DestinationRule. When omitted/false the field isn't set and Istio's default (ROUND_ROBIN) applies.

### `spec.canary`

```yaml
canary:
  enabled: true                          # required
  weight: 10                             # int32 (0–100), default 10
  image: ""                              # full image URL; derived if empty
  tag: ""                                # image tag override
```

When enabled the operator:

1. Sets `status.stableTag = status.buildTag` (locks the current stable version).
2. Keeps the main Deployment on `stableTag` even when new builds arrive.
3. Creates `<name>-canary` Deployment + Service running the canary image.
4. Updates the HTTPRoute with two `backendRefs` weighted by `weight`.

Setting `enabled: false` (or removing `canary`) promotes: canary infrastructure is torn down, `stableTag` is cleared, the main Deployment picks up the latest build.

### `spec.shutdown`

```yaml
shutdown:
  preStopSleepSeconds: 15                # 0–600, default 15
  drainBufferSeconds:  5                 # 0–600, default 5
```

`terminationGracePeriodSeconds` is auto-computed as the sum (default 20s). Increase `drainBufferSeconds` for long-running requests (uploads, streaming, batch jobs).

### `spec.singleton`

```yaml
singleton: true                          # bool, default false
```

When `true`, the operator uses `Recreate` deployment strategy (all old pods stop, then new pods start — brief downtime). PDB is skipped (meaningless with one replica). `maxDown` is ignored. `replicas`/`hpa` are still honored but typically set to `1`.

### `spec.maxDown`

```yaml
maxDown: 1                               # int32, ≥0
```

PodDisruptionBudget `maxUnavailable` count. Ignored when `singleton: true` or effective replicas < 2. Default `floor(N/2)` where `N` = `replicas` or `hpa.minReplicas`. The PDB is skipped entirely when `maxDown >= effective_replicas` (it would never block).

## Status

```yaml
status:
  availableReplicas: 3                   # int32 — from Deployment.status
  buildImage: "registry.../app:abc1234"  # last build image URL
  buildStatus: "Succeeded"               # Building | Succeeded | Failed
  buildTag: "abc1234"                    # tag used in last build
  lastRebuild: "1717804800"              # unix ts of last webhook rebuild
  stableTag: "abc1234"                   # canary mode: locked stable tag
  canaryImage: "registry.../app:v2-rc1"  # canary mode: display only
```

### Status fields

| Field | Set By | Description |
|---|---|---|
| `availableReplicas` | reconciler | Mirror of `Deployment.status.availableReplicas`. |
| `buildImage` | reconciler | Full image URL of the most recent build. |
| `buildStatus` | reconciler | `Building`, `Succeeded`, or `Failed`. |
| `buildTag` | reconciler | Tag for the most recent build (drives `needsRebuild`). |
| `lastRebuild` | reconciler | Annotation timestamp consumed after rebuild. |
| `stableTag` | reconciler (canary) | Locked stable tag while a canary is active. |
| `canaryImage` | reconciler (canary) | Display-only canary image URL. |

## Annotations the Operator Watches

| Annotation | Description | Set By |
|---|---|---|
| `deploy.easydeploy.io/rebuild` | Unix timestamp — triggers a rebuild when changed | webhook server |

## Annotations the Operator Sets

| Annotation | On | Purpose |
|---|---|---|
| `deploy.easydeploy.io/pipeline-injected` | BirService | Marks repo as having received the GitHub Actions workflow |
| `deploy.easydeploy.io/mesh-rollout-generation` | Deployment | Avoids re-rollout for the same spec generation |
| `instrumentation.opentelemetry.io/inject-<runtime>` | Pod template | Selects OTel auto-instrumentation SDK |

## Labels the Operator Sets

| Label | Description | Applied To |
|---|---|---|
| `app.kubernetes.io/name` | BirService name | All child resources |
| `app.kubernetes.io/managed-by` | `easy-deploy-operator` | All child resources |
| `deploy.easydeploy.io/tenant` | Namespace | All child resources |
| `deploy.easydeploy.io/purpose` | `build` | Kaniko jobs |
| `deploy.easydeploy.io/build-tag` | Image tag | Kaniko jobs |
| `istio.io/use-waypoint` | `waypoint` | Service + Pod template when mesh-enabled |

## Resources the Operator Creates

For each `BirService` with `repo`/`image` set:

| Resource | Naming | Created When |
|---|---|---|
| `Deployment` | `<name>-deploy` | always |
| `Service` (ClusterIP) | `<name>-svc` | always |
| `HTTPRoute` | `<name>-route` | `expose: true` and hostname resolvable |
| `HorizontalPodAutoscaler` | `<name>-hpa` | `hpa.minReplicas`+`maxReplicas` set, `replicas` not set |
| `PodDisruptionBudget` | `<name>-pdb` | effective replicas ≥ 2 and `singleton: false` |
| `ServiceMonitor` | `<name>-monitor` | `metrics` enabled |
| `DestinationRule` | `<name>-outlier` | `traffic:` present; merges outlier-detection + LB policy |
| `EnvoyFilter` | `<name>-ratelimit` | `traffic.rateLimit.enabled: true` |
| `ObservabilityPolicy` | `<name>-route-tracing` | tracing ratio > 0, exposed |
| `Job` | `<name>-build-<tag>` | first build of a Git repo |
| `Deployment` (canary) | `<name>-canary-deploy` | `canary.enabled: true` |
| `Service` (canary) | `<name>-canary-svc` | `canary.enabled: true` |

All resources are owned by the `BirService` CR via owner references → garbage collected when the CR is deleted. The waypoint `Gateway` is namespace-scoped and shared by all mesh-enabled BirServices in the namespace; it is deleted only when the last mesh-enabled sibling is removed.

## Full Schema

The authoritative schema lives in [`charts/easy-deploy-platform/values.yaml`](https://github.com/Murad-Suleymanov/BasePlate/blob/main/charts/easy-deploy-platform/values.yaml) under `crd.specProperties`. The operator chart renders it into the CRD's OpenAPI v3 schema, so `kubectl explain birservices.spec.<field>` returns the canonical description.
