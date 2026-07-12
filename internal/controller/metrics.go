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

	// sloBreachTotal counts SLO breach OBSERVATIONS, not rollbacks: a rollout under
	// observation is polled every requeueRollout, so a breaching version increments this
	// on each poll until it is quarantined (mode=enforce, one or two polls) or its
	// observation window ends (mode=monitor). Use it to see which services breach and how
	// often; the AutoRollback/SLOBreach events are the per-incident record.
	sloBreachTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "easydeploy_slo_breach_total",
			Help: "SLO breach observations during rollouts, by namespace, service and autoRollback mode.",
		},
		[]string{"namespace", "service", "mode"},
	)
)

func registerControllerMetrics() {
	metricsRegisterOnce.Do(func() {
		ctrlmetrics.Registry.MustRegister(
			reconcileTotal,
			reconcileDuration,
			reconcileInflight,
			buildStatusTotal,
			sloBreachTotal,
		)

		// Pre-create common label combinations so metrics are visible
		// even before the first reconcile/build event happens.
		for _, result := range []string{"success", "error", "requeue"} {
			reconcileTotal.WithLabelValues(controllerName, result).Add(0)
			_, _ = reconcileDuration.GetMetricWithLabelValues(controllerName, result)
		}
		reconcileInflight.WithLabelValues(controllerName).Set(0)
		for _, status := range []string{"building", "succeeded", "failed", "error"} {
			buildStatusTotal.WithLabelValues(status).Add(0)
		}
	})
}
