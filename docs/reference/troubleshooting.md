# Troubleshooting

Common issues and their solutions when working with Easy-Deploy.

## Service Not Accessible

### Symptoms

- `DNS_PROBE_FINISHED_NXDOMAIN` in browser
- `Connection refused` or `Connection timed out`
- `502 Bad Gateway`

### Diagnostic Steps

```bash
# 1. Check BirService exists and has status
kubectl -n <namespace> get birservice <name>

# 2. Check pods are running
kubectl -n <namespace> get pods -l app.kubernetes.io/name=<name>

# 3. Check HTTPRoute is accepted
kubectl -n <namespace> get httproute <name>-route -o jsonpath='{.status.parents[0].conditions}' | python3 -m json.tool

# 4. Check DNS resolution
dig <name>-<namespace>.easysolution.work @1.1.1.1

# 5. Check operator logs
kubectl -n easy-deploy-system logs deployment/easy-deploy-operator --tail=50
```

### DNS Not Resolving

| Cause | Solution |
|-------|----------|
| ExternalDNS hasn't created the record yet | Wait 2–3 minutes |
| Local DNS cache | Flush DNS: `ipconfig /flushdns` (Windows) or `sudo dscacheutil -flushcache` (Mac) |
| ISP negative caching | Use `1.1.1.1` or `8.8.8.8` as DNS resolver |
| ExternalDNS error | Check: `kubectl -n external-dns logs deployment/external-dns` |

### 502 Bad Gateway

This usually means the NGINX Gateway can route the request but the backend is unhealthy.

| Cause | Solution |
|-------|----------|
| Wrong port | Specify `port:` explicitly in YAML |
| Pod crashing | Check: `kubectl -n <ns> logs deploy/<name>-deploy` |
| Pod not ready | Check: `kubectl -n <ns> describe pod <pod-name>` |

### Connection Timed Out

| Cause | Solution |
|-------|----------|
| Gateway not running | Check: `kubectl -n nginx-gateway get pods` |
| Firewall blocking 80/443 | Open ports in your cloud provider's firewall |
| DNS points to wrong IP | Verify: `dig <hostname> +short` should match `TARGET_IP` |

---

## Build Failed

### Symptoms

- BirService shows `buildStatus: Failed`
- No pods created
- Kaniko job in `Error` state

### Diagnostic Steps

```bash
# Check build job status
kubectl -n <namespace> get jobs -l deploy.easydeploy.io/purpose=build

# Read Kaniko build logs
kubectl -n <namespace> logs job/<name>-build-<tag>
```

### Common Kaniko Errors

| Error | Cause | Solution |
|-------|-------|---------|
| `error resolving source context: reference not found` | Wrong branch/tag | Check `tag:` field, or omit to use default branch |
| `MANIFEST_UNKNOWN` | Invalid Dockerfile or registry issue | Check Dockerfile syntax |
| `error building stage` | Dockerfile build error | Fix the Dockerfile in the source repo |
| `Unauthorized` | Private repository | Currently only public repos are supported |
| `dial tcp: i/o timeout` | Network issue cloning repo | Check cluster DNS and outbound connectivity |

### Rebuilding After Failure

Fix the issue (e.g., fix the Dockerfile, correct the tag) and either:

1. Change the `tag` field in the YAML
2. Add/update the rebuild annotation:
   ```bash
   kubectl -n <ns> annotate bs <name> deploy.easydeploy.io/rebuild=$(date +%s) --overwrite
   ```

---

## ImagePullBackOff

### Symptoms

- Pods stuck in `ImagePullBackOff` after a successful Kaniko build
- `describe pod` shows `failed to resolve reference` or `server gave HTTP response to HTTPS client`

### Cause

The worker node's containerd runtime cannot pull from the local insecure registry.

### Solution

Configure the worker node. See [Worker Node Configuration](../admin-guide/worker-node-config.md).

```bash
# Quick check on worker node
resolvectl query registry.registry.svc.cluster.local
# Should resolve to the registry pod's ClusterIP

cat /etc/containerd/certs.d/registry.registry.svc.cluster.local:5000/hosts.toml
# Should exist with skip_verify = true
```

---

## Port Mismatch

### Symptoms

- `502 Bad Gateway` after successful build and deployment
- Pod is running but returning errors

### Diagnostic Steps

```bash
# Check what port the service is using
kubectl -n <ns> get svc <name>-svc -o jsonpath='{.spec.ports[0].port}'; echo

# Check what port the container actually listens on
kubectl -n <ns> logs deploy/<name>-deploy --tail=10
```

### Solution

If auto-detection picked the wrong port (or didn't find one), set it explicitly:

```yaml
repo: https://github.com/your-org/your-app
port: 3000
```

---

## ArgoCD Issues

### Application Not Syncing

```bash
# Check application status
kubectl -n argocd get application <name>

# Check application details
kubectl -n argocd describe application <name>

# Force sync
kubectl -n argocd patch application <name> --type merge -p '{"operation":{"initiatedBy":{"username":"admin"},"sync":{"prune":true}}}'
```

### ApplicationSet Not Discovering YAML

```bash
# Check ApplicationSet status
kubectl -n argocd get applicationset easy-deploy-birservices -o yaml | grep -A5 status

# Verify file path matches the pattern
# Must be: tenants/*/simple-yaml/*.yaml
```

---

## Certificate Issues

### Certificate Not Ready

```bash
# Check certificate status
kubectl -n nginx-gateway get certificate wildcard-easysolution

# Check ACME challenges
kubectl get challenges -A

# Check cert-manager logs
kubectl -n cert-manager logs deployment/cert-manager --tail=50
```

| Issue | Cause | Solution |
|-------|-------|---------|
| `Pending` indefinitely | Cloudflare token wrong | Recreate the secret with correct token |
| `no configured challenge solvers` | Selector mismatch | Use `dnsZones` in ClusterIssuer |
| `Timeout` | DNS propagation | Wait and retry |

---

## Webhook Not Triggering Rebuild

```bash
# Check webhook server is running
kubectl -n easy-deploy-system logs deployment/easy-deploy-operator --tail=20 | grep webhook

# Check webhook route exists
kubectl -n easy-deploy-system get httproute webhook-route

# Test webhook manually
curl -X POST https://webhook.easysolution.work/webhook/github \
  -H 'Content-Type: application/json' \
  -d '{"ref":"refs/heads/main","repository":{"html_url":"https://github.com/your-org/your-app"}}'
```

| Issue | Cause | Solution |
|-------|-------|---------|
| `404` | Route not configured | Check webhook HTTPRoute and Service |
| `{"triggered":0}` | No matching BirService | Verify repo URL matches spec.repo exactly |
| GitHub shows delivery failure | DNS/network issue | Check webhook route DNS resolution |

---

## Operator Not Running

```bash
# Check operator pod
kubectl -n easy-deploy-system get pods

# Check operator logs
kubectl -n easy-deploy-system logs deployment/easy-deploy-operator --tail=50

# Check events
kubectl -n easy-deploy-system get events --sort-by=.lastTimestamp
```

| Issue | Cause | Solution |
|-------|-------|---------|
| `CrashLoopBackOff` | Missing CRD or RBAC | Ensure CRD is installed, check ClusterRole |
| `ImagePullBackOff` | GHCR auth issue | Check image exists at `ghcr.io/murad-suleymanov/easy-deploy-operator:main` |
| Pod not created | Namespace missing | `kubectl create ns easy-deploy-system` |
