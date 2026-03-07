# Monitoring

Easy-Deploy includes Prometheus and Grafana for cluster monitoring, deployed via the `kube-prometheus-stack` Helm chart.

## Components

| Component | Purpose |
|-----------|---------|
| **Prometheus** | Collects metrics from all cluster components |
| **Grafana** | Visualization dashboards |
| **Alertmanager** | Alert routing and notification |
| **kube-state-metrics** | Kubernetes resource metrics |
| **node-exporter** | Node-level system metrics |

## Accessing Grafana

### Public URL

Grafana is exposed publicly via the NGINX Gateway:

**https://grafana.easysolution.work**

### Credentials

```bash
# Username: admin
# Password:
kubectl -n monitoring get secret monitoring-grafana \
  -o jsonpath='{.data.admin-password}' | base64 -d; echo
```

### Port Forward (Alternative)

If you prefer local-only access:

```bash
kubectl -n monitoring port-forward svc/monitoring-grafana 13000:80
```

Open [http://localhost:13000](http://localhost:13000).

### SSH Tunnel (Alternative)

```bash
# From your local machine
ssh -L 13000:localhost:13000 root@YOUR_MASTER_NODE_IP

# On the master node
kubectl -n monitoring port-forward svc/monitoring-grafana 13000:80
```

## Useful Dashboards

Grafana comes with pre-configured dashboards from the kube-prometheus-stack:

| Dashboard | What It Shows |
|-----------|--------------|
| Kubernetes / Compute Resources / Cluster | CPU, memory, network across the cluster |
| Kubernetes / Compute Resources / Namespace (Pods) | Per-pod resource usage in a namespace |
| Kubernetes / Compute Resources / Node (Pods) | Node-level resource breakdown |
| CoreDNS | DNS query rates and latencies |
| NGINX Gateway | (if configured) Request rates and response codes |

## Monitoring Easy-Deploy

### Operator Metrics

The operator exposes Prometheus metrics on port `8080`:

```bash
# Port-forward to operator metrics
kubectl -n easy-deploy-system port-forward deployment/easy-deploy-operator 8080:8080

# Curl metrics
curl http://localhost:8080/metrics
```

### Key Metrics to Watch

| Metric | Description |
|--------|-------------|
| `controller_runtime_reconcile_total` | Total reconciliation attempts |
| `controller_runtime_reconcile_errors_total` | Failed reconciliations |
| `controller_runtime_reconcile_time_seconds` | Reconciliation latency |
| `workqueue_depth` | Items waiting in the work queue |

### Build Job Monitoring

Monitor Kaniko build jobs:

```bash
# List all build jobs across namespaces
kubectl get jobs -A -l deploy.easydeploy.io/purpose=build

# Check job durations
kubectl get jobs -A -l deploy.easydeploy.io/purpose=build \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,STATUS:.status.conditions[0].type,DURATION:.status.completionTime
```

## Accessing Prometheus

```bash
kubectl -n monitoring port-forward svc/monitoring-kube-prometheus-prometheus 9090:9090
```

Open [http://localhost:9090](http://localhost:9090) for the Prometheus UI.

### Useful PromQL Queries

**BirService count per namespace:**

```promql
count by (namespace) (kube_customresource_birservice_info)
```

**Pod restart rate (indicates issues):**

```promql
rate(kube_pod_container_status_restarts_total{namespace!~"kube-system|argocd"}[5m]) > 0
```

**Node resource usage:**

```promql
100 - (avg by(instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)
```

## Alerting

Alertmanager handles alert routing. Default alerts from kube-prometheus-stack include:

- Node down
- Pod crash looping
- High memory/CPU usage
- PersistentVolume running out of space
- Certificate expiry warnings

Configure notification channels (Slack, email, PagerDuty) in the Alertmanager configuration within the monitoring ArgoCD application.
