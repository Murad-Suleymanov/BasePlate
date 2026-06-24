# YAML Reference

Complete reference for the developer-facing tenant `values.yaml` that produces a `BirService` custom resource.

## File Location

```
BasePlate-Dev/<service_name>/service.yaml      # shared service metadata
BasePlate-Dev/<service_name>/<namespace>.yaml  # per-environment config
```

- `<service_name>` — the service name (folder name)
- `service.yaml` — service-level fields shared by all environments (`repo`, `owner`, optionally `image`/`tag`/`dockerfile`)
- `<namespace>.yaml` — environment-specific operational config (filename = Kubernetes namespace, e.g., `dev`, `prod`, `stage`)

The ApplicationSet loads `service.yaml` first, then merges the environment file on top (env wins on conflict). `service.yaml` is optional but recommended — when absent, all fields must live in the env files.

## Service-level vs environment-level

| Lives in `service.yaml` | Lives in `<env>.yaml` |
|---|---|
| `repo`, `image`, `tag`, `dockerfile`, `imageTag` (image source) | `hpa`, `resources`, `replicas` (sizing) |
| `owner` (ownership metadata) | `traffic`, `canary`, `readinessProbe`, `livenessProbe` (per-env tuning) |
| `name` (optional override) | `singleton`, `maxDown`, `shutdown` |

`service.yaml` typically looks like:

```yaml
repo: https://github.com/your-org/api
owner: platform-team
```

## Single-instance vs Multi-instance

A service can have ONE deployment per environment (default, single-instance) or **multiple deployments per environment** (multi-instance) — useful for read replicas, primary/standby workers, or split front/back roles that share the same image.

### Single-instance (default)

`<env>.yaml` has operational keys at the root:

```yaml
hpa:
  minReplicas: 1
  maxReplicas: 3
resources: {...}
traffic: {...}
```

→ Creates **one** BirService named `<service_name>`, Service DNS `<service_name>-svc.<namespace>.svc.cluster.local`.

### Multi-instance

`<env>.yaml` has instance names at the root, each containing its own operational config:

```yaml
main:
  hpa:
    minReplicas: 1
    maxReplicas: 2
  resources: {...}
  traffic: {...}

slave:
  hpa:
    minReplicas: 1
    maxReplicas: 2
  resources: {...}
  traffic: {...}
```

→ Creates **N** BirServices named `<service_name>-<instance>` (e.g. `hello-csharp-main`, `hello-csharp-slave`), each with its own Service DNS `<service_name>-<instance>-svc.<namespace>.svc.cluster.local`.

Shared service-level fields (`repo`, `image`, `tag`, …) come from `service.yaml` and are inherited by every instance unless the instance overrides them.

**Shape detection is automatic**: if the root has any operational key (`hpa`, `resources`, `traffic`, …), it's single-instance; otherwise it's multi-instance (and every map at the root becomes an instance).

#### Inherit from a sibling instance (`inheritFrom`)

When two instances are nearly identical, don't duplicate the whole block. An instance can inherit the **full** config of a sibling with `inheritFrom: <name>` and override only the fields it declares. The override is a **deep merge**, so you can change a single nested leaf:

```yaml
main:
  hpa:
    minReplicas: 1
    maxReplicas: 2
  resources: {...}
  traffic: {...}
  readinessProbe: {path: /Health}

slave:
  inheritFrom: main      # copy everything from main…
  hpa:
    maxReplicas: 4       # …then override just this one leaf (minReplicas stays 1)
```

Rules:

- The target must be a defined sibling instance in the **same** file.
- No chains and no self-reference: the target must not itself use `inheritFrom`.
- `inheritFrom` is resolution metadata only — it never appears in the rendered BirService spec.
- Service-level fields from `service.yaml` (`repo`, `image`, …) still apply on top, with the instance/its parent winning on conflict.

#### Share one hostname across instances (`route.shareWith`)

By default each instance gets its **own** hostname (`<service>-<instance>-<env>.<baseDomain>`) and HTTPRoute. To put several instances behind **one** DNS name, a "child" instance joins a "parent" instance's route with `route.shareWith` and scopes itself with `route.pathPrefix`:

```yaml
main:
  hpa: {minReplicas: 1, maxReplicas: 2}
  resources: {...}
  traffic: {...}

testing:
  inheritFrom: main        # same image/config as main
  route:
    shareWith: main        # join main's hostname instead of creating its own
    pathPrefix: /testing    # only /testing reaches testing; everything else stays on main
```

