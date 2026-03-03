#!/usr/bin/env bash
set -euo pipefail

echo "== CRDs =="
kubectl get crd \
  prometheuses.monitoring.coreos.com \
  alertmanagers.monitoring.coreos.com \
  servicemonitors.monitoring.coreos.com \
  podmonitors.monitoring.coreos.com

echo
echo "== Argo CD Application (monitoring) =="
kubectl -n argocd get application kube-prometheus-stack -o wide || true

echo
echo "== monitoring namespace pods =="
kubectl -n monitoring get pods -o wide || true

echo
echo "== Prometheus/Alertmanager CRs (should exist) =="
kubectl -n monitoring get prometheus,alertmanager || true

echo
echo "== Prometheus StatefulSets (should exist) =="
kubectl -n monitoring get statefulset -o wide | grep -i prometheus || true
