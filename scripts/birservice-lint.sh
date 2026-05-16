#!/bin/bash
# Semantic (cross-field) validation for BirService tenant values.yaml.
# JSON Schema (values.schema.json) catches structural errors (typos, wrong types,
# unknown fields). This script catches *combinations* that the schema can't express:
# mutually exclusive fields, derived constraints, deployment-time conflicts.
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

errors=0
note() { echo "::notice file=$file::$1"; }
warn() { echo "::warning file=$file::$1"; }
error() { echo "::error file=$file::$1"; errors=$((errors+1)); }

# helper: yq read with default
y() { yq "$1 // $2" "$file"; }

singleton=$(y '.singleton' 'false')
hpa_min=$(y '.hpa.minReplicas' '0')
hpa_max=$(y '.hpa.maxReplicas' '0')
replicas=$(y '.replicas' '0')
image=$(y '.image' '""')
repo=$(y '.repo' '""')
max_down=$(y '.maxDown' '-1')
traffic_present=$(yq 'has("traffic")' "$file")
eject=$(y '.traffic.ejectUnhealthy' 'true')
ratelimit_enabled=$(y '.traffic.rateLimit.enabled' 'false')
req_mem=$(y '.resources.requests.memory' '""')
lim_mem=$(y '.resources.limits.memory' '""')
req_cpu=$(y '.resources.requests.cpu' '""')
lim_cpu=$(y '.resources.limits.cpu' '""')

# yq strips quotes from string defaults: '.image // ""' returns empty for missing
# fields, not the literal "". Normalize so the rest of the script can use [ -n "$x" ].
strip_empty() { case "$1" in ''|'""'|null) echo "" ;; *) echo "$1" ;; esac; }
image=$(strip_empty "$image")
repo=$(strip_empty "$repo")
req_mem=$(strip_empty "$req_mem")
lim_mem=$(strip_empty "$lim_mem")
req_cpu=$(strip_empty "$req_cpu")
lim_cpu=$(strip_empty "$lim_cpu")

# Rule: singleton + multi-replica HPA is contradictory.
if [ "$singleton" = "true" ] && [ "$hpa_min" -gt 1 ]; then
  error "singleton: true + hpa.minReplicas: $hpa_min — singleton apps cannot run multiple pods. Remove HPA or set minReplicas: 1."
fi
if [ "$singleton" = "true" ] && [ "$replicas" -gt 1 ]; then
  error "singleton: true + replicas: $replicas — singleton apps cannot run multiple pods. Set replicas: 1 or remove."
fi

# Rule: image and repo are mutually exclusive.
if [ -n "$image" ] && [ -n "$repo" ]; then
  error "image and repo are mutually exclusive — use one. image=$image repo=$repo"
fi

# Rule: replicas + hpa is a precedence trap (replicas wins).
if [ "$replicas" -gt 0 ] && [ "$hpa_min" -gt 0 ]; then
  warn "replicas: $replicas + hpa.minReplicas: $hpa_min — replicas wins, HPA will not be created. Remove one."
fi

# Rule: hpa.minReplicas > hpa.maxReplicas.
if [ "$hpa_min" -gt 0 ] && [ "$hpa_max" -gt 0 ] && [ "$hpa_min" -gt "$hpa_max" ]; then
  error "hpa.minReplicas ($hpa_min) > hpa.maxReplicas ($hpa_max)."
fi

# Rule: maxDown >= effective replicas — PDB will be skipped (no actual protection).
effective=$replicas
[ "$effective" -eq 0 ] && effective=$hpa_min
[ "$effective" -eq 0 ] && effective=1
if [ "$max_down" -ge 0 ] && [ "$max_down" -ge "$effective" ]; then
  warn "maxDown ($max_down) >= effective replicas ($effective) — PDB skipped, no voluntary-disruption protection."
fi

# Rule: rateLimit.enabled requires traffic.provider istio (or empty).
if [ "$ratelimit_enabled" = "true" ] && [ "$traffic_present" != "true" ]; then
  error "rateLimit.enabled but traffic block missing — rate limit needs the mesh."
fi

# Rule: ejectUnhealthy: false makes outlier detection a no-op — make sure it's intentional.
if [ "$eject" = "false" ]; then
  note "ejectUnhealthy: false — outlier detection disabled. Confirm this app legitimately returns 5xx (webhook, batch endpoint)."
fi

# Rule: limits < requests (k8s would reject at apply, fail fast here).
quantity_to_bytes() {
  python3 -c "
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
    error "resources.limits.memory ($lim_mem) < requests.memory ($req_mem)."
  fi
fi

quantity_cpu_to_milli() {
  python3 -c "
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
    error "resources.limits.cpu ($lim_cpu) < requests.cpu ($req_cpu)."
  fi
fi

if [ "$errors" -gt 0 ]; then
  echo "$file: $errors semantic error(s)"
  exit 1
fi
echo "$file: OK"
