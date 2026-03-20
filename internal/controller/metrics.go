package controller

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const controllerName = "birservice"

var (
	metricsRegisterOnce sync.Once

	reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "easydeploy_reconcile_total",
			Help: "Total number of BirService reconciliations partitioned by result.",
		},
		[]string{"controller", "result"},
	)

	reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "easydeploy_reconcile_duration_seconds",
			Help:    "Duration of BirService reconciliations partitioned by result.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"controller", "result"},
	)

	reconcileInflight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "easydeploy_reconcile_inflight",
			Help: "Current number of inflight BirService reconciliations.",
		},
		[]string{"controller"},
	)

	buildStatusTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "easydeploy_build_status_total",
			Help: "Total number of build status updates by status.",
		},
		[]string{"status"},
	)
)

func registerControllerMetrics() {
	metricsRegisterOnce.Do(func() {
		ctrlmetrics.Registry.MustRegister(
			reconcileTotal,
			reconcileDuration,
			reconcileInflight,
			buildStatusTotal,
		)
	})
}
