package controller

import (
	"testing"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
)

func TestHPAMetricsDefaultsToCPU(t *testing.T) {
	bs := &deployv1alpha1.BirService{}
	bs.Namespace = "tenant-a"

	for _, hpa := range []*deployv1alpha1.HPASpec{
		nil,
		{MinReplicas: int32p(1), MaxReplicas: int32p(3)},                       // no targetRPS
		{MinReplicas: int32p(1), MaxReplicas: int32p(3), TargetRPS: int32p(0)}, // zero ignored
	} {
		bs.Spec.HPA = hpa
		metrics := hpaMetrics(bs, "app")
		if len(metrics) != 1 || metrics[0].Type != autoscalingv2.ResourceMetricSourceType {
			t.Fatalf("hpa %+v: expected a single Resource metric, got %+v", hpa, metrics)
		}
		res := metrics[0].Resource
		if res == nil || res.Name != corev1.ResourceCPU {
			t.Fatalf("expected CPU resource metric, got %+v", res)
		}
		if res.Target.Type != autoscalingv2.UtilizationMetricType ||
			res.Target.AverageUtilization == nil || *res.Target.AverageUtilization != 80 {
			t.Fatalf("expected 80%% CPU utilization target, got %+v", res.Target)
		}
	}
}

func TestHPAMetricsRPSExternal(t *testing.T) {
	bs := &deployv1alpha1.BirService{}
	bs.Namespace = "tenant-a"
	bs.Spec.HPA = &deployv1alpha1.HPASpec{
		MinReplicas: int32p(2),
		MaxReplicas: int32p(8),
		TargetRPS:   int32p(100),
	}

	metrics := hpaMetrics(bs, "app")
	if len(metrics) != 1 || metrics[0].Type != autoscalingv2.ExternalMetricSourceType {
		t.Fatalf("expected a single External metric, got %+v", metrics)
	}
	ext := metrics[0].External
	// Default window (1m) → the adapter serves istio_requests_per_second_1m.
	if ext == nil || ext.Metric.Name != "istio_requests_per_second_1m" {
		t.Fatalf("expected istio_requests_per_second_1m external metric, got %+v", ext)
	}
	if ext.Metric.Selector == nil || ext.Metric.Selector.MatchLabels["destination_workload"] != "app" {
		t.Fatalf("expected destination_workload=app selector, got %+v", ext.Metric.Selector)
	}
	// The window is now encoded in the metric name, not a selector label.
	if _, ok := ext.Metric.Selector.MatchLabels["window"]; ok {
		t.Fatalf("did not expect a window selector label, got %+v", ext.Metric.Selector)
	}
	if ext.Target.Type != autoscalingv2.AverageValueMetricType ||
		ext.Target.AverageValue == nil || ext.Target.AverageValue.Value() != 100 {
		t.Fatalf("expected AverageValue=100 target, got %+v", ext.Target)
	}
}

func TestHPAMetricsScaleTypeMemory(t *testing.T) {
	bs := &deployv1alpha1.BirService{}
	bs.Namespace = "tenant-a"
	bs.Spec.HPA = &deployv1alpha1.HPASpec{
		MinReplicas: int32p(2),
		MaxReplicas: int32p(10),
		ScaleType:   "memory",
		Target:      75,
	}

	metrics := hpaMetrics(bs, "app")
	if len(metrics) != 1 || metrics[0].Type != autoscalingv2.ResourceMetricSourceType {
		t.Fatalf("expected a single Resource metric, got %+v", metrics)
	}
	res := metrics[0].Resource
	if res == nil || res.Name != corev1.ResourceMemory ||
		res.Target.AverageUtilization == nil || *res.Target.AverageUtilization != 75 {
		t.Fatalf("expected memory util 75%%, got %+v", res)
	}
}

// cpu/memory without an explicit target default to 80%.
func TestHPAMetricsScaleTypeCPUDefaultsTarget(t *testing.T) {
	bs := &deployv1alpha1.BirService{}
	bs.Namespace = "tenant-a"
	bs.Spec.HPA = &deployv1alpha1.HPASpec{MinReplicas: int32p(1), MaxReplicas: int32p(3), ScaleType: "cpu"}

	metrics := hpaMetrics(bs, "app")
	res := metrics[0].Resource
	if res == nil || res.Name != corev1.ResourceCPU ||
		res.Target.AverageUtilization == nil || *res.Target.AverageUtilization != 80 {
		t.Fatalf("expected CPU util default 80%%, got %+v", res)
	}
}

func TestHPAMetricsScaleTypeRPSAndWorker(t *testing.T) {
	bs := &deployv1alpha1.BirService{}
	bs.Namespace = "tenant-a"

	bs.Spec.HPA = &deployv1alpha1.HPASpec{MinReplicas: int32p(2), MaxReplicas: int32p(8), ScaleType: "rps", Target: 100}
	if m := hpaMetrics(bs, "app"); len(m) != 1 || m[0].Type != autoscalingv2.ExternalMetricSourceType ||
		m[0].External.Metric.Name != "istio_requests_per_second_1m" || m[0].External.Target.AverageValue.Value() != 100 {
		t.Fatalf("expected rps external 100, got %+v", m)
	}

	// A custom window is encoded in the metric name (no recording rule / selector label).
	bs.Spec.HPA = &deployv1alpha1.HPASpec{MinReplicas: int32p(2), MaxReplicas: int32p(8), ScaleType: "rps", Target: 100, Window: "5m"}
	if m := hpaMetrics(bs, "app"); len(m) != 1 || m[0].External == nil ||
		m[0].External.Metric.Name != "istio_requests_per_second_5m" {
		t.Fatalf("expected window-suffixed metric istio_requests_per_second_5m, got %+v", m)
	}

	// worker target is a utilization % (per-pod), served as a Pods metric.
	bs.Spec.HPA = &deployv1alpha1.HPASpec{MinReplicas: int32p(2), MaxReplicas: int32p(8), ScaleType: "worker", Target: 70}
	if m := hpaMetrics(bs, "app"); len(m) != 1 || m[0].Type != autoscalingv2.PodsMetricSourceType ||
		m[0].Pods.Metric.Name != "app_worker_utilization" || m[0].Pods.Target.AverageValue.Value() != 70 {
		t.Fatalf("expected worker pods 70%%, got %+v", m)
	}

	// worker, like cpu/memory, defaults to 80% when target is omitted.
	bs.Spec.HPA = &deployv1alpha1.HPASpec{MinReplicas: int32p(2), MaxReplicas: int32p(8), ScaleType: "worker"}
	if m := hpaMetrics(bs, "app"); len(m) != 1 || m[0].Pods == nil ||
		m[0].Pods.Target.AverageValue.Value() != 80 {
		t.Fatalf("expected worker default 80%%, got %+v", m)
	}
}

// scaleType wins over the legacy targetRPS.
func TestHPAMetricsScaleTypeWinsOverLegacy(t *testing.T) {
	bs := &deployv1alpha1.BirService{}
	bs.Namespace = "tenant-a"
	bs.Spec.HPA = &deployv1alpha1.HPASpec{
		MinReplicas: int32p(1),
		MaxReplicas: int32p(5),
		TargetRPS:   int32p(50),
		ScaleType:   "cpu",
		Target:      60,
	}

	metrics := hpaMetrics(bs, "app")
	if len(metrics) != 1 || metrics[0].Resource == nil ||
		metrics[0].Resource.Name != corev1.ResourceCPU || *metrics[0].Resource.Target.AverageUtilization != 60 {
		t.Fatalf("expected scaleType cpu=60 to win over legacy targetRPS, got %+v", metrics)
	}
}
