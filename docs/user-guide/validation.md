# Validation

Tenant `values.yaml` files are validated in **four layers**, each catching errors earlier than the next so feedback is as fast as possible:

```text
        Developer typing in VSCode
                 │
                 ▼  <1 second
   [1] IDE schema (values.schema.json)
                 │
                 ▼  ~5 seconds
   [2] git commit → pre-commit hooks
                 │
                 ▼  ~1 minute
   [3] PR → GitHub Actions (validate workflow)
                 │
                 ▼
   [4] ArgoCD sync → K8s API server (CRD schema)
```

The first three share the *same* logic — a JSON schema and a semantic-lint script published by BasePlate. There is no drift between local and CI.

## Layer 1 — IDE (real-time)

The chart ships `charts/birservice/values.schema.json`. The workspace setting in `BasePlate-Dev/.vscode/settings.json` binds every `*/dev.yaml` and `*/prod.yaml` to that schema:

```json
{
  "yaml.schemas": {
    "../BasePlate/charts/birservice/values.schema.json": [
      "*/dev.yaml",
      "*/prod.yaml"
    ]
  }
}
```

What this catches **as you type**:

- Unknown fields (typos like `ejectUnhelthy`, `traficc`) → red squiggle, suggestion list.
- Wrong types (`replicas: "3"` as a string) → underlined.
- Out-of-range values (`port: 99999`) → flagged.
- `Ctrl+Space` → autocomplete with field descriptions from the schema.

**Setup:** none — VSCode picks it up automatically if both repos are siblings in the workspace. If you cloned BasePlate-Dev alone, switch the relative path in `.vscode/settings.json` to the GitHub raw URL (the file has the URL commented).

## Layer 2 — Local pre-commit (before push)

Configured in `BasePlate-Dev/.pre-commit-config.yaml`:

```yaml
repos:
  - repo: https://github.com/Murad-Suleymanov/BasePlate
    rev: main
    hooks:
      - id: birservice-lint
      - id: birservice-helm-validate
```

**One-time setup per clone:**

```bash
pip install pre-commit
pre-commit install
```

After `pre-commit install`, every `git commit` runs both hooks against staged `dev.yaml`/`prod.yaml`:

| Hook | What it does |
|---|---|
| `birservice-helm-validate` | Runs `helm template charts/birservice -f <file>`. Catches schema violations (typos, types, ranges) and Go template errors. |
| `birservice-lint` | Cross-field semantic rules the schema can't express. |

If either fails, the commit is **blocked**. Example output:

```
BirService helm template (schema)......................Failed
- traffic: Additional property ejectUnhelthy is not allowed

BirService semantic lint...............................Failed
::error::singleton: true + hpa.minReplicas: 3 — a singleton app cannot run multiple pods.
::error::image and repo are mutually exclusive — pick one.
```

To bypass locally (not recommended; CI will catch it anyway):

```bash
git commit --no-verify
```

## Layer 3 — PR CI (GitHub Actions)

`BasePlate-Dev/.github/workflows/validate.yml` runs the **same** pre-commit hooks on PRs and pushes to `main`:

```bash
pip install pre-commit
pre-commit run --from-ref $base --to-ref $head
```

On failure, the workflow posts a **sticky comment** on the PR with the formatted error log, plus inline annotations on the offending lines. The merge button is disabled by branch protection.

Sticky comment example:

> ### ⛔ BirService validation failed
>
> One or more tenant values files in this PR have problems. Fix the issues below and push again.
>
> ```
> BirService helm template (schema)............Failed
> - (root): Additional property singletton is not allowed
> BirService semantic lint......................Failed
> ::error::limits.memory (200Mi) < requests.memory (500Mi).
> ```

The comment is **updated in place** on every push (no spam); it is **deleted automatically** when validation passes.

### Branch protection

To make `validate` a real gate, branch protection on `main` is configured to:

