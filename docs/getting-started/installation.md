# Installation

This guide covers the one-time setup of the Easy-Deploy platform on a Kubernetes cluster.

## Step 1: Install ArgoCD

ArgoCD is the GitOps engine that syncs all platform and developer manifests.

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
```

Wait for all pods to become ready:

```bash
kubectl -n argocd get pods -w
```

Retrieve the initial admin password:

```bash
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

!!! tip
    Save this password — you'll need it to access the ArgoCD UI later.

## Step 2: Install Prerequisite CRDs

Clone the infrastructure repository and install CRDs:

```bash
git clone https://github.com/Murad-Suleymanov/BasePlate-Infra.git
cd BasePlate-Infra

# Local-path StorageClass (for Prometheus persistent storage)
kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.30/deploy/local-path-storage.yaml

# Prometheus Operator CRDs
bash install-kube-prometheus-crds.sh

# Gateway API CRDs
bash install-gateway-api-crds.sh
```

## Step 3: Create Cloudflare Secrets

Both ExternalDNS and cert-manager need a Cloudflare API token to manage DNS records and solve DNS-01 challenges.

```bash
# ExternalDNS namespace
kubectl create ns external-dns
kubectl create secret generic cloudflare-api-token \
  --namespace external-dns \
  --from-literal=cloudflare_api_token=YOUR_CLOUDFLARE_TOKEN

# cert-manager namespace
kubectl create ns cert-manager
kubectl create secret generic cloudflare-api-token \
  --namespace cert-manager \
  --from-literal=cloudflare_api_token=YOUR_CLOUDFLARE_TOKEN
```

!!! warning "Token Permissions"
    Your Cloudflare API token must have these permissions:

    - **Zone:DNS:Edit** — to create and update DNS records
    - **Zone:Zone:Read** — to list zones

## Step 4: Deploy the Platform

Apply both roots (one-time bootstrap):

```bash
# Infra: gateway, monitoring, cert-manager, dns, registry, argocd-config
cd BasePlate-Infra
kubectl apply -f argocd/application-root.yaml

# Platform: CRD + Operator
cd ../BasePlate
kubectl apply -f argocd/application-root.yaml
```

| Root | Applications |
|------|--------------|
| **root-infra** (BasePlate-Infra) | gateway, nginx-gateway-fabric, monitoring, kube-prometheus-stack, metrics-server, cert-manager, external-dns, registry, argocd-config |
| **root-platform** (BasePlate) | easy-deploy-platform (CRD + Operator) |

## Step 5: Configure Worker Nodes

Each worker node needs two configurations for the in-cluster build pipeline to work. See the [Worker Node Configuration](../admin-guide/worker-node-config.md) guide for details.

**Summary of changes on each worker:**

1. **DNS forwarding** — so containerd can resolve `*.svc.cluster.local` addresses
2. **Insecure registry** — so containerd can pull images from the local registry

## Step 6: Verify the Installation

```bash
# All ArgoCD applications should be Synced + Healthy
kubectl -n argocd get applications

# All platform pods should be Running
kubectl -n easy-deploy-system get pods    # Operator
kubectl -n nginx-gateway get pods         # Gateway
kubectl -n external-dns get pods          # ExternalDNS
kubectl -n cert-manager get pods          # cert-manager
kubectl -n registry get pods              # Local registry
kubectl -n monitoring get pods            # Prometheus + Grafana
```

!!! success "Installation Complete"
    If all pods are running, proceed to the [Quick Start](quickstart.md) to deploy your first application.

## Accessing Platform UIs

### ArgoCD Dashboard

```bash
# From your local machine (SSH tunnel)
ssh -L 18080:localhost:18080 root@YOUR_MASTER_NODE_IP

# On the master node
kubectl port-forward svc/argocd-server -n argocd 18080:443
```

Open [https://localhost:18080](https://localhost:18080) — Username: `admin`, password from Step 1.

### Grafana Dashboard

Public URL: [https://grafana.easysolution.work](https://grafana.easysolution.work)

```bash
# Get the password
kubectl -n monitoring get secret monitoring-grafana \
  -o jsonpath='{.data.admin-password}' | base64 -d; echo
```

Username: `admin`.
