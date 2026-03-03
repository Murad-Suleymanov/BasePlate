#!/usr/bin/env bash
set -euo pipefail

# Installs the standard Gateway API CRDs required by nginx-gateway-fabric.
# Run this once before deploying the nginx-gateway-fabric Helm chart.

NGF_VERSION="v2.4.2"

echo "Installing Gateway API CRDs (standard channel) for nginx-gateway-fabric ${NGF_VERSION}..."
kubectl kustomize "https://github.com/nginx/nginx-gateway-fabric/config/crd/gateway-api/standard?ref=${NGF_VERSION}" | kubectl apply -f -

echo
echo "Verifying Gateway API CRDs..."
kubectl get crd gateways.gateway.networking.k8s.io
kubectl get crd httproutes.gateway.networking.k8s.io
kubectl get crd gatewayclasses.gateway.networking.k8s.io

echo
echo "OK: Gateway API CRDs installed."
