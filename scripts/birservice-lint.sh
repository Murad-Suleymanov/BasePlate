#!/usr/bin/env bash
# Semantic (cross-field) validation for BirService tenant values.yaml.
# JSON Schema (values.schema.json) catches structural errors (typos, wrong types,
# unknown fields). This script catches *combinations* that the schema can't express:
# mutually exclusive fields, derived constraints, deployment-time conflicts.
#
# Supports two values shapes:
#   single-instance — operational keys at root (legacy)
#   multi-instance  — instance maps at root (main, slave, worker, …), service-level
#                     fields (repo, owner, image) may be at root too
#
# For service.yaml (service-level metadata only): validates owner is non-empty and
# repo is set; structural checks happen via helm template (separate hook).
#
# Usage: birservice-lint.sh <path-to-values.yaml>
# Exits 1 on errors (warnings are non-fatal).
# Emits GitHub Actions ::error and ::warning annotations when running in CI.

set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <values.yaml>" >&2
  exit 2
fi

file="$1"
if [ ! -f "$file" ]; then
  echo "file not found: $file" >&2
  exit 2
fi

if ! command -v yq >/dev/null 2>&1; then
  echo "yq is required (https://github.com/mikefarah/yq)" >&2
  exit 2
fi

# Resolve a usable Python 3 interpreter (Windows often only has `python`).
PY=""
for _cmd in python3 python; do
  if command -v "$_cmd" >/dev/null 2>&1 && \
     "$_cmd" -c "import sys; sys.exit(0 if sys.version_info[0] >= 3 else 1)" >/dev/null 2>&1; then
    PY="$_cmd"
    break
  fi
done
if [ -z "$PY" ]; then
  echo "python 3 is required (looked for python3, python)" >&2
  exit 2
fi

errors=0

# Output format: GitHub Actions annotations in CI, human-readable in terminals.
if [ -n "${GITHUB_ACTIONS:-}" ]; then
  note()  { echo "::notice file=$file::$1"; }
  warn()  { echo "::warning file=$file::$1"; }
  error() { echo "::error file=$file::$1"; errors=$((errors+1)); }
  summary_ok()  { echo "$file: OK"; }
  summary_err() { echo "$file: $errors semantic error(s)"; }
else
  if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    C_RED=$'\033[31m'; C_YELLOW=$'\033[33m'; C_BLUE=$'\033[34m'
    C_GREEN=$'\033[32m'; C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'; C_RESET=$'\033[0m'
  else
    C_RED=""; C_YELLOW=""; C_BLUE=""; C_GREEN=""; C_BOLD=""; C_DIM=""; C_RESET=""
  fi
  _header_shown=0
  _show_header() {
    if [ "$_header_shown" -eq 0 ]; then
      echo "${C_BOLD}${file}${C_RESET}"
      _header_shown=1
    fi
  }
  note()  { _show_header; echo "  ${C_BLUE}note${C_RESET}    $1"; }
  warn()  { _show_header; echo "  ${C_YELLOW}warning${C_RESET} $1"; }
  error() { _show_header; echo "  ${C_RED}error${C_RESET}   $1"; errors=$((errors+1)); }
  summary_ok()  { echo "${C_GREEN}✓${C_RESET} ${C_DIM}${file}${C_RESET}"; }
  summary_err() { echo "${C_RED}✗${C_RESET} ${file} ${C_DIM}— ${errors} error(s)${C_RESET}"; }
fi

base=$(basename "$file")

# ─── service.yaml validation ─────────────────────────────────────────────────
# service.yaml holds shared metadata (repo, owner). No operational fields.
if [ "$base" = "service.yaml" ]; then
  owner=$(yq '.owner // ""' "$file")
  repo=$(yq '.repo // ""' "$file")
  case "$owner" in ''|'""'|null) owner="" ;; esac
  case "$repo"  in ''|'""'|null) repo=""  ;; esac

  if [ -z "$owner" ]; then
    error "service.yaml is missing required field 'owner' (free text — team name, github handle, email)."
  fi
  if [ -z "$repo" ]; then
    warn "service.yaml has no 'repo' field — fine if the env files provide image instead, otherwise the operator can't build."
  fi

  if [ "$errors" -gt 0 ]; then
    summary_err
    exit 1
  fi
  summary_ok
  exit 0
fi

# ─── env-file validation: detect single vs multi-instance shape ─────────────
KNOWN_KEYS=(name owner image repo tag imageTag dockerfile port containerPort replicas
            hpa resources hostname expose metrics traffic readinessProbe livenessProbe
            singleton maxDown shutdown canary injectPipeline)

