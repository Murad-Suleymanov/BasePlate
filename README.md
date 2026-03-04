# Easy-Deploy

Easy-Deploy lets developers define a service in a **simple YAML file**, commit it to Git, and have it automatically deployed to Kubernetes with external DNS access.

## How it works

1. Developer commits a simple YAML to the catalog repo (`BasePlate-Dev`)
2. ArgoCD `ApplicationSet` discovers it and creates a `BirService` Custom Resource
3. The Easy-Deploy operator:
   - **Ready image?** Creates `Deployment` + `Service` + `HTTPRoute`
   - **Git repo?** Runs a Kaniko build Job → pushes to local registry → then deploys
4. ExternalDNS reads the HTTPRoute and creates a Cloudflare DNS record
5. Service is accessible at `https://<name>-<namespace>.easysolution.work`

## Repository structure

```
BasePlate/                          # Platform repo
├── api/v1alpha1/                   # Go types for BirService CRD
├── cmd/operator/                   # Operator entrypoint
├── cmd/easydeployctl/              # CLI tool (optional)
├── internal/controller/            # Operator reconciliation logic
├── charts/birservice/              # Helm chart (simple YAML → BirService CR)
├── config/crd/                     # CRD definition (source of truth)
├── manifests/                      # Platform manifests (synced by ArgoCD)
│   ├── crd/                        # BirService CRD
│   ├── operator/                   # Operator deployment, RBAC
│   ├── gateway/                    # Gateway, ReferenceGrants, ClusterIssuer
│   └── registry/                   # Local container registry
├── argocd/                         # ArgoCD Application definitions
│   ├── application-platform.yaml   # Platform manifests
│   ├── applicationset-birservices.yaml  # Auto-discover developer YAMLs
│   ├── application-gateway.yaml    # NGINX Gateway Fabric
│   ├── application-cert-manager.yaml    # cert-manager (TLS)
│   ├── application-monitoring.yaml # kube-prometheus-stack
│   └── application-external-dns.yaml   # ExternalDNS (Cloudflare)
├── Dockerfile                      # Operator container image
└── .github/workflows/              # CI/CD

BasePlate-Dev/                      # Developer catalog repo
└── tenants/<tenant>/simple-yaml/
    └── <service>.yaml
```

## Simple YAML examples (developer writes only this)

In `BasePlate-Dev` repo: `tenants/dev/simple-yaml/<service>.yaml`

**Deploy a ready image (1 line):**

```yaml
image: ealen/echo-server:0.9.2
```

**Build from Git repo (1 line):**

```yaml
repo: https://github.com/user/myapp
```

**With optional overrides:**

```yaml
repo: https://github.com/user/myapp
tag: v2.0.0
port: 3000
replicas: 3
dockerfile: build/Dockerfile
```

Everything else is automatic:
- `name` = filename (`myapp.yaml` → `myapp`)
- `namespace` = folder (`tenants/dev/` → `dev`)
- `hostname` = `<name>-<namespace>.easysolution.work`
- `port` = default 8080
- `replicas` = default 1
- `tag` = default `latest` (images) or `main` (git repos)

---

## One-time cluster setup

These steps are performed **once** when setting up a new cluster. After this, no manual intervention is needed for new services.

### Step 1: Install Kubernetes

Set up a Kubernetes cluster (kubeadm, k3s, etc.) with at least 1 master + 1 worker node.

### Step 2: Install ArgoCD

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
```

Wait for pods to be ready:

```bash
kubectl -n argocd get pods
```

Get ArgoCD admin password:

```bash
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo
```

### Step 3: Install local-path StorageClass

Required for Prometheus/Alertmanager persistent storage:

```bash
kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.30/deploy/local-path-storage.yaml
```

### Step 4: Install Prometheus Operator CRDs

These CRDs are too large for standard `kubectl apply`. Install manually:

```bash
bash install-kube-prometheus-crds.sh
```

### Step 5: Install Gateway API CRDs

Required before NGINX Gateway Fabric:

```bash
bash install-gateway-api-crds.sh
```

### Step 6: Create Cloudflare API token secret

Create a Cloudflare API token at https://dash.cloudflare.com/profile/api-tokens with permissions:
- **Zone:DNS:Edit**
- **Zone:Zone:Read**

Then create the secrets (ExternalDNS and cert-manager both need it):

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

### Step 7: Clone repo and apply ArgoCD applications

```bash
git clone https://github.com/Murad-Suleymanov/BasePlate.git
cd BasePlate

# Platform (CRD, operator, gateway, registry)
kubectl apply -n argocd -f argocd/application-platform.yaml

# Developer service auto-discovery
kubectl apply -n argocd -f argocd/applicationset-birservices.yaml

# NGINX Gateway Fabric
kubectl apply -n argocd -f argocd/application-gateway.yaml

# cert-manager (TLS certificates)
kubectl apply -n argocd -f argocd/application-cert-manager.yaml

# Monitoring (Prometheus + Grafana)
kubectl apply -n argocd -f argocd/application-monitoring.yaml

# ExternalDNS (Cloudflare automatic DNS)
kubectl apply -n argocd -f argocd/application-external-dns.yaml
```

### Step 8: Verify

```bash
# All ArgoCD apps synced?
kubectl -n argocd get applications

# Operator running?
kubectl -n easy-deploy-system get pods

# Gateway running?
kubectl -n nginx-gateway get pods

# ExternalDNS running?
kubectl -n external-dns get pods

# Monitoring running?
kubectl -n monitoring get pods
```

---

## After setup: adding a new service

Developer simply commits a YAML to `BasePlate-Dev` repo. No other steps needed:

```yaml
image: nginxdemos/hello:plain-text
```

Within ~2 minutes: `https://<name>-<namespace>.easysolution.work` is live with HTTPS.

For Git repos with a Dockerfile:

```yaml
repo: https://github.com/user/myapp
```

Within ~3 minutes: Kaniko builds the image, pushes to local registry, and deploys.

## Configuration

The operator reads these environment variables from `manifests/operator/deployment.yaml`:

| Variable | Description | Default |
|---|---|---|
| `BASE_DOMAIN` | Auto-generated hostname suffix | `easysolution.work` |
| `TARGET_IP` | IP for DNS A records (worker node public IP) | `116.203.203.121` |

## Access ArgoCD UI

From your local machine, SSH tunnel + port-forward:

```bash
ssh -L 18080:localhost:18080 root@<MASTER_NODE_IP>
# Then on the VPS:
kubectl port-forward svc/argocd-server -n argocd 18080:443
```

Open https://localhost:18080 in your browser. Username: `admin`, password from Step 2.

## Access Grafana

```bash
# Get password
kubectl -n monitoring get secret monitoring-grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo

# Port-forward
kubectl -n monitoring port-forward svc/monitoring-grafana 13000:80
```

Open http://localhost:13000. Username: `admin`.
