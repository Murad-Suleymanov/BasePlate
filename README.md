# Easy-Deploy

Easy-Deploy lets developers define a service in a **simple YAML file**, commit it to Git, and have it automatically deployed to Kubernetes with external DNS access.

## How it works

1. Developer commits a simple YAML to the catalog repo (`BasePlate-Dev`)
2. ArgoCD `ApplicationSet` discovers it and creates a `BirService` Custom Resource
3. The Easy-Deploy operator creates: `Deployment` + `Service` + `HTTPRoute`
4. ExternalDNS reads the HTTPRoute and creates a Cloudflare DNS record
5. Service is accessible at `<name>.easysolution.work`

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

## Simple YAML example (developer writes only this)

In `BasePlate-Dev` repo: `tenants/dev/simple-yaml/echo.yaml`:

```yaml
name: echo
namespace: dev
image: ealen/echo-server:0.9.2
port: 8080
replicas: 1
```

Result: `echo.easysolution.work` automatically becomes accessible.

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

Then create the secret:

```bash
kubectl create ns external-dns
kubectl create secret generic cloudflare-api-token \
  --namespace external-dns \
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

### Step 8: Port redirect (worker node)

On each worker node, redirect standard HTTP/HTTPS ports to NodePort range. Run **once**, persists across reboots:

```bash
iptables -t nat -A PREROUTING -p tcp --dport 80 ! -s 192.168.0.0/16 -j REDIRECT --to-port 30080
iptables -t nat -A PREROUTING -p tcp --dport 443 ! -s 192.168.0.0/16 -j REDIRECT --to-port 30443
apt install -y iptables-persistent
netfilter-persistent save
```

The `! -s 192.168.0.0/16` excludes internal pod traffic from being redirected (only external traffic is affected).

### Step 9: Verify

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
name: myapp
namespace: dev
image: myregistry/myapp:1.0.0
port: 8080
```

Within ~2 minutes: `myapp.easysolution.work` is live.

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
