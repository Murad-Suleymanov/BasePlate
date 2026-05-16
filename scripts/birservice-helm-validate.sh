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

  # "Additional property X is not allowed" → naməlum field (typo və ya naməlum knob)
  while IFS= read -r prop; do
    [ -z "$prop" ] && continue
    line_no=$(grep -n "^[[:space:]]*${prop}:" "$file" | head -1 | cut -d: -f1 || true)
    if [ -n "$line_no" ]; then
      echo "::error file=${file},line=${line_no}::Naməlum field '${prop}'. values.schema.json-da yoxdur — typo? Mümkün dəyərləri görmək üçün VSCode-da Ctrl+Space."
    else
      echo "::error file=${file}::Naməlum field '${prop}'."
    fi
  done < <(echo "$err" | grep -oE "Additional property [^ ]+ is not allowed" | awk '{print $3}')

  # "value at path X must be of type Y" — tip uyğunsuzluğu
  while IFS= read -r path; do
    [ -z "$path" ] && continue
    # path "spec.replicas" formatında ola bilər; sondan field adını götür
    field=$(basename "$path" | tr -d '.')
    line_no=$(grep -n "^[[:space:]]*${field}:" "$file" | head -1 | cut -d: -f1 || true)
    if [ -n "$line_no" ]; then
      echo "::error file=${file},line=${line_no}::Tip uyğunsuzluğu '${field}' field-ində. Schema-da yoxla."
    fi
  done < <(echo "$err" | grep -oE "value at path [^ ]+ " | awk '{print $4}')

  # Fallback: schema xətasını tutmadıqsa ham çıxışı ümumi ::error kimi göstər
  if ! echo "$err" | grep -qE "Additional property|value at path"; then
    msg=$(echo "$err" | tr '\n' ' ' | sed 's/  */ /g' | head -c 500)
    echo "::error file=${file}::${msg}"
  fi
}

fail=0
for f in "$@"; do
  echo "rendering chart with $f..."
  if err=$(helm template "$CHART_DIR" -f "$f" 2>&1 >/dev/null); then
    : # success
  else
    emit_annotations "$f" "$err"
    # full output for log
    echo "$err"
    fail=1
  fi
done
exit $fail
