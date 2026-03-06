# Worker Node Configuration

For the in-cluster build pipeline to work, each worker node needs two configurations: DNS forwarding for Kubernetes service names, and insecure registry access for the local container registry.

!!! warning "Required"
    Without these configurations, pods will fail with `ImagePullBackOff` after Kaniko builds, because containerd cannot resolve the registry's cluster DNS name or pull from an HTTP registry.

## Why This Is Needed

The build pipeline stores images in the local registry at `registry.registry.svc.cluster.local:5000`. When kubelet instructs containerd to pull this image:

1. **containerd runs on the host**, not inside a pod — so it doesn't use CoreDNS by default
2. **containerd doesn't trust HTTP registries** by default — it expects HTTPS

Both need to be configured at the host level.

## Configuration Steps

SSH into each worker node as root.

### 1. DNS Forwarding to CoreDNS

Configure `systemd-resolved` to forward `.svc.cluster.local` queries to CoreDNS:

```bash
mkdir -p /etc/systemd/resolved.conf.d

cat > /etc/systemd/resolved.conf.d/cluster-local.conf <<'EOF'
[Resolve]
DNS=10.96.0.10
Domains=~svc.cluster.local
EOF

systemctl restart systemd-resolved
```

!!! note "CoreDNS IP"
    `10.96.0.10` is the default CoreDNS ClusterIP. Verify yours with:
    ```bash
    kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}'
    ```

**Verify:**

```bash
resolvectl query registry.registry.svc.cluster.local
```

Should return the registry service's ClusterIP.

### 2. Insecure Registry for containerd

Configure containerd to allow pulling from the HTTP registry:

```bash
mkdir -p /etc/containerd/certs.d/registry.registry.svc.cluster.local:5000

cat > /etc/containerd/certs.d/registry.registry.svc.cluster.local:5000/hosts.toml <<'EOF'
server = "http://registry.registry.svc.cluster.local:5000"

[host."http://registry.registry.svc.cluster.local:5000"]
  capabilities = ["pull", "resolve", "push"]
  skip_verify = true
EOF
```

Enable the certificate directory in containerd's config:

```bash
sed -i 's|config_path = ""|config_path = "/etc/containerd/certs.d"|' /etc/containerd/config.toml

systemctl restart containerd
```

!!! warning
    If `config_path` is already set to a different value, make sure `/etc/containerd/certs.d` is included in the path.

**Verify:**

```bash
# Check containerd can resolve and pull from the registry
crictl pull registry.registry.svc.cluster.local:5000/welcome:latest
```

## Verification Script

Run this on each worker node to verify both configurations:

```bash
echo "=== DNS Resolution ==="
resolvectl query registry.registry.svc.cluster.local && echo "OK" || echo "FAILED"

echo ""
echo "=== containerd Config ==="
grep -q 'config_path.*certs.d' /etc/containerd/config.toml && echo "OK" || echo "FAILED"

echo ""
echo "=== hosts.toml ==="
cat /etc/containerd/certs.d/registry.registry.svc.cluster.local:5000/hosts.toml 2>/dev/null || echo "MISSING"
```

## Troubleshooting

### Pods stuck in ImagePullBackOff

```bash
kubectl -n <namespace> describe pod <pod-name>
```

Common errors:

| Error | Cause | Fix |
|-------|-------|-----|
| `failed to resolve reference` | DNS not configured | Configure systemd-resolved |
| `http: server gave HTTP response to HTTPS client` | Insecure registry not configured | Configure containerd hosts.toml |
| `dial tcp: lookup ... no such host` | DNS forwarding not working | Restart systemd-resolved, check CoreDNS IP |

### CoreDNS Not Responding

```bash
# Check CoreDNS is running
kubectl -n kube-system get pods -l k8s-app=kube-dns

# Test DNS from inside the cluster
kubectl run -it --rm dns-test --image=busybox -- nslookup registry.registry.svc.cluster.local
```

### containerd Not Using Config

After changing containerd configuration, always restart:

```bash
systemctl restart containerd
systemctl status containerd
```

Check containerd logs for errors:

```bash
journalctl -u containerd --since "5 min ago"
```
