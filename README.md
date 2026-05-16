# Easy-Deploy

A Kubernetes-native platform that turns a **single-line YAML** into a fully deployed, HTTPS-enabled service with automatic DNS.

```yaml
repo: https://github.com/your-org/your-app
```

That's it. Within minutes, your app is live at `https://your-app-dev.easysolution.work` with TLS, DNS, and auto-scaling — zero Kubernetes knowledge required.

---

## Architecture

```
Developer                          Platform (Kubernetes)
─────────                          ─────────────────────
                                   ┌─────────────────────────────────┐
 1-line YAML ──► BasePlate-Dev ──► │  ArgoCD ApplicationSet          │
                  (Git repo)       │       │                         │
                                   │       ▼                         │
                                   │  BirService CR                  │
                                   │       │                         │
                                   │       ▼                         │
                                   │  Easy-Deploy Operator           │
                                   │       │                         │
                                   │  ┌────┴────────┐               │
                                   │  │ Git repo?   │ Ready image?  │
                                   │  ▼             ▼               │
                                   │  Kaniko Job    Deployment      │
                                   │  (build+push)  Service         │
                                   │  ▼             HTTPRoute       │
                                   │  Local Registry       │        │
                                   │  ▼                    ▼        │
                                   │  Deployment    ExternalDNS     │
                                   │  Service       (Cloudflare)    │
                                   │  HTTPRoute            │        │
                                   │       │               ▼        │
                                   │       ▼          DNS A Record  │
                                   │  NGINX Gateway                 │
                                   │  (TLS termination)             │
                                   └─────────────────────────────────┘
                                              │
                                              ▼
                                   https://app-ns.easysolution.work
```

## How it works

| Step | What happens | Who does it |
|------|-------------|-------------|
| 1 | Developer commits YAML to `BasePlate-Dev` repo | Developer |
| 2 | ArgoCD discovers the file and creates a `BirService` CR | ArgoCD ApplicationSet |
| 3a | If `image:` is set, creates Deployment + Service + HTTPRoute | Operator |
| 3b | If `repo:` is a Git URL, runs Kaniko build, pushes to local registry, then deploys | Operator |
| 4 | HTTPRoute is read by ExternalDNS, creates Cloudflare DNS record | ExternalDNS |
| 5 | Wildcard TLS certificate covers all subdomains | cert-manager |
| 6 | Service is live at `https://<name>-<namespace>.easysolution.work` | NGINX Gateway |

---

## Developer YAML reference

Place YAML files in `BasePlate-Dev` repo at `<service_name>/<namespace_name>.yaml`.

The **folder** becomes the service name. The **filename** (without `.yaml`) becomes the namespace.

Example: `api/prod.yaml` → service `api` in namespace `prod`.

### Minimal examples

**Deploy a container image:**

```yaml
image: ealen/echo-server:0.9.2
```

**Build and deploy from a Git repo:**

```yaml
repo: https://github.com/docker/welcome-to-docker
```

### Available fields (short list)

```yaml
# Image source (one of)
image: ""                    # Pre-built image (e.g. nginx:latest)
repo: ""                     # Git URL — platform builds with Kaniko

# Build
tag: ""                      # Git ref or image tag
dockerfile: ""               # Path inside repo (default: Dockerfile)
imageTag: ""                 # Rollback pin to a previous build SHA

# Runtime
port: 0                      # auto-detect → 8080
containerPort: 0             # default: same as port
hostname: ""                 # default: <name>-<namespace>.<baseDomain>
expose: true                 # false = ClusterIP-only (no HTTPRoute/DNS)

# Scaling
replicas: 1                  # fixed count
hpa: { minReplicas: 2, maxReplicas: 10 }   # autoscaling (alternative)
singleton: false             # true → Recreate strategy, PDB skipped
maxDown: 1                   # PDB maxUnavailable (default floor(replicas/2))

# Resources / probes / shutdown / metrics — see docs/user-guide/yaml-reference.md
resources: { requests: { memory: 200Mi, cpu: 75m }, limits: { memory: 400Mi, cpu: 150m } }
readinessProbe: { path: /healthz }
livenessProbe:  { path: /healthz }
metrics: true                # ServiceMonitor at /metrics
shutdown: { preStopSleepSeconds: 15, drainBufferSeconds: 5 }

# Service mesh (Istio ambient) — presence enables mesh
traffic:
  provider: istio
  rateLimit: { enabled: true, mode: local, local: { requestsPerSecond: 100, burst: 20 } }
  ejectUnhealthy: true       # outlier detection (default true)
  latencyAware: false        # LB: false=round-robin, true=least-request

# Canary rollout
canary: { enabled: true, weight: 10, tag: v2.0.0-rc1 }
```

