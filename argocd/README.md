## ArgoCD flow (developer manual yoxdur)

Developer yalnız bu faylları əlavə edib Git-ə push edir:

- `tenants/<tenant>/simple-yaml/<service>.yaml`

ArgoCD `ApplicationSet` həmin faylları avtomatik tapır və `charts/birservice` Helm chart ilə `BirService` CR yaradır.
Sonra klasterdə işləyən Easy-Deploy operator `BirService`-dən `Deployment` və `Service` yaradır.

### 1) Tələblər (bir dəfəlik)

- ArgoCD klasterdə qurulub
- Easy-Deploy CRD + operator ArgoCD ilə də quraşdırıla bilər (aşağıda)

### 2) ApplicationSet-i apply edin (bir dəfəlik)

`argocd/applicationset-birservices.yaml` faylında `<YOUR_REPO_URL>` hissəsini repo URL ilə dəyişin, sonra:

```bash
kubectl apply -f argocd/applicationset-birservices.yaml -n argocd
```

### 2.1) Platform app (CRD + operator) - ArgoCD ilə

`argocd/application-platform.yaml` faylında `<YOUR_REPO_URL>` və `manifests/operator/deployment.yaml` içində `<YOUR_REGISTRY>` hissələrini doldurun.

Sonra:

```bash
kubectl apply -f argocd/application-platform.yaml -n argocd
```

### 3) Developer nümunəsi

`tenants/acme/simple-yaml/hello.yaml` artıq mövcuddur.
Yalnız `name`, `namespace`, `repo` və `tag` dəyişib commit edin.

