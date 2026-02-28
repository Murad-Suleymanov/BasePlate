## ArgoCD flow (developer manual yoxdur)

Developer yalnız bu faylları əlavə edib Git-ə push edir:

- `tenants/<tenant>/simple-yaml/<service>.yaml`

ArgoCD `ApplicationSet` həmin faylları avtomatik tapır və platform repo-dakı `charts/birservice` Helm chart ilə `BirService` CR yaradır.
Sonra klasterdə işləyən Easy-Deploy operator `BirService`-dən `Deployment` və `Service` yaradır.

### 1) Tələblər (bir dəfəlik)

- ArgoCD klasterdə qurulub
- Easy-Deploy CRD + operator ArgoCD ilə də quraşdırıla bilər (aşağıda)

### 2) ApplicationSet-i apply edin (bir dəfəlik)

`argocd/applicationset-birservices.yaml` artıq developer repo-ya (`BasePlate-Dev`) işarə edir. Sonra:

```bash
kubectl apply -f argocd/applicationset-birservices.yaml -n argocd
```

### 2.1) Platform app (CRD + operator) - ArgoCD ilə

`manifests/operator/deployment.yaml` içində operator image (GHCR) public olmalıdır (və ya `imagePullSecret` lazımdır).

Sonra:

```bash
kubectl apply -f argocd/application-platform.yaml -n argocd
```

### 3) Developer nümunəsi

Developer repo-da `tenants/<tenant>/simple-yaml/<service>.yaml` əlavə edin və push edin.

