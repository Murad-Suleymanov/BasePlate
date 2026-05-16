# User Guide

This section covers everything an application developer needs to know to deploy and manage applications on the Easy-Deploy platform.

## Developer Workflow

```mermaid
flowchart LR
    Write["Write YAML"] --> Push["Push to Git"]
    Push --> Sync["ArgoCD Syncs"]
    Sync --> Live["App is Live"]
```

As a developer, your workflow is:

1. **Write** a simple YAML file (as little as one line)
2. **Commit** it to the `BasePlate-Dev` repository
3. **Wait** for ArgoCD to sync (1–3 minutes)
4. **Access** your application at `https://<name>-<namespace>.easysolution.work`

## File Placement

Place your YAML files in the `BasePlate-Dev` repository following this structure:

```
BasePlate-Dev/
├── echo/           # service_name = folder
│   ├── dev.yaml    # namespace = filename
│   └── prod.yaml
├── api/
│   ├── dev.yaml
│   └── stage.yaml
└── welcome/
    └── dev.yaml
```

The **folder name** determines the service name. The **filename** (without `.yaml`) determines the Kubernetes namespace.

| Path | Namespace | Service Name | URL |
|------|-----------|-------------|-----|
| `echo/dev.yaml` | `dev` | `echo` | `echo-dev.easysolution.work` |
| `api/stage.yaml` | `stage` | `api` | `api-stage.easysolution.work` |

## What Can You Deploy?

=== "Container Image"

    Deploy a ready-made container image from any public registry:

    ```yaml
    image: nginx:alpine
    ```

=== "Git Repository"

    Build and deploy from a Git repository containing a Dockerfile:

    ```yaml
    repo: https://github.com/your-org/your-app
    ```

=== "Custom Configuration"

    Customize ports, replicas, hostname, and more:

    ```yaml
    repo: https://github.com/your-org/your-app
    tag: v2.0.0
    replicas: 3
    port: 3000
    hostname: api.mycompany.com
    ```

## Guides

| Guide | Description |
|-------|-------------|
| [YAML Reference](yaml-reference.md) | Complete list of all available fields |
| [Validation](validation.md) | IDE feedback, pre-commit hooks, and PR CI for catching errors early |
| [Deploying Images](deploying-images.md) | Deploy pre-built container images |
| [Deploying from Git](deploying-from-git.md) | Build images from Git repositories |
| [Rebuild Strategies](rebuild-strategies.md) | Tag-based and webhook-based rebuild triggers |
| [Port Auto-Detection](auto-detection.md) | How the platform detects your application's port |