This renders **one** HTTPRoute on the host `<service>-main-<env>.<baseDomain>` with two rules:

```
/testing  → <service>-testing-svc     # the child
/         → <service>-main-svc        # the parent (catch-all)
```

Gateway API matches the **longest** path prefix first, so `/testing` wins over `/` regardless of order, and `main` keeps all other traffic (none is stolen).

Variants:

- **Weighted split** — omit `pathPrefix` and set `weight` instead. The child becomes a weighted backend on the parent's `/` rule (e.g. `weight: 10` → 10% of all traffic to the child). Use for blue/green or gradual cutover.
- Multiple children may share the same parent (each adds its own rule/backend).

Rules:

- `shareWith` must name a defined sibling instance in the **same** file.
- No chains and no self-reference: the parent must not itself use `route.shareWith`.
- A child must **not** set its own `hostname` — the host comes from the parent.
- `pathPrefix`, when set, must start with `/`.
- The child still gets its own Deployment + Service; it just has **no HTTPRoute of its own** (reachable only through the shared host). Children share the parent's Service port (set it once on the parent — `inheritFrom` keeps them in sync).
- This is distinct from `canary:` (a shadow variant of a single service); `route.shareWith` joins two **independent, standing** instances under one host.

## Editor Setup

The chart ships a JSON schema (`charts/birservice/values.schema.json`) and a workspace VSCode binding so any `*/dev.yaml` / `*/prod.yaml` opened from BasePlate-Dev gets real-time validation:

- Unknown fields are flagged immediately (catches typos like `ejectUnhelthy`).
- Type mismatches (e.g., `replicas: "3"` as string) underline the line.
- `Ctrl+Space` autocompletes valid field names and shows the description.

If you clone BasePlate-Dev without a sibling BasePlate checkout, the relative schema path fails — switch `.vscode/settings.json` to the GitHub raw URL (commented in that file).

See [Validation](validation.md) for the full feedback loop (IDE → pre-commit → PR CI).

## Minimal Examples

=== "Container Image"

    ```yaml
    image: ealen/echo-server:0.9.2
    ```

=== "Git Repository"

    ```yaml
    repo: https://github.com/docker/welcome-to-docker
    ```

=== "Full mesh-enabled"

    ```yaml
    repo: https://github.com/your-org/api
    hpa:
      minReplicas: 2
      maxReplicas: 5
    traffic:
      provider: istio
    ```

## All Fields