- Require pull request before merging (greys out "Commit directly to main" in the GitHub web editor).
- Require status checks to pass — specifically the `validate` job.
- Disallow direct pushes / deletes.

This means **even commits made via the GitHub web UI** cannot bypass validation — they're forced through a PR.

## Layer 4 — K8s API server (last line)

When ArgoCD applies the rendered `BirService` CR, the kube-apiserver re-validates against the CRD's OpenAPI v3 schema (published in `charts/easy-deploy-platform/values.yaml` → `crd.specProperties`). Anything malformed reaches this layer (e.g. someone bypassed all prior layers via admin override) is rejected here. ArgoCD's sync goes red, and the bad commit is easy to revert.

## What Each Rule Checks

### Schema rules (Layer 1–3)

| Rule | Example violation |
|---|---|
| Unknown property | `ejectUnhelthy: false` (typo) |
| Wrong type | `replicas: "3"` (string instead of int) |
| Out of range | `port: 0`, `port: 99999`, `weight: 200` |
| Required field missing | `readinessProbe: {}` (path required) |
| Enum violation | `traffic.provider: linkerd` (only "" or "istio") |

### Semantic rules (Layer 2–3)

| Rule | Severity | Why |
|---|---|---|
| `singleton: true` + `hpa.minReplicas > 1` | error | Singleton can't run multiple pods. |
| `singleton: true` + `replicas > 1` | error | Same. |
| `image` and `repo` both set | error | Mutually exclusive. |
| `replicas` and `hpa.minReplicas` both set | warning | Replicas wins; HPA silently ignored. |
| `hpa.minReplicas > hpa.maxReplicas` | error | Invalid range. |
| `maxDown >= effective replicas` | warning | PDB skipped; no protection. |
| `rateLimit.enabled` + no `traffic` block | error | Rate limit needs the mesh. |
| `ejectUnhealthy: false` | notice | Confirm intent (only for 5xx-returning apps). |
| `limits.memory < requests.memory` | error | k8s would reject; fail fast. |
| `limits.cpu < requests.cpu` | error | Same. |

## Adding a New Rule

1. Edit `BasePlate/scripts/birservice-lint.sh` — add a new `error/warn/note` block.
2. Commit + push to BasePlate `main`.
3. Tenant pre-commit and CI pick it up automatically (`rev: main`).

For schema-level rules (new fields, type changes):

1. Add the field to `BasePlate/api/v1alpha1/birservice_types.go` (operator API).
2. Update `BasePlate/charts/easy-deploy-platform/values.yaml` (`crd.specProperties` — CRD schema served by the API server).
3. Update `BasePlate/charts/birservice/values.schema.json` (chart-level schema for Helm + IDE + pre-commit).
4. Update [`yaml-reference.md`](yaml-reference.md) and [`crd-reference.md`](../reference/crd-reference.md).
5. Wire the reconciler in `BasePlate/internal/controller/`.

The schema lives in two places by design — the operator's CRD (kube-apiserver) and the chart's values.schema.json (Helm + IDE). Keep them in sync. A future auto-gen step (`cmd/gen-values-schema`) can derive one from the other; for now it's a manual two-file edit.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Schema not picked up in VSCode | BasePlate not a sibling folder | Switch `.vscode/settings.json` to the GitHub raw URL |
| Pre-commit fails with `is not executable` | Scripts checked in without `+x` bit | `git update-index --chmod=+x scripts/*.sh` in BasePlate, push |
| `helm: command not found` in pre-commit | Helm not installed locally | Install Helm; or skip the `birservice-helm-validate` hook with `SKIP=birservice-helm-validate git commit` |
| CI fails with `mutable reference (main)` warning | Informational; pre-commit warns about `rev: main` | Switch to a tag (`v0.1.0`) once BasePlate cuts releases |
| Sticky comment not posted | Workflow lacks `pull-requests: write` | Permissions in `validate.yml` must include `pull-requests: write` |
