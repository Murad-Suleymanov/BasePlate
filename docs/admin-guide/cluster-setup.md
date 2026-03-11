# Cluster Setup

Detailed guide for setting up the Easy-Deploy platform from scratch.

## Prerequisites

| Requirement | Details |
|------------|---------|
| Kubernetes cluster | kubeadm, k3s, or any conformant distribution (v1.26+) |
| Nodes | 1+ master, 1+ worker |
| kubectl | Configured with cluster-admin access |
| Domain | Managed by Cloudflare |
| Cloudflare token | Zone:DNS:Edit + Zone:Zone:Read permissions |

## Step 1: Install ArgoCD

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
```

Wait for all ArgoCD pods to be running:

```bash
kubectl -n argocd wait --for=condition=Ready pods --all --timeout=300s
```

Retrieve admin credentials:

```bash
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

## Step 2: Install CRDs

Clone the infrastructure repository and install CRDs:

```bash
git clone https://github.com/Murad-Suleymanov/BasePlate-Infra.git
cd BasePlate-Infra
```

=== "Gateway API"

    ```bash
    bash install-gateway-api-crds.sh
    ```

    This installs the `Gateway`, `HTTPRoute`, `GatewayClass`, and related CRDs.

=== "Prometheus Operator"

    ```bash
    bash install-kube-prometheus-crds.sh
    ```

    This installs `ServiceMonitor`, `PodMonitor`, `PrometheusRule`, and related CRDs.

=== "Local Path Provisioner"

    ```bash
    kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.30/deploy/local-path-storage.yaml
    ```

    Provides a `local-path` StorageClass for Prometheus persistent volumes.

## Step 3: Create Secrets

### Cloudflare API Token

Create the same secret in two namespaces — one for ExternalDNS and one for cert-manager:

```bash
# Create namespaces
kubectl create ns external-dns
kubectl create ns cert-manager

# Create secrets
kubectl create secret generic cloudflare-api-token \
  --namespace external-dns \
  --from-literal=cloudflare_api_token=YOUR_TOKEN

kubectl create secret generic cloudflare-api-token \
  --namespace cert-manager \
  --from-literal=cloudflare_api_token=YOUR_TOKEN
```

!!! danger "Security"
    The Cloudflare API token grants DNS edit access. Keep it secret and rotate it periodically.

## Step 4: Deploy Platform Components

From the `BasePlate-Infra` repo (cloned in Step 2), apply all ArgoCD applications:

```bash
cd BasePlate-Infra
kubectl apply -n argocd -f argocd/
```

### What Each Application Deploys

| Application | Namespace | Components |
|------------|-----------|------------|
| `application-platform` | `easy-deploy-system` | BirService CRD + Operator (from BasePlate repo) |
| `application-infra` | Various | Gateway, Registry, Webhook (from BasePlate-Infra repo) |
| `applicationset-birservices` | Per-tenant | Auto-discovers developer YAMLs |
| `application-gateway` | `nginx-gateway` | NGINX Gateway Fabric |
| `application-cert-manager` | `cert-manager` | cert-manager controller + webhooks |
| `application-monitoring` | `monitoring` | Prometheus + Grafana |
| `application-external-dns` | `external-dns` | ExternalDNS controller |

## Step 5: Configure Worker Nodes

**Critical step.** Each worker node needs DNS forwarding and containerd configuration. See [Worker Node Configuration](worker-node-config.md).

## Step 6: Verify Installation

### ArgoCD Applications

```bash
kubectl -n argocd get applications
```

All applications should show `Synced` and `Healthy`:

```
NAME                          SYNC STATUS   HEALTH STATUS
easy-deploy-platform          Synced        Healthy
easy-deploy-gateway           Synced        Healthy
easy-deploy-cert-manager      Synced        Healthy
easy-deploy-monitoring        Synced        Healthy
easy-deploy-external-dns      Synced        Healthy
```

### Platform Pods

```bash
kubectl -n easy-deploy-system get pods    # Operator
kubectl -n nginx-gateway get pods         # Gateway
kubectl -n external-dns get pods          # ExternalDNS
kubectl -n cert-manager get pods          # cert-manager
kubectl -n registry get pods              # Registry
kubectl -n monitoring get pods            # Prometheus + Grafana
```

### TLS Certificate

```bash
kubectl -n nginx-gateway get certificate wildcard-easysolution
```

Should show `True` in the READY column. If not, check cert-manager logs:

```bash
kubectl -n cert-manager logs deployment/cert-manager --tail=30
```

## Updating the Platform

The platform is managed by ArgoCD. To update:

1. Push changes to `BasePlate` (operator/CRD) or `BasePlate-Infra` (infra manifests/ArgoCD apps)
2. ArgoCD detects the changes and syncs automatically

For the operator image:

1. Push code changes to `main` branch
2. GitHub Actions builds and pushes a new image to GHCR
3. The operator Deployment uses `imagePullPolicy: Always`, so a pod restart picks up the new image:

```bash
kubectl -n easy-deploy-system rollout restart deployment/easy-deploy-operator
```
