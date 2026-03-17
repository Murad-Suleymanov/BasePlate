## ArgoCD — BasePlate (platform)

Platform öz App of Apps-ına malikdir.

### Fayllar

| Fayl | Məqsəd |
|------|--------|
| `application-root.yaml` | root-platform — platform app-ləri yaradır |
| `application-platform.yaml` | easy-deploy-platform — CRD + Operator |

### Bootstrap

```bash
kubectl apply -f argocd/application-root.yaml
```

Root `argocd/*.yaml` (application-root istisna) tapır və Application-ları yaradır.
