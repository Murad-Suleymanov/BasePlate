# CRD Reference

Complete specification of the `BirService` custom resource definition.

## Overview

| Property | Value |
|----------|-------|
| **API Group** | `deploy.easydeploy.io` |
| **Version** | `v1alpha1` |
| **Kind** | `BirService` |
| **Plural** | `birservices` |
| **Short Name** | `bs` |
| **Scope** | Namespaced |

## Usage

```bash
# List BirServices in a namespace
kubectl -n dev get birservices
kubectl -n dev get bs

# Describe a specific BirService
kubectl -n dev describe bs echo

# Get BirService as YAML
kubectl -n dev get bs echo -o yaml
```

## Printer Columns

When listing BirServices, `kubectl` displays these columns:

| Column | JSON Path | Type |
|--------|-----------|------|
| Repo | `.spec.repo` | string |
| Image | `.spec.image` | string |
| Port | `.spec.port` | integer |
| Hostname | `.spec.hostname` | string |
| Available | `.status.availableReplicas` | integer |
| Build | `.status.buildStatus` | string |

Example output:

```
NAME      REPO                                           IMAGE                       PORT   HOSTNAME                         AVAILABLE   BUILD
echo                                                     ealen/echo-server:0.9.2            echo-dev.easysolution.work       1
welcome   https://github.com/docker/welcome-to-docker                                3000   welcome-dev.easysolution.work    1           Succeeded
```

## Spec

```yaml
apiVersion: deploy.easydeploy.io/v1alpha1
kind: BirService
metadata:
  name: example
  namespace: dev
spec:
  image: ""           # string, optional
  repo: ""            # string, optional
  tag: ""             # string, optional
  dockerfile: ""      # string, optional
  port: 0             # int32, optional (1–65535)
  containerPort: 0    # int32, optional (1–65535)
  replicas: 1         # int32, optional (≥0)
  hostname: ""        # string, optional
```

### `spec.image`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No (but one of `image` or `repo` must be set) |
| Default | `""` |

A fully-qualified container image reference.

### `spec.repo`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No (but one of `image` or `repo` must be set) |
| Default | `""` |

Either a container image repository or a Git URL containing a Dockerfile. Git URLs are detected by prefix (`https://github.com/`, `https://gitlab.com/`, `https://bitbucket.org/`) or `.git` suffix.

### `spec.tag`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | Repository default branch (Git) / `latest` (image) |

Git branch, tag, or commit reference. Only used when `repo` is a Git URL.

### `spec.dockerfile`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | `Dockerfile` |

Path to the Dockerfile relative to the repository root. Only used when `repo` is a Git URL.

### `spec.port`

| Property | Value |
|----------|-------|
| Type | `int32` |
| Required | No |
| Default | Auto-detected from image, fallback `8080` |
| Validation | 1–65535 |

The service port. Used for the Kubernetes Service and HTTPRoute backend reference.

### `spec.containerPort`

| Property | Value |
|----------|-------|
| Type | `int32` |
| Required | No |
| Default | Same as `port` |
| Validation | 1–65535 |

The container port, if different from the service port.

### `spec.replicas`

| Property | Value |
|----------|-------|
| Type | `int32` |
| Required | No |
| Default | `1` |
| Validation | ≥ 0 |

Number of pod replicas.

### `spec.hostname`

| Property | Value |
|----------|-------|
| Type | `string` |
| Required | No |
| Default | `<name>-<namespace>.<BASE_DOMAIN>` |

Custom hostname for external access. Overrides the auto-generated hostname.

## Status

```yaml
status:
  availableReplicas: 1    # int32
  buildImage: ""          # string
  buildStatus: ""         # string: Building, Succeeded, Failed
  buildTag: ""            # string
  lastRebuild: ""         # string (unix timestamp)
```

### `status.availableReplicas`

Number of ready pods in the Deployment.

### `status.buildImage`

Full image reference in the local registry (e.g., `registry.registry.svc.cluster.local:5000/welcome:latest`). Only set for Git-based builds.

### `status.buildStatus`

Current build state. One of:

| Value | Description |
|-------|-------------|
| `Building` | Kaniko job is running |
| `Succeeded` | Build completed successfully |
| `Failed` | Build failed |

### `status.buildTag`

The tag used for the current/last build. Used to detect tag changes for rebuild triggers.

### `status.lastRebuild`

Unix timestamp of the last webhook-triggered rebuild. Compared against the `deploy.easydeploy.io/rebuild` annotation to detect new rebuild requests.

## Full CRD YAML

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: birservices.deploy.easydeploy.io
spec:
  group: deploy.easydeploy.io
  scope: Namespaced
  names:
    plural: birservices
    singular: birservice
    kind: BirService
    shortNames:
      - bs
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      additionalPrinterColumns:
        - name: Repo
          type: string
          jsonPath: .spec.repo
        - name: Image
          type: string
          jsonPath: .spec.image
        - name: Port
          type: integer
          jsonPath: .spec.port
        - name: Hostname
          type: string
          jsonPath: .spec.hostname
        - name: Available
          type: integer
          jsonPath: .status.availableReplicas
        - name: Build
          type: string
          jsonPath: .status.buildStatus
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                image:
                  type: string
                repo:
                  type: string
                tag:
                  type: string
                dockerfile:
                  type: string
                replicas:
                  type: integer
                  format: int32
                  minimum: 0
                port:
                  type: integer
                  format: int32
                  minimum: 1
                  maximum: 65535
                containerPort:
                  type: integer
                  format: int32
                  minimum: 1
                  maximum: 65535
                hostname:
                  type: string
            status:
              type: object
              properties:
                availableReplicas:
                  type: integer
                  format: int32
                buildImage:
                  type: string
                buildStatus:
                  type: string
                buildTag:
                  type: string
                lastRebuild:
                  type: string
```

## Annotations

| Annotation | Description | Set By |
|-----------|-------------|--------|
| `deploy.easydeploy.io/rebuild` | Unix timestamp — triggers a rebuild when changed | Webhook server |

## Labels

| Label | Description | Applied To |
|-------|-------------|-----------|
| `app.kubernetes.io/name` | BirService name | All child resources |
| `app.kubernetes.io/managed-by` | `easy-deploy-operator` | All child resources |
| `deploy.easydeploy.io/tenant` | Namespace | All child resources |
| `deploy.easydeploy.io/purpose` | `build` | Build jobs |
| `deploy.easydeploy.io/build-tag` | Image tag | Build jobs |