is_known_key() {
  local k="$1"
  for kk in "${KNOWN_KEYS[@]}"; do [ "$k" = "$kk" ] && return 0; done
  return 1
}

# Top-level keys whose value is a map AND name is not in KNOWN_KEYS → candidate instances.
# A real instance must contain at least one KNOWN_KEY among its children (e.g., image,
# repo, port). If none of its children are recognized, it is almost certainly a typo
# of a known top-level key (e.g., "trafcas:" instead of "traffic:"). The JSON schema
# cannot catch this because additionalProperties is true at root (required for the
# multi-instance shape).
mapfile -t unknown_map_keys < <(yq 'keys | .[]' "$file" | while read -r k; do
  is_known_key "$k" && continue
  kind=$(yq ".\"$k\" | type" "$file" 2>/dev/null || echo "")
  [ "$kind" = "!!map" ] && echo "$k"
done)

# Suggest the closest KNOWN_KEY for a typo (Levenshtein distance ≤ 3).
suggest_known_key() {
  "$PY" -c "
import sys
target = sys.argv[1]
candidates = sys.argv[2:]
def dl(a, b):
    m, n = len(a), len(b)
    dp = [[0]*(n+1) for _ in range(m+1)]
    for i in range(m+1): dp[i][0] = i
    for j in range(n+1): dp[0][j] = j
    for i in range(1, m+1):
        for j in range(1, n+1):
            cost = 0 if a[i-1] == b[j-1] else 1
            dp[i][j] = min(dp[i-1][j]+1, dp[i][j-1]+1, dp[i-1][j-1]+cost)
    return dp[m][n]
best = min(candidates, key=lambda c: dl(target.lower(), c.lower()))
if dl(target.lower(), best.lower()) <= 3:
    print(best)
" "$1" "${KNOWN_KEYS[@]}" 2>/dev/null
}

instance_keys=()
for k in "${unknown_map_keys[@]}"; do
  has_known_child=false
  while read -r sub; do
    if is_known_key "$sub"; then has_known_child=true; break; fi
  done < <(yq ".\"$k\" | keys | .[]" "$file" 2>/dev/null)
  if [ "$has_known_child" = "true" ]; then
    instance_keys+=("$k")
  else
    suggestion=$(suggest_known_key "$k")
    if [ -n "$suggestion" ]; then
      error "unknown root key '$k' — none of its sub-keys are recognized. Did you mean '$suggestion'?"
    else
      error "unknown root key '$k' — none of its sub-keys are recognized as known fields. If this is meant to be a new instance, it must contain at least one of: ${KNOWN_KEYS[*]}."
    fi
  fi
done

