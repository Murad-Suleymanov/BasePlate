#!/bin/bash
# Pre-commit hook + CI helper: renders charts/birservice with each given values file.
# Helm reads charts/birservice/values.schema.json automatically — catches structural
# errors (typos, wrong types, unknown fields) and Go template errors. The output is
# discarded; we only care about exit code.
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

fail=0
for f in "$@"; do
  echo "rendering chart with $f..."
  if ! helm template "$CHART_DIR" -f "$f" > /dev/null; then
    echo "::error file=$f::helm template / schema validation failed" >&2
    fail=1
  fi
done
exit $fail