```yaml
# ┌─────────────────────────────────────────────────┐
# │ Image source (specify ONE of image / repo)      │
# └─────────────────────────────────────────────────┘

# Pre-built container image from any registry
image: ""

# Git repository URL containing a Dockerfile
repo: ""

# Git branch, tag, or commit to build (only with repo)
# Default: repository's default branch
tag: ""

# Path to Dockerfile relative to repo root (only with repo)
# Default: "Dockerfile"
dockerfile: ""

# Rollback override — previous build SHA (e.g. "abc1234").
# When set, deploys this exact image instead of the latest build.
imageTag: ""

# When true and repo is a GitHub URL, the platform adds a GitHub Actions
# build workflow into the repo. Requires the platform GitHub PAT.
injectPipeline: false

# ┌─────────────────────────────────────────────────┐
# │ Runtime                                         │
# └─────────────────────────────────────────────────┘

# Port the application listens on.
# 0 / omitted = auto-detect from image, fallback 8080.
port: 0

# Container port if different from service port. 0 = same as port.
containerPort: 0

# Custom DNS hostname. Empty = <name>-<namespace>.<baseDomain>.
hostname: ""

# Expose externally via HTTPRoute + DNS. false = internal ClusterIP only.
expose: true

# ┌─────────────────────────────────────────────────┐
# │ Scaling                                         │
# └─────────────────────────────────────────────────┘

# Fixed pod count. Default 1 if HPA is not set.
# When set, HPA is NOT created (replicas wins).
replicas: 1

# Horizontal Pod Autoscaler. min/max required together. Optionally pick ONE
# scaling signal via scaleType (cpu/memory/rps/worker) + a per-pod target — the
# platform wires up the metric. Omit scaleType to default to CPU at 80%.
hpa:
  minReplicas: 2
  maxReplicas: 10
  scaleType: cpu      # cpu | memory | rps | worker
  target: 70          # cpu/memory: util %; rps: req/s; worker: busy workers

# App can only run one version at a time (leader-elected, exclusive lock,
# in-memory state). Default false → zero-downtime rolling deploy.
# true → old pods fully stop before new ones start (brief downtime),
# PDB skipped, replicas/HPA still honored but typically set to 1.
singleton: false

# Maximum pods that may go down simultaneously during voluntary disruptions
# (node drains, cluster upgrades, autoscaler removing nodes).
# Default floor(replicas/2). Lower it for latency-critical apps; 0 forbids
# voluntary disruption (blocks node drains). Ignored when singleton: true.
maxDown: 1

# ┌─────────────────────────────────────────────────┐
# │ Resources                                       │
# └─────────────────────────────────────────────────┘

resources:
  requests:
    memory: 200Mi   # default
    cpu: 75m        # default
  limits:
    memory: 400Mi   # default: 2× requests
    cpu: 150m       # default: 2× requests

# ┌─────────────────────────────────────────────────┐
# │ Health probes                                   │
# └─────────────────────────────────────────────────┘

# HTTP readiness probe. Without this, a default TCP probe is used.
readinessProbe:
  path: /healthz
  port: 8080         # defaults to containerPort

# HTTP liveness probe. Omit to disable.
livenessProbe:
  path: /healthz
  port: 8080

# ┌─────────────────────────────────────────────────┐
# │ Observability                                   │
# └─────────────────────────────────────────────────┘

# Prometheus ServiceMonitor. Defaults: enabled at /metrics.
# Object form: { enabled: true, path: /actuator/prometheus }
metrics: true

# Graceful termination. preStop drains endpoints; total grace =
# preStopSleepSeconds + drainBufferSeconds.
shutdown:
  preStopSleepSeconds: 15
  drainBufferSeconds: 5

# ┌─────────────────────────────────────────────────┐
# │ Service mesh (Istio ambient)                    │
# │ Presence of `traffic:` implies mesh intent      │
# └─────────────────────────────────────────────────┘

traffic:
  provider: istio                # empty or istio
  rateLimit:
    enabled: true                # Envoy local rate limit
    mode: local
    local:
      requestsPerSecond: 100
      burst: 20
  # Outlier detection: failing pods temporarily removed from LB pool.
  # Default true (5 consecutive 5xx or 3 consecutive gateway errors
  # 502/503/504 → 30s eject, max 50% of pods, panic mode disabled
  # so ejected pods never receive traffic again until ejection expires).
  # Set false for apps that legitimately return 5xx (webhooks, batch).
  ejectUnhealthy: true
  # Load balancer. Default false → round-robin.
  # true → least-request (good for heterogeneous request latency).
  latencyAware: false

# ┌─────────────────────────────────────────────────┐
# │ Canary rollout                                  │
# └─────────────────────────────────────────────────┘

canary:
  enabled: true
  weight: 10          # % of traffic to canary (0–100), default 10
  image: ""           # override; if empty, derived from spec.image/repo
  tag: ""             # canary image tag override
```

## Field Details

### Image source

#### `image`

Fully-qualified container image reference. Mutually exclusive with `repo` — the semantic lint catches that.

```yaml
image: nginx:alpine
image: ghcr.io/your-org/your-app:v1.0.0
```

#### `repo`

Git repository URL. Supported prefixes: `https://github.com/`, `https://gitlab.com/`, `https://bitbucket.org/`, or any URL ending in `.git`. The platform clones, builds with Kaniko, pushes to the internal registry.

#### `tag`

Git branch, tag, or commit ref for the build. Default: repository's default branch. Changing it triggers a rebuild — see [Rebuild Strategies](rebuild-strategies.md).

#### `dockerfile`

Path to the Dockerfile relative to repo root. Useful for monorepos. Default `Dockerfile`.

#### `imageTag`

Pin the deployed image to a previous successful build SHA — used for rollback. Example:

```yaml
repo: https://github.com/your-org/api
imageTag: a1b2c3d4   # previous good build
```

When set, the operator ignores the latest pipeline build and serves this tag.

#### `injectPipeline`

When `true` and `repo` is a GitHub URL, the operator adds `.github/workflows/easy-deploy.yml` to the tenant repo (one-time). Requires `github-pipeline-secret` configured platform-side.

### Runtime

#### `port` / `containerPort`

