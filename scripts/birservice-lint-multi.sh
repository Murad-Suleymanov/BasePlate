#!/usr/bin/env bash
# Pre-commit wrapper around birservice-lint.sh — pre-commit hands the hook all matching
# staged files in one invocation; the underlying lint script takes one file at a time.
set -euo pipefail
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <values.yaml> [more.yaml...]" >&2
  exit 2
fi

fail=0
for f in "$@"; do
  if ! "$SCRIPT_DIR/birservice-lint.sh" "$f"; then
    fail=1
  fi
done
exit $fail
