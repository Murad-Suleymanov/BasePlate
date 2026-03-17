## ArgoCD — BasePlate (platform)

Platform öz App of Apps-ına malikdir.

### Fayllar

| Fayl | Məqsəd |
|------|--------|
| `application-root.yaml` | root-platform — manifests/ + argocd/ (özünü izləyir) |

### Bootstrap

```bash
kubectl apply -f argocd/application-root.yaml
```

Root `manifests/*/-application.yaml` + `argocd/*.yaml` tapır və Application-ları yaradır.

Platform Application: `manifests/platform/easy-deploy-platform-application.yaml`
