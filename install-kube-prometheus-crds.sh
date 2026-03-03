#!/usr/bin/env bash
set -euo pipefail

# Installs (or upgrades) the Prometheus-Operator CRDs used by kube-prometheus-stack.
# These CRDs are large; using server-side apply avoids the 256KB last-applied annotation limit.

CRD_BASE_URL="https://raw.githubusercontent.com/prometheus-community/helm-charts/main/charts/kube-prometheus-stack/charts/crds/crds"

crd_apply() {
  local crd_name="$1"
  local filename="$2"
  local url="${CRD_BASE_URL}/${filename}"

  if kubectl get crd "${crd_name}" >/dev/null 2>&1; then
    kubectl apply --server-side=true -f "${url}"
  else
    kubectl create -f "${url}"
  fi
}

crd_apply "alertmanagerconfigs.monitoring.coreos.com" "crd-alertmanagerconfigs.yaml"
crd_apply "alertmanagers.monitoring.coreos.com" "crd-alertmanagers.yaml"
crd_apply "podmonitors.monitoring.coreos.com" "crd-podmonitors.yaml"
crd_apply "probes.monitoring.coreos.com" "crd-probes.yaml"
crd_apply "prometheusagents.monitoring.coreos.com" "crd-prometheusagents.yaml"
crd_apply "prometheuses.monitoring.coreos.com" "crd-prometheuses.yaml"
crd_apply "prometheusrules.monitoring.coreos.com" "crd-prometheusrules.yaml"
crd_apply "scrapeconfigs.monitoring.coreos.com" "crd-scrapeconfigs.yaml"
crd_apply "servicemonitors.monitoring.coreos.com" "crd-servicemonitors.yaml"
crd_apply "thanosrulers.monitoring.coreos.com" "crd-thanosrulers.yaml"

echo "OK: Prometheus-Operator CRDs are present."
