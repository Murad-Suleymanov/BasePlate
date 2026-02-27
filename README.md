# Easy-Deploy (MVP)

Developer sadə YAML yazır və Git-ə commit edir → ArgoCD `ApplicationSet` həmin YAML-ları tapıb `BirService` CR deploy edir → operator `BirService`-i reconcile edib `Deployment` + `Service` yaradır.

## Repo strukturu (tenant izolasiya)

- `tenants/<tenant>/simple-yaml/<service>.yaml` (developer yazır)
- ArgoCD ilə əsasən yalnız `simple-yaml/` saxlayırsınız; `cr/` qovluğu istəsəniz lokal/debug üçün qala bilər.

## Sadə YAML nümunəsi

`tenants/acme/simple-yaml/hello.yaml`:

```yaml
name: hello
namespace: acme
repo: ghcr.io/acme/hello
tag: "1.0.0"
port: 8080
replicas: 1
```

## ArgoCD (manual yoxdur, bir dəfəlik setup)

- `argocd/applicationset-birservices.yaml` içində `<YOUR_REPO_URL>` hissəsini repo URL ilə dəyişin
- `kubectl apply -f .\\argocd\\applicationset-birservices.yaml -n argocd`

Detallar: `argocd/README.md`

## Lokal generasiya (debug üçün)

```powershell
go run .\cmd\easydeployctl\main.go generate `
  -f .\tenants\acme\simple-yaml\hello.yaml `
  -o .\tenants\acme\cr\hello.yaml
```

## CRD + Operator (lokal run)

1) CRD-ni apply edin:

```powershell
kubectl apply -f .\config\crd\birservice_crd.yaml
```

2) Operatoru lokal işə salın (kubeconfig ilə):

```powershell
go run .\cmd\operator\main.go
```

3) Generasiya olunmuş CR-ni apply edin:

```powershell
kubectl apply -f .\tenants\acme\cr\hello.yaml
```

## Nəticə (MVP)

- Operator `BirService` → `Deployment` + `Service` yaradır.
- Status-a `availableReplicas` yazır.
- CRD-də printer columns var: repo/image/port/availableReplicas.

