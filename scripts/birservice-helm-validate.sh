#!/bin/bash
# Pre-commit hook + CI helper: renders charts/birservice with each given values file.
# Helm reads charts/birservice/values.schema.json automatically — catches structural
# errors (typos, wrong types, unknown fields) and Go template errors.
# Helm's schema error only names the offending property; this script grep-resolves the
# line in the source file so GitHub annotations point at the exact spot.
set -euo pipefail
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
CHART_DIR="$SCRIPT_DIR/../charts/birservice"

if ! command -v helm >/dev/null 2>&1; then
  echo "::error::helm not found — install https://helm.sh/docs/intro/install/" >&2
  exit 2
fi

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <values.yaml> [more.yaml...]" >&2
  exit 2
fi

emit_annotations() {
  local file="$1"
  local err="$2"
  local line_no prop msg

  # "Additional property X is not allowed" → unknown field (typo or unsupported knob)
  while IFS= read -r prop; do
    [ -z "$prop" ] && continue
    line_no=$(grep -n "^[[:space:]]*${prop}:" "$file" | head -1 | cut -d: -f1 || true)
    if [ -n "$line_no" ]; then
      echo "::error file=${file},line=${line_no}::Unknown field '${prop}' — not declared in values.schema.json. Likely a typo. In VSCode, hit Ctrl+Space for valid options."
    else
      echo "::error file=${file}::Unknown field '${prop}' — not declared in values.schema.json."
    fi
  done < <(echo "$err" | grep -oE "Additional property [^ ]+ is not allowed" | awk '{print $3}')

  # "value at path X must be of type Y" → type mismatch
  while IFS= read -r path; do
    [ -z "$path" ] && continue
    field=$(basename "$path" | tr -d '.')
    line_no=$(grep -n "^[[:space:]]*${field}:" "$file" | head -1 | cut -d: -f1 || true)
    if [ -n "$line_no" ]; then
      echo "::error file=${file},line=${line_no}::Type mismatch on field '${field}'. See values.schema.json for the expected type."
    fi
  done < <(echo "$err" | grep -oE "value at path [^ ]+ " | awk '{print $4}')

  # Fallback: if we didn't recognize the schema error shape, dump the raw text.
  if ! echo "$err" | grep -qE "Additional property|value at path"; then
    msg=$(echo "$err" | tr '\n' ' ' | sed 's/  */ /g' | head -c 500)
    echo "::error file=${file}::${msg}"
  fi
}

render_one() {
  # Render the chart with given values files. Returns 0 on success, captures stderr.
  local primary="$1"
  shift
  local extra_args=("$@")
  echo "rendering chart with $primary${extra_args[*]:+ + ${extra_args[*]}}..."
  if err=$(helm template "$CHART_DIR" -f "$primary" "${extra_args[@]}" 2>&1 >/dev/null); then
    return 0
  else
    emit_annotations "$primary" "$err"
    echo "$err"
    return 1
  fi
}

fail=0
for f in "$@"; do
  dir=$(dirname "$f")
  base=$(basename "$f")

  if [ "$base" = "service.yaml" ]; then
    # service.yaml alone isn't a complete chart input — render with each sibling env file.
    found_env=0
    for env_f in "$dir"/dev.yaml "$dir"/prod.yaml; do
      [ -f "$env_f" ] || continue
      found_env=1
      render_one "$f" -f "$env_f" || fail=1
    done
    if [ "$found_env" -eq 0 ]; then
      # No sibling env file yet — render service.yaml alone so schema is still checked.
      render_one "$f" || fail=1
    fi
  else
    # dev.yaml / prod.yaml — include sibling service.yaml if present (service first, env wins).
    service_f="$dir/service.yaml"
    if [ -f "$service_f" ]; then
      render_one "$service_f" -f "$f" || fail=1
    else
      render_one "$f" || fail=1
    fi
  fi
done
exit $fail