lint_workload() {
  # Lint a single workload's fields. Args: yq_path_prefix (empty for root, ".name" for instance).
  local prefix="$1"
  local label="$2"  # human-readable label for messages: "root" or "instance 'main'"

  y() { yq "${prefix}$1 // $2" "$file"; }

  # Catch typos inside instance maps too — additionalProperties: true at root
  # means JSON Schema can't see them, and our root-level scan stops at the
  # instance boundary. Walk each immediate child; warn on any name that
  # isn't a KNOWN_KEY.
  if [ -n "$prefix" ]; then
    while read -r child; do
      [ -z "$child" ] && continue
      if ! is_known_key "$child"; then
        suggestion=$(suggest_known_key "$child")
        if [ -n "$suggestion" ]; then
          error "[$label] unknown field '$child'. Did you mean '$suggestion'?"
        else
          error "[$label] unknown field '$child' — not declared in values.schema.json."
        fi
      fi
    done < <(yq "${prefix} | keys | .[]" "$file" 2>/dev/null)
  fi

  local singleton hpa_min hpa_max replicas image repo max_down traffic_present eject ratelimit_enabled
  local req_mem lim_mem req_cpu lim_cpu

  singleton=$(y '.singleton' 'false')
  hpa_min=$(y '.hpa.minReplicas' '0')
  hpa_max=$(y '.hpa.maxReplicas' '0')
  replicas=$(y '.replicas' '0')
  image=$(y '.image' '""')
  repo=$(y '.repo' '""')
  max_down=$(y '.maxDown' '-1')
  traffic_present=$(yq "${prefix} | has(\"traffic\")" "$file" 2>/dev/null || echo "false")
  eject=$(y '.traffic.ejectUnhealthy' 'true')
  ratelimit_enabled=$(y '.traffic.rateLimit.enabled' 'false')
  req_mem=$(y '.resources.requests.memory' '""')
  lim_mem=$(y '.resources.limits.memory' '""')
  req_cpu=$(y '.resources.requests.cpu' '""')
  lim_cpu=$(y '.resources.limits.cpu' '""')

  strip_empty() { case "$1" in ''|'""'|null) echo "" ;; *) echo "$1" ;; esac; }
  image=$(strip_empty "$image")
  repo=$(strip_empty "$repo")
  req_mem=$(strip_empty "$req_mem")
  lim_mem=$(strip_empty "$lim_mem")
  req_cpu=$(strip_empty "$req_cpu")
  lim_cpu=$(strip_empty "$lim_cpu")

  if [ "$singleton" = "true" ] && [ "$hpa_min" -gt 1 ]; then
    error "[$label] singleton: true + hpa.minReplicas: $hpa_min — a singleton app cannot run multiple pods. Remove the HPA block or set minReplicas: 1."
  fi
  if [ "$singleton" = "true" ] && [ "$replicas" -gt 1 ]; then
    error "[$label] singleton: true + replicas: $replicas — a singleton app cannot run multiple pods. Set replicas: 1 or remove the field."
  fi
  if [ -n "$image" ] && [ -n "$repo" ]; then
    error "[$label] image and repo are mutually exclusive — pick one. image=$image repo=$repo"
  fi
  if [ "$replicas" -gt 0 ] && [ "$hpa_min" -gt 0 ]; then
    warn "[$label] replicas: $replicas + hpa.minReplicas: $hpa_min — replicas wins, the HPA will not be created. Remove one."
  fi
  if [ "$hpa_min" -gt 0 ] && [ "$hpa_max" -gt 0 ] && [ "$hpa_min" -gt "$hpa_max" ]; then
    error "[$label] hpa.minReplicas ($hpa_min) > hpa.maxReplicas ($hpa_max)."
  fi
  local effective=$replicas
  [ "$effective" -eq 0 ] && effective=$hpa_min
  [ "$effective" -eq 0 ] && effective=1
  if [ "$max_down" -ge 0 ] && [ "$max_down" -ge "$effective" ]; then
    warn "[$label] maxDown ($max_down) >= effective replicas ($effective) — PDB will be skipped, no voluntary-disruption protection."
  fi
  if [ "$ratelimit_enabled" = "true" ] && [ "$traffic_present" != "true" ]; then
    error "[$label] rateLimit.enabled set but traffic block is missing — rate limit requires the mesh."
  fi
  if [ "$eject" = "false" ]; then
    note "[$label] ejectUnhealthy: false — outlier detection disabled. Make sure this app legitimately returns 5xx (webhook, batch endpoint, …)."
  fi

  quantity_to_bytes() {
    "$PY" -c "
import re, sys
s = sys.argv[1]
m = re.match(r'^([0-9.]+)\s*([KMGTP]?i?)\$', s)
if not m: sys.exit(0)
n, suf = float(m.group(1)), m.group(2)
mult = {'':1,'K':1e3,'M':1e6,'G':1e9,'T':1e12,'P':1e15,
        'Ki':1024,'Mi':1024**2,'Gi':1024**3,'Ti':1024**4,'Pi':1024**5}.get(suf,1)
print(int(n*mult))
" "$1" 2>/dev/null || echo ""
  }
  if [ -n "$req_mem" ] && [ -n "$lim_mem" ]; then
    rb=$(quantity_to_bytes "$req_mem")
    lb=$(quantity_to_bytes "$lim_mem")
    if [ -n "$rb" ] && [ -n "$lb" ] && [ "$lb" -lt "$rb" ]; then
      error "[$label] resources.limits.memory ($lim_mem) < requests.memory ($req_mem)."
    fi
  fi
  quantity_cpu_to_milli() {
    "$PY" -c "
import re, sys
s = sys.argv[1]
if s.endswith('m'):
    print(int(float(s[:-1])))
else:
    print(int(float(s)*1000))
" "$1" 2>/dev/null || echo ""
  }
  if [ -n "$req_cpu" ] && [ -n "$lim_cpu" ]; then
    rc=$(quantity_cpu_to_milli "$req_cpu")
    lc=$(quantity_cpu_to_milli "$lim_cpu")
    if [ -n "$rc" ] && [ -n "$lc" ] && [ "$lc" -lt "$rc" ]; then
      error "[$label] resources.limits.cpu ($lim_cpu) < requests.cpu ($req_cpu)."
    fi
  fi
}

# ─── dispatch: single vs multi-instance ─────────────────────────────────────
if [ "${#instance_keys[@]}" -eq 0 ]; then
  lint_workload "" "root"
else
  for ik in "${instance_keys[@]}"; do
    lint_workload ".\"$ik\"" "instance '$ik'"
  done
fi

if [ "$errors" -gt 0 ]; then
  summary_err
  exit 1
fi
summary_ok