Service port the platform exposes. `containerPort` only matters when the container listens on a different port than the service should expose externally. Both can be `0` or omitted — the operator auto-detects from the image (`EXPOSE`, `ENV PORT`, `CMD --port`). See [Port Auto-Detection](auto-detection.md). Fallback: `8080`.

#### `hostname`

Custom external DNS name. Default: `<name>-<namespace>.<BASE_DOMAIN>` (e.g. `api-prod.easysolution.work`). If you set a custom hostname, you must configure DNS for that domain yourself — ExternalDNS only manages the platform's zone.

#### `expose`

`true` (default) creates an HTTPRoute + DNS entry. `false` keeps the service ClusterIP-only — useful for internal services consumed by other workloads.

### Scaling

#### `replicas`

Fixed pod count. Default `1`. When set, HPA is not created.

#### `hpa`

`minReplicas` + `maxReplicas` (both required together) bound the replica range.
Optionally pick **one** scaling signal via `scaleType` plus a per-pod `target`; the
platform resolves the underlying metric. Omit `scaleType` to default to **CPU at 80%**.

```yaml
hpa:
  minReplicas: 2
  maxReplicas: 10
  scaleType: cpu      # cpu | memory | rps | worker
  target: 70          # % for cpu/memory/worker; req/s for rps
```

| `scaleType` | `target` | Prerequisite |
|-------------|----------|--------------|
| `cpu` | CPU utilization % (default 80) | — |
| `memory` | memory utilization % (default 80) | — |
| `rps` | requests/sec per pod (integer) | `traffic` (waypoint) |
| `worker` | worker-pool saturation % (default 80) | `metrics` + worker exporter |

`target` is a utilization **%** for `cpu`/`memory`/`worker` (defaults to 80) and an
**integer** req/s for `rps` (required). `worker` is language-agnostic — a central
recording rule normalizes each runtime's busy/max worker gauges into one
`app_worker_utilization` %. A signal whose prerequisite is missing is created but
stays idle (the operator logs a warning). The legacy `targetRPS` equals
`scaleType: rps`.

#### `singleton`

The single domain knob for "this app cannot run two versions concurrently":

- `false` / omitted → RollingUpdate, zero-downtime (default).
- `true` → Recreate strategy: old pods fully terminate before new ones start. PDB skipped. Use for leader-elected jobs, in-memory state, exclusive resource locks.

The semantic lint rejects `singleton: true` combined with `hpa.minReplicas > 1` or `replicas > 1`.

#### `maxDown`

Tunes the PodDisruptionBudget's `maxUnavailable` count — how many pods may be evicted at once during voluntary disruptions.

| `replicas` | `maxDown` omitted | `maxDown: 1` | `maxDown: 0` |
|---|---|---|---|
| 1 | PDB not created | not created | not created |
| 2 | 1 (half) | 1 | 0 (drains blocked) |
| 4 | 2 (half) | 1 (stricter) | 0 (drains blocked) |
| HPA min 3 | 1 | 1 | 0 |

Set lower for latency-critical apps. Set `0` only when you truly cannot tolerate even one pod restart — be aware it blocks node drains until pods reschedule.

### Resources

#### `resources`

Container requests + limits. Defaults when omitted:

- `requests.memory: 200Mi`
- `requests.cpu: 75m`
- `limits` = 2× requests

The semantic lint rejects `limits.memory < requests.memory` and `limits.cpu < requests.cpu` (k8s would also reject, but failing in PR is faster).

### Health probes

#### `readinessProbe` / `livenessProbe`

Both take `path` (required) and `port` (optional, defaults to `containerPort`). When you omit `readinessProbe`, the operator inserts a default TCP probe on `containerPort` so rolling updates remain zero-downtime even for apps that forget probes.

```yaml
readinessProbe:
  path: /healthz
livenessProbe:
  path: /healthz
```

### Observability

#### `metrics`

Three forms:

- `metrics: true` (default) — ServiceMonitor at `/metrics`
- `metrics: false` — disabled
- `metrics: { enabled: true, path: /actuator/prometheus }` — custom path (Spring Boot, etc.)

#### `shutdown`

Graceful termination tuning:

- `preStopSleepSeconds` (default 15) — endpoint drain wait before SIGTERM.
- `drainBufferSeconds` (default 5) — post-SIGTERM in-flight request budget.

`terminationGracePeriodSeconds` is auto-computed as the sum. Increase `drainBufferSeconds` for long uploads, streaming, or batch work.

### Service mesh

