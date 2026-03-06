# Admin Guide

This section covers cluster administration tasks for the Easy-Deploy platform. It is intended for platform engineers and cluster administrators.

## Responsibilities

As a platform admin, you manage:

| Area | Description |
|------|-------------|
| **Cluster Setup** | Initial installation and ArgoCD configuration |
| **Worker Nodes** | DNS forwarding and containerd configuration |
| **DNS & TLS** | Cloudflare integration and certificate management |
| **Monitoring** | Prometheus, Grafana dashboards, and alerting |
| **Upgrades** | Operator image updates and CRD migrations |

## Operational Overview

```mermaid
flowchart TB
    subgraph Admin["Platform Admin"]
        A1["Cluster Setup"]
        A2["Worker Config"]
        A3["DNS/TLS Config"]
        A4["Monitoring"]
    end

    subgraph Platform["Platform Components"]
        P1["ArgoCD"]
        P2["Operator"]
        P3["Gateway"]
        P4["cert-manager"]
        P5["ExternalDNS"]
        P6["Registry"]
        P7["Prometheus/Grafana"]
    end

    A1 --> P1
    A1 --> P2
    A2 --> P6
    A3 --> P4
    A3 --> P5
    A3 --> P3
    A4 --> P7
```

## Guides

| Guide | Description |
|-------|-------------|
| [Cluster Setup](cluster-setup.md) | Full installation walkthrough |
| [Worker Node Configuration](worker-node-config.md) | DNS forwarding and insecure registry setup |
| [DNS & TLS Setup](dns-tls-setup.md) | Cloudflare, ExternalDNS, cert-manager configuration |
| [Monitoring](monitoring.md) | Prometheus and Grafana setup |
