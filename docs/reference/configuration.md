# Configuration

Reference for all configurable settings in the Easy-Deploy platform.

## Operator Environment Variables

Set in `manifests/operator/deployment.yaml`:

| Variable | Description | Default | Example |
|----------|-------------|---------|---------|
| `BASE_DOMAIN` | Domain suffix for auto-generated hostnames | (none) | `easysolution.work` |
| `TARGET_IP` | Public IP address for DNS A records | (none) | `116.203.203.121` |

### `BASE_DOMAIN`

When set, the operator generates hostnames in the format `<name>-<namespace>.<BASE_DOMAIN>` for each BirService. If not set, hostnames are only generated when explicitly specified in `spec.hostname`.

### `TARGET_IP`

The public IP address that ExternalDNS will use for DNS A records. This should be the public IP of your worker node running the NGINX Gateway Fabric.

## Operator Command-Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `:8080` | Prometheus metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Health and readiness probes |
| `--webhook-bind-address` | `:9090` | GitHub webhook server |
| `--leader-elect` | `false` | Enable leader election |

## Operator Ports

| Port | Name | Protocol | Purpose |
|------|------|----------|---------|
| 8080 | metrics | HTTP | Prometheus metrics |
| 8081 | health | HTTP | `/healthz` and `/readyz` probes |
| 9090 | webhook | HTTP | GitHub webhook endpoint |

## Internal Constants

These are compiled into the operator and not configurable at runtime:

| Constant | Value | Description |
|----------|-------|-------------|
| `registryURL` | `registry.registry.svc.cluster.local:5000` | Local registry address |
| `kanikoImage` | `gcr.io/kaniko-project/executor:latest` | Kaniko build image |
| `requeueBuild` | `10s` | Requeue interval while build is running |
| `gatewayName` | `main-gateway` | Name of the NGINX Gateway resource |
| `gatewayNamespace` | `nginx-gateway` | Namespace of the gateway |

## Gateway Configuration

`manifests/gateway/gateway.yaml`:

| Setting | Value |
|---------|-------|
| Gateway name | `main-gateway` |
| Gateway class | `nginx` |
| HTTP listener | Port 80, all namespaces |
| HTTPS listener | Port 443, TLS terminate, wildcard cert |

## Certificate Configuration

`manifests/gateway/wildcard-certificate.yaml`:

| Setting | Value |
|---------|-------|
| Certificate name | `wildcard-easysolution` |
| Secret name | `wildcard-tls` |
| Issuer | `letsencrypt-prod` (ClusterIssuer) |
| DNS names | `*.easysolution.work` |

## Registry Configuration

`manifests/registry/`:

| Setting | Value |
|---------|-------|
| Image | `registry:2` |
| Namespace | `registry` |
| Service name | `registry` |
| ClusterIP port | 5000 |
| NodePort | 30500 |
| Storage | `hostPath: /var/lib/registry` |

## RBAC Permissions

The operator's ClusterRole grants these permissions:

| API Group | Resource | Verbs |
|-----------|----------|-------|
| `deploy.easydeploy.io` | `birservices` | get, list, watch, create, update, patch, delete |
| `deploy.easydeploy.io` | `birservices/status` | get, update, patch |
| `apps` | `deployments` | get, list, watch, create, update, patch, delete |
| (core) | `services` | get, list, watch, create, update, patch, delete |
| `batch` | `jobs` | get, list, watch, create, update, patch, delete |
| `gateway.networking.k8s.io` | `httproutes` | get, list, watch, create, update, patch, delete |

## CI/CD Configuration

`.github/workflows/operator-image.yml`:

| Setting | Value |
|---------|-------|
| Trigger | Push to `main` or manual dispatch |
| Registry | `ghcr.io` |
| Image | `ghcr.io/<owner>/easy-deploy-operator:main` |
| Platforms | `linux/amd64` |
| Cache | GitHub Actions cache (`type=gha`) |