Full field reference, defaults, and design notes: [docs/user-guide/yaml-reference.md](docs/user-guide/yaml-reference.md). API contract: [docs/reference/crd-reference.md](docs/reference/crd-reference.md).

### Auto-detection

| Field | Auto-detected from | Fallback |
|-------|-------------------|----------|
| `name` | Folder path (`api/` → `api`) | — |
| `namespace` | Filename (`prod.yaml` → `prod`) | — |
| `hostname` | `<name>-<namespace>.easysolution.work` | — |
| `port` | Image `EXPOSE`, `ENV PORT=`, or `CMD --port` | 8080 |
| `tag` | — | Repo default branch (Git) / `latest` (image) |

---

## Repository structure

The platform spans three Git repositories:

| Repo | Purpose |
|------|---------|
| **[BasePlate](https://github.com/Murad-Suleymanov/BasePlate)** (this) | Go operator, CRD, Helm chart, operator manifests |
| **[BasePlate-Infra](https://github.com/Murad-Suleymanov/BasePlate-Infra)** | ArgoCD apps, gateway/registry/webhook manifests, install scripts |
| **[BasePlate-Dev](https://github.com/Murad-Suleymanov/BasePlate-Dev)** | Developer YAML files (`*/*.yaml` = `service_name/namespace_name.yaml`) |

```
BasePlate/                                   # Platform repo (this repo)
├── api/v1alpha1/                            # BirService CRD Go types
│   ├── birservice_types.go                  #   Spec + Status definitions
│   ├── groupversion_info.go                 #   API group registration
│   └── zz_generated.deepcopy.go             #   Generated deep copy
├── cmd/operator/main.go                     # Operator entrypoint
├── internal/
│   ├── controller/                          # Reconciliation logic
│   │   ├── birservice_controller.go         #   Deployment, Service, HTTPRoute, HPA, PDB, ServiceMonitor, Kaniko
│   │   ├── canary.go                        #   Canary deployment + weighted HTTPRoute
│   │   ├── destinationrule_outlier.go       #   Istio DestinationRule (outlier + LB)
│   │   ├── envoyfilter_ratelimit.go         #   Istio EnvoyFilter (local rate limit)
│   │   ├── namespace_istio.go               #   Ambient mesh labels + waypoint Gateway
│   │   └── metrics.go                       #   Controller-level Prometheus metrics
│   ├── credentials/                         # Resolves GitHub PAT + registry creds
│   ├── injector/                            # Auto-inject GH Actions build workflow into tenant repos
│   ├── registry/                            # Registry v2 client (image inspect, port detect, runtime detect)
│   └── webhook/                             # GitHub webhook → rebuild trigger
├── charts/
│   ├── birservice/                          # Tenant chart: values.yaml → BirService CR
│   │   ├── Chart.yaml
│   │   ├── values.yaml
│   │   ├── values.schema.json               #   JSON schema: Helm + IDE + pre-commit
│   │   └── templates/{birservice,namespace,waypoint}.yaml
│   └── easy-deploy-platform/                # Operator chart (RBAC, Deployment, CRD)
│       ├── values.yaml                      #   crd.specProperties = OpenAPI schema
│       └── templates/{crd,deployment,rbac,webhook,...}.yaml
├── config/crd/                              # Reference CRD YAML (single-doc form)
├── scripts/
│   ├── birservice-lint.sh                   # Semantic cross-field rules
│   ├── birservice-lint-multi.sh             # Pre-commit wrapper (multi-file)
│   └── birservice-helm-validate.sh          # helm template + line-annotated errors
├── .pre-commit-hooks.yaml                   # Hook definitions consumed by tenant repos
├── docs/                                    # mkdocs material site
│   ├── user-guide/{yaml-reference,validation,...}.md
│   ├── reference/{crd-reference,troubleshooting,...}.md
│   └── architecture/{operator,networking,build-pipeline}.md
├── Dockerfile
└── .github/workflows/operator-image.yml     # Build + push operator image; bump chart values in Infra

BasePlate-Infra/                             # Infrastructure repo
├── dev/  prod/                               # Per-env ArgoCD AppSets + platform values
│   ├── applicationsets/                     #   AppSet definitions
│   ├── platform/values/                     #   easy-deploy-platform-values.yaml (image pinned by CI)
│   └── infra-applications-values.yaml
├── argocd/  cert-manager/  istio*/  nginx-gateway-fabric/  ...
└── install-*.sh                             # Bootstrap scripts

BasePlate-Dev/                               # Tenant values catalog
├── .vscode/settings.json                    # Maps */dev.yaml + */prod.yaml to chart schema
├── .pre-commit-config.yaml                  # References BasePlate hooks
├── .github/workflows/validate.yml           # CI: pre-commit + sticky PR comment on failure
├── hello-csharp/{dev,prod}.yaml
├── hello-nodejs/{dev,prod}.yaml
├── hello-python/{dev,prod}.yaml
└── hello-websocket/prod.yaml
```

---

## Platform components

| Component | Namespace | Purpose |
|-----------|-----------|---------|
| **Easy-Deploy Operator** | `easy-deploy-system` | Reconciles BirService CRs into Deployments, Services, HTTPRoutes, and Kaniko Jobs |
| **NGINX Gateway Fabric** | `nginx-gateway` | Ingress gateway with HTTP/HTTPS listeners, TLS termination via wildcard cert |
| **cert-manager** | `cert-manager` | Issues `*.easysolution.work` wildcard certificate via Let's Encrypt DNS-01 |
| **ExternalDNS** | `external-dns` | Creates Cloudflare DNS A records from HTTPRoute resources |
| **Local Registry** | `registry` | In-cluster Docker registry for Kaniko-built images (NodePort 30500) |
| **ArgoCD** | `argocd` | GitOps engine: syncs all platform and developer manifests |
| **Prometheus + Grafana** | `monitoring` | Cluster monitoring and dashboards |

---

## Validation pipeline

Tenant YAML is validated in four layers, fastest-first:

| Layer | Where | Catches |
|---|---|---|
| 1. IDE | VSCode reads `charts/birservice/values.schema.json` via `.vscode/settings.json` in BasePlate-Dev | Typos, wrong types, range violations — real-time underline |
| 2. Pre-commit | `pip install pre-commit && pre-commit install` in BasePlate-Dev | Schema + semantic rules before `git push` |
| 3. PR CI | `.github/workflows/validate.yml` runs `pre-commit run` | Same checks, sticky PR comment with line-anchored errors |
| 4. K8s API server | CRD OpenAPI schema | Last gate on `kubectl apply` |

Branch protection on `BasePlate-Dev/main` requires the `validate` check, so even GitHub web-editor edits are forced through a PR. Full architecture: [docs/user-guide/validation.md](docs/user-guide/validation.md).

---

## Build and rebuild

### Git repo build flow

When `repo:` contains a Git URL (GitHub, GitLab, Bitbucket):

1. Operator creates a **Kaniko Job** that clones the repo, builds the Dockerfile, and pushes to the local registry
2. After build succeeds, operator **inspects the image** to auto-detect the port
3. Operator creates Deployment using the built image from `registry.registry.svc.cluster.local:5000/<name>:<tag>`

### Tag-based rebuild

Change the `tag` field in YAML → ArgoCD updates BirService → operator detects `status.BuildTag != spec.Tag` → deletes old Job → creates new build.

```yaml
# Before
repo: https://github.com/user/myapp
tag: v1.0.0

# After (triggers rebuild)
repo: https://github.com/user/myapp
tag: v2.0.0
```

### Webhook-based rebuild

For automatic rebuilds when code is pushed (without changing YAML):

1. Configure GitHub webhook: `https://webhook.easysolution.work/webhook/github`
   - Content type: `application/json`
   - Events: `push`
2. When developer pushes to their repo, GitHub notifies the operator
3. Operator annotates the matching BirService with `deploy.easydeploy.io/rebuild: <timestamp>`
4. Reconciler detects the annotation change and triggers a new build

---

## One-time cluster setup

### Prerequisites

- Kubernetes cluster (kubeadm, k3s, etc.) with 1+ master and 1+ worker nodes
- A domain managed by Cloudflare (e.g. `easysolution.work`)
- Cloudflare API token with **Zone:DNS:Edit** and **Zone:Zone:Read** permissions

### Step 1: Install ArgoCD

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl -n argocd get pods  # wait until all pods are Running
```

Get admin password:

```bash
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo
```

### Step 2: Install prerequisite CRDs

```bash
git clone https://github.com/Murad-Suleymanov/BasePlate-Infra.git
cd BasePlate-Infra

# local-path StorageClass (for Prometheus persistent storage)
kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.30/deploy/local-path-storage.yaml

# Prometheus Operator CRDs
bash install-kube-prometheus-crds.sh

# Gateway API CRDs
bash install-gateway-api-crds.sh
```

### Step 3: Create Cloudflare secrets

```bash
kubectl create ns external-dns
kubectl create secret generic cloudflare-api-token \
  --namespace external-dns \
  --from-literal=cloudflare_api_token=YOUR_CLOUDFLARE_TOKEN

kubectl create ns cert-manager
kubectl create secret generic cloudflare-api-token \
  --namespace cert-manager \
  --from-literal=cloudflare_api_token=YOUR_CLOUDFLARE_TOKEN
```

### Step 4: Deploy platform

```bash
# 1. Infra root (BasePlate-Infra repo)
kubectl apply -f argocd/application-root.yaml

# 2. Platform root (BasePlate repo)
kubectl apply -f argocd/application-root.yaml
```

### Step 5: Configure worker node

On each **worker node**, configure containerd to pull from the insecure local registry and resolve Kubernetes service DNS:

```bash
# DNS: forward cluster.local queries to CoreDNS
mkdir -p /etc/systemd/resolved.conf.d
cat > /etc/systemd/resolved.conf.d/cluster-local.conf <<'EOF'
[Resolve]
DNS=10.96.0.10
Domains=~svc.cluster.local
EOF
systemctl restart systemd-resolved

# containerd: allow insecure local registry
mkdir -p /etc/containerd/certs.d/registry.registry.svc.cluster.local:5000
cat > /etc/containerd/certs.d/registry.registry.svc.cluster.local:5000/hosts.toml <<'EOF'
server = "http://registry.registry.svc.cluster.local:5000"

[host."http://registry.registry.svc.cluster.local:5000"]
  capabilities = ["pull", "resolve", "push"]
  skip_verify = true
EOF

sed -i 's|config_path = ""|config_path = "/etc/containerd/certs.d"|' /etc/containerd/config.toml
systemctl restart containerd
```

### Step 6: Verify

```bash
kubectl -n argocd get applications          # All Synced + Healthy?
kubectl -n easy-deploy-system get pods      # Operator running?
kubectl -n nginx-gateway get pods           # Gateway running?
kubectl -n external-dns get pods            # ExternalDNS running?
kubectl -n cert-manager get pods            # cert-manager running?
kubectl -n registry get pods                # Registry running?
kubectl -n monitoring get pods              # Prometheus + Grafana running?
```

---

## Configuration

The operator reads these environment variables from `manifests/operator/deployment.yaml`:

| Variable | Description | Default |
|----------|-------------|---------|
| `BASE_DOMAIN` | Auto-generated hostname suffix | `easysolution.work` |
| `TARGET_IP` | Public IP for DNS A records (worker node) | `116.203.203.121` |

---

## Troubleshooting

### Service not accessible

```bash
# Check BirService status
kubectl -n <namespace> get birservice

# Check build job (for Git repos)
kubectl -n <namespace> get jobs
kubectl -n <namespace> logs job/<name>-build-latest

# Check pod status
kubectl -n <namespace> get pods
kubectl -n <namespace> logs deploy/<name>-deploy

# Check HTTPRoute acceptance
kubectl -n <namespace> get httproute <name>-route -o jsonpath='{.status.parents[0].conditions}' | python3 -m json.tool

# Check DNS
dig <name>-<namespace>.easysolution.work @1.1.1.1

# Check operator logs
kubectl -n easy-deploy-system logs deployment/easy-deploy-operator --tail=30
```

### Build failed

```bash
# Check Kaniko job logs
kubectl -n <namespace> logs job/<name>-build-latest

# Common issues:
# - "reference not found" → wrong branch/tag, specify tag: in YAML
# - "MANIFEST_UNKNOWN" → Dockerfile error
# - ImagePullBackOff → worker node containerd not configured for insecure registry
```

### Port mismatch (502 Bad Gateway)

```bash
# Check what port the service uses
kubectl -n <namespace> get svc <name>-svc -o jsonpath='{.spec.ports}' ; echo

# Check what port the container actually listens on
kubectl -n <namespace> logs deploy/<name>-deploy --tail=5

# Fix: add explicit port in YAML
# port: 3000
```

---

## Access UIs

### ArgoCD

```bash
ssh -L 18080:localhost:18080 root@<MASTER_NODE_IP>
kubectl port-forward svc/argocd-server -n argocd 18080:443
```

Open https://localhost:18080 — Username: `admin`, password from Step 1.

### Grafana

Public URL: **https://grafana.easysolution.work**

```bash
# Get password
kubectl -n monitoring get secret monitoring-grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo
```

Username: `admin`.

---

## Tech stack

| Technology | Role |
|-----------|------|
| Go + controller-runtime | Custom operator |
| Kaniko | In-cluster container image builds |
| NGINX Gateway Fabric | Gateway API ingress controller |
| cert-manager + Let's Encrypt | Wildcard TLS certificates (DNS-01) |
| ExternalDNS + Cloudflare | Automatic DNS record management |
| ArgoCD | GitOps continuous deployment |
| Prometheus + Grafana | Monitoring and dashboards |
| Docker Registry v2 | Local container image storage |

## License

MIT
