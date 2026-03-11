# CLI Tool — easydeployctl

`easydeployctl` is a command-line utility that converts the developer's simple YAML format into a full `BirService` custom resource manifest.

## Purpose

The CLI tool bridges the gap between the simplified developer YAML and the actual Kubernetes custom resource. While this conversion is normally handled automatically by the Helm chart in the ArgoCD pipeline, the CLI is useful for:

- **Debugging** — see what BirService CR will be generated from your YAML
- **Testing** — validate your YAML before committing
- **Manual apply** — apply directly with `kubectl` without ArgoCD

## Usage

```bash
# Build the CLI
go build -o easydeployctl ./cmd/easydeployctl

# Convert a simple YAML to a BirService CR
./easydeployctl -f echo/dev.yaml

# Pipe to kubectl
./easydeployctl -f echo.yaml | kubectl apply -f -
```

## Input Format

The CLI accepts the same simple YAML format used in the BasePlate-Dev repository:

```yaml
image: ealen/echo-server:0.9.2
port: 8080
```

## Output Format

The CLI produces a `BirService` custom resource:

```yaml
apiVersion: deploy.easydeploy.io/v1alpha1
kind: BirService
metadata:
  name: echo
  namespace: dev
spec:
  image: ealen/echo-server:0.9.2
  port: 8080
  replicas: 1
```

## Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-f` | Path to the input YAML file (e.g. api/prod.yaml) | (required) |
| `-n` | Namespace override | Derived from filename (prod.yaml → prod) |
| `--name` | Service name override | Derived from folder (api/ → api) |

## Building from Source

```bash
cd BasePlate
go build -o easydeployctl ./cmd/easydeployctl
```

The binary has no external dependencies and can be distributed as a standalone executable.
