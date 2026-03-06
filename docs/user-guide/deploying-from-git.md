# Deploying from Git

Easy-Deploy can build container images directly from Git repositories using Kaniko — no external CI/CD required.

## Basic Git Deployment

```yaml title="tenants/dev/simple-yaml/welcome.yaml"
repo: https://github.com/docker/welcome-to-docker
```

This single line triggers the full pipeline:

1. ArgoCD creates a BirService CR
2. The operator detects a Git URL and creates a Kaniko build job
3. Kaniko clones the repo, builds the Dockerfile, pushes to the local registry
4. The operator inspects the image to auto-detect the port
5. Creates Deployment, Service, and HTTPRoute
6. App is live at **https://welcome-dev.easysolution.work**

## Supported Git Providers

```yaml
repo: https://github.com/user/repo
repo: https://gitlab.com/user/repo
repo: https://bitbucket.org/user/repo
repo: https://any-host.com/user/repo.git  # .git suffix
```

## Building a Specific Branch or Tag

```yaml
repo: https://github.com/your-org/your-app
tag: v2.0.0
```

```yaml
repo: https://github.com/your-org/your-app
tag: develop
```

If `tag` is omitted, Kaniko uses the repository's default branch.

## Custom Dockerfile Location

For monorepos where the Dockerfile isn't at the root:

```yaml
repo: https://github.com/your-org/monorepo
dockerfile: services/api/Dockerfile
```

## Monitoring the Build

### Check BirService Status

```bash
kubectl -n dev get birservice welcome
```

Output:

```
NAME      REPO                                           IMAGE   PORT   HOSTNAME                            AVAILABLE   BUILD
welcome   https://github.com/docker/welcome-to-docker                   welcome-dev.easysolution.work       1           Succeeded
```

### Watch Build Logs

```bash
# List build jobs
kubectl -n dev get jobs

# Follow Kaniko build logs
kubectl -n dev logs job/welcome-build-latest -f
```

### Check Build Status in Detail

```bash
kubectl -n dev get birservice welcome -o jsonpath='{.status}' | python3 -m json.tool
```

```json
{
    "availableReplicas": 1,
    "buildImage": "registry.registry.svc.cluster.local:5000/welcome:latest",
    "buildStatus": "Succeeded",
    "buildTag": "latest"
}
```

## Build States

| State | Description |
|-------|-------------|
| `Building` | Kaniko job is running |
| `Succeeded` | Build completed, image available in registry |
| `Failed` | Build failed (check Kaniko logs) |

## Requirements for Your Repository

Your Git repository must contain a valid `Dockerfile` (or the path specified in `dockerfile`). The Dockerfile should:

- Be able to build independently (no external build context dependencies)
- Produce a runnable container that listens on a network port
- Ideally include an `EXPOSE` directive so the platform can auto-detect the port

!!! tip "Port Detection"
    If your Dockerfile uses `EXPOSE`, `ENV PORT=`, or your entrypoint uses `--port`/`-p`, the platform will automatically detect the port. Otherwise, specify it with `port:`.

## Example Repositories

These public repositories work out of the box with Easy-Deploy:

| Repository | Description | Port |
|-----------|-------------|------|
| `docker/welcome-to-docker` | React welcome page | 3000 (EXPOSE) |
| `crccheck/docker-hello-world` | Python hello world | 8000 (ENV PORT) |
| `ganjbakhshali/todo_docker` | Node.js TODO API | 8080 (default) |
