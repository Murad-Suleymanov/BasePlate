# YAML Reference

This is the complete reference for the developer YAML format used by BasePlate.

## File Location

```
BasePlate-Dev/<service_name>/<namespace_name>.yaml
```

- `<service_name>` — the service name (folder name)
- `<namespace_name>` — the Kubernetes namespace (filename without `.yaml`, e.g., `dev`, `prod`, `stage`)

## Minimal Examples

=== "Container Image"

    ```yaml
    image: ealen/echo-server:0.9.2
    ```

=== "Git Repository"

    ```yaml
    repo: https://github.com/docker/welcome-to-docker
    ```

## All Fields

```yaml
# ┌─────────────────────────────────────────────────┐
# │ Image Source (specify one)                       │
# └─────────────────────────────────────────────────┘

# Pre-built container image from any registry
image: ""

# Git repository URL containing a Dockerfile
repo: ""

# ┌─────────────────────────────────────────────────┐
# │ Build Options (only used with repo:)             │
# └─────────────────────────────────────────────────┘

# Git branch, tag, or commit to build
# Default: repository's default branch
tag: ""

# Path to Dockerfile relative to repo root
# Default: "Dockerfile"
dockerfile: ""

# Add GitHub Actions build workflow to the repo (GitHub only)
# Requires GITHUB_TOKEN configured in platform. Pipeline pushes to registry on every push.
injectPipeline: false

# ┌─────────────────────────────────────────────────┐
# │ Runtime Options                                  │
# └─────────────────────────────────────────────────┘

# Port the application listens on
# Default: auto-detected from image, fallback 8080
port: 0

# Container port if different from service port
# Default: same as port
containerPort: 0

# Number of pod replicas
# Default: 1
replicas: 1

# HPA config (used when `replicas` is not set)
hpa:
  minReplicas: 2
  maxReplicas: 5

# Container resources
resources:
  requests:
    memory: 200Mi   # default
    cpu: 75m        # default
  limits:
    memory: 400Mi   # default: 2x requests
    cpu: 150m       # default: 2x requests

# Custom hostname for external access
# Default: <name>-<namespace>.easysolution.work
hostname: ""
```

## Field Details

### `image`

A fully-qualified container image reference. Used when deploying pre-built images.

```yaml
image: nginx:alpine
image: ealen/echo-server:0.9.2
image: ghcr.io/your-org/your-app:v1.0.0
```

!!! note
    `image` and `repo` are mutually exclusive. If both are set, `image` takes precedence.

### `repo`

A Git repository URL. When this is a recognized Git hosting URL (GitHub, GitLab, Bitbucket), the platform clones the repository, builds the Dockerfile with Kaniko, and pushes the image to the local registry.

```yaml
repo: https://github.com/your-org/your-app
repo: https://gitlab.com/your-org/your-app
repo: https://bitbucket.org/your-org/your-app
```

Supported URL patterns:

- `https://github.com/*`
- `https://gitlab.com/*`
- `https://bitbucket.org/*`
- Any URL ending in `.git`

### `tag`

The Git branch, tag, or commit to build. Only used when `repo` is a Git URL.

```yaml
repo: https://github.com/your-org/your-app
tag: v2.0.0      # Build from a Git tag
tag: develop      # Build from a branch
tag: abc123f      # Build from a commit
```

If omitted, Kaniko uses the repository's default branch.

!!! tip "Triggering Rebuilds"
    Changing `tag` triggers a new build. See [Rebuild Strategies](rebuild-strategies.md).

### `injectPipeline`

When `true` and `repo` is a GitHub URL, the platform adds a GitHub Actions workflow to the repository. Requires `github-pipeline-secret` (one-time setup).

### `dockerfile`

Path to the Dockerfile relative to the repository root. Useful for monorepos.

```yaml
repo: https://github.com/your-org/monorepo
dockerfile: services/api/Dockerfile
```

Default: `Dockerfile`

### `port`

The port your application listens on. This is used for both the Kubernetes Service and the HTTPRoute backend.

```yaml
port: 3000
```

If omitted, the platform tries to auto-detect the port from the image. See [Port Auto-Detection](auto-detection.md).

Fallback: `8080`

### `containerPort`

The container port, if different from the service port. This is an advanced option for cases where you want the Service to expose a different port than what the container listens on.

```yaml
port: 80              # Service port (external)
containerPort: 8080   # Container port (internal)
```

Default: same as `port`

### `replicas`

Number of pod replicas.

```yaml
replicas: 3
```

Default: `1`

### `hpa`

Horizontal Pod Autoscaler config. Use this when you want autoscaling instead of fixed `replicas`.

```yaml
hpa:
  minReplicas: 2
  maxReplicas: 10
```

!!! note
    If `replicas` is set, it takes priority and HPA is not created.

### `resources`

Container resource requests/limits.

```yaml
resources:
  requests:
    memory: 300Mi
    cpu: 100m
  limits:
    memory: 600Mi
    cpu: 200m
```

Defaults when not provided:

- `requests.memory: 200Mi`
- `requests.cpu: 75m`
- `limits` are calculated as `2x requests`

### `hostname`

Custom hostname for external access. Overrides the auto-generated `<name>-<namespace>.<baseDomain>` pattern.

```yaml
hostname: api.mycompany.com
```

!!! warning
    If you use a custom hostname, you must configure DNS for that domain separately. The platform's ExternalDNS only manages the `*.easysolution.work` zone.

## Auto-Detected Fields

Several fields are automatically derived if not specified:

| Field | Auto-Detected From | Fallback |
|-------|-------------------|----------|
| `name` | Folder path (`api/` → `api`) | — |
| `namespace` | Filename (`prod.yaml` → `prod`) | — |
| `hostname` | `<name>-<namespace>.easysolution.work` | — |
| `port` | Image `EXPOSE`, `ENV PORT=`, or `CMD --port` | `8080` |
| `tag` | — | Repo default branch (Git) / `latest` (image) |

## Complete Examples

### Simple Web Server

```yaml title="web/dev.yaml"
image: nginx:alpine
port: 80
```

### Git-Based API with Custom Tag

```yaml title="api/stage.yaml"
repo: https://github.com/your-org/api-service
tag: v3.1.0
replicas: 2
```

### Monorepo Microservice

```yaml title="auth/preprod.yaml"
repo: https://github.com/your-org/platform
dockerfile: services/auth/Dockerfile
tag: release/2.0
port: 4000
replicas: 3
```

### Custom Domain

```yaml title="frontend/prod.yaml"
image: ghcr.io/your-org/frontend:latest
hostname: app.yourcompany.com
port: 3000
replicas: 5
```
