# Easy-Deploy (MVP)

Easy-Deploy lets developers define a service in a **simple YAML file**, commit it to Git, and let Kubernetes reconcile it automatically.

## How it works

1) Developer commits a file like `tenants/<tenant>/simple-yaml/<service>.yaml`
2) Argo CD `ApplicationSet` discovers those files and renders a `BirService` Custom Resource using a Helm chart
3) The Easy-Deploy operator watches `BirService` and creates/updates:
   - `Deployment`
   - `Service`

## Repository structure (mono-repo)

- **Developer input (only thing developers change)**:
  - `tenants/<tenant>/simple-yaml/<service>.yaml`
- **Platform**:
  - CRD: `config/crd/birservice_crd.yaml`
  - Operator manifests: `manifests/`
  - Argo CD apps: `argocd/`
- **Renderer**:
  - Helm chart: `charts/birservice/`

Note: `tenants/<tenant>/cr/` is optional and only useful for local/debug. With Argo CD you can keep just `simple-yaml/`.

## Simple YAML example

`tenants/acme/simple-yaml/hello.yaml`:

```yaml
name: hello
namespace: acme
repo: ghcr.io/acme/hello
tag: "1.0.0"
port: 8080
replicas: 1
```

## Install Argo CD (one-time, in-cluster)

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl get pods -n argocd
```

If your `kubectl` runs on a VPS and you want the UI on your local machine, use an SSH tunnel + port-forward (see `argocd/README.md`).

## Install Easy-Deploy platform (CRD + operator) via Argo CD

1) Build and push the operator image from this repo:

```bash
docker build -t <YOUR_REGISTRY>/easy-deploy-operator:0.1.0 .
docker push <YOUR_REGISTRY>/easy-deploy-operator:0.1.0
```

2) Update these placeholders:
- `manifests/operator/deployment.yaml`: set the `image:` to your registry
- `argocd/application-platform.yaml`: set `<YOUR_REPO_URL>`

3) Apply the Argo CD Application:

```bash
kubectl apply -f argocd/application-platform.yaml -n argocd
```

## Auto-deploy services from `simple-yaml/` via Argo CD

1) Update `argocd/applicationset-birservices.yaml` and replace `<YOUR_REPO_URL>` (both occurrences).
2) Apply it:

```bash
kubectl apply -f argocd/applicationset-birservices.yaml -n argocd
```

After this, developers only commit `tenants/<tenant>/simple-yaml/*.yaml`. Argo CD renders `BirService` and the operator creates `Deployment`/`Service`.

## Local debug (optional)

Generate a `BirService` YAML from a simple YAML file:

```powershell
go run .\cmd\easydeployctl\main.go generate `
  -f .\tenants\acme\simple-yaml\hello.yaml `
  -o .\tenants\acme\cr\hello.yaml
```

Run the operator locally (uses your kubeconfig):

```powershell
go run .\cmd\operator\main.go
```

## MVP notes

- The operator currently reconciles `BirService` into `Deployment` + `Service` only.
- `BirService` status includes `availableReplicas` and the CRD has printer columns (repo/image/port/availableReplicas).