Presence of the `traffic:` block opts the workload into Istio ambient mesh. The operator labels the namespace, ensures a waypoint Gateway, and creates DestinationRule + (optionally) EnvoyFilter resources.

#### `traffic.provider`

Empty or `istio`. Reserved for future mesh providers.

#### `traffic.rateLimit`

Envoy local (per-pod) rate limit:

```yaml
traffic:
  rateLimit:
    enabled: true
    mode: local
    local:
      requestsPerSecond: 100
      burst: 20
```

Token bucket: `max_tokens = requestsPerSecond + burst`, refills `requestsPerSecond` per second. The lint warns if `rateLimit.enabled` is set without the `traffic:` block.

#### `traffic.ejectUnhealthy`

Toggle for Istio outlier detection (failing endpoints removed from LB pool):

- `true` / omitted → enabled with platform defaults (5 consecutive 5xx or 3 consecutive gateway errors (502/503/504) → 30s eject, max 50% of pods, panic mode disabled via `minHealthPercent: 0`).
- `false` → disabled. Use for workloads that *legitimately* return 5xx (webhook endpoints, batch processors with retries). The lint emits a notice when disabled.

No tuning knobs by design — the thresholds are platform-managed.

#### `traffic.latencyAware`

Load balancer algorithm:

- `false` / omitted → ROUND_ROBIN (Istio default).
- `true` → LEAST_REQUEST (power-of-two-choices).

Only flip this for **heterogeneous-latency** workloads — services where some endpoints hit cache (fast) and others hit DB (slow). For uniform request latency, round-robin is better. Verify P50 vs P99 in Grafana before enabling.

### Canary

#### `canary`

Weighted canary rollout via HTTPRoute split:

```yaml
canary:
  enabled: true
  weight: 10          # 10% to canary, 90% to stable
  tag: v2.0.0-rc1     # canary image tag
```

When `enabled: true`, the operator:

1. Locks the current `buildTag` as `status.stableTag`.
2. Stable deployment keeps serving the locked tag.
3. Canary deployment runs the new image at `tag`.
4. HTTPRoute splits traffic by `weight`.

Set `enabled: false` (or remove) to promote: canary disappears, stable picks up the latest tag.

## Auto-Detected Fields

| Field | Auto-Detected From | Fallback |
|---|---|---|
| `name` | Folder path (`api/` → `api`) | — |
| `namespace` | Filename (`prod.yaml` → `prod`) | — |
| `hostname` | `<name>-<namespace>.<BASE_DOMAIN>` | — |
| `port` | Image `EXPOSE`, `ENV PORT=`, `CMD --port` | `8080` |
| `containerPort` | Same as `port` | — |
| `tag` | Repository default branch (git) / `latest` (image) | — |

## Complete Examples

### Simple Web Server

```yaml title="web/dev.yaml"
image: nginx:alpine
port: 80
```

### Git-Based API with HPA + Mesh

```yaml title="api/prod.yaml"
repo: https://github.com/your-org/api
hpa:
  minReplicas: 2
  maxReplicas: 10
resources:
  requests:
    memory: 256Mi
    cpu: 100m
traffic:
  rateLimit:
    enabled: true
    local:
      requestsPerSecond: 200
      burst: 50
```

### Singleton Background Worker

```yaml title="worker/prod.yaml"
repo: https://github.com/your-org/worker
replicas: 1
singleton: true                  # leader-elected, exclusive DB lock
readinessProbe:
  path: /ready
shutdown:
  preStopSleepSeconds: 30
  drainBufferSeconds: 60         # long-running jobs need time to wrap up
```

### Latency-Critical API

```yaml title="payments/prod.yaml"
repo: https://github.com/your-org/payments
hpa:
  minReplicas: 4
  maxReplicas: 20
maxDown: 1                       # never lose more than one pod at a time
traffic:
  latencyAware: true             # mixed cache/DB latency, least-request helps
  rateLimit:
    enabled: true
    local:
      requestsPerSecond: 500
      burst: 100
```

### Webhook Receiver (5xx is normal)

```yaml title="github-webhook/prod.yaml"
repo: https://github.com/your-org/gh-webhook
traffic:
  ejectUnhealthy: false          # upstream sometimes returns 5xx legitimately
```

### Canary Rollout

```yaml title="api/prod.yaml"
repo: https://github.com/your-org/api
hpa:
  minReplicas: 3
  maxReplicas: 10
canary:
  enabled: true
  weight: 20
  tag: v3.0.0-rc1
```
