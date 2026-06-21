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
	if ext == nil || ext.Metric.Name != "istio_requests_per_second" {
		t.Fatalf("expected istio_requests_per_second external metric, got %+v", ext)
	}
	if ext.Metric.Selector == nil || ext.Metric.Selector.MatchLabels["destination_workload"] != "app" {
		t.Fatalf("expected destination_workload=app selector, got %+v", ext.Metric.Selector)
	}
	if ext.Target.Type != autoscalingv2.AverageValueMetricType ||
		ext.Target.AverageValue == nil || ext.Target.AverageValue.Value() != 100 {
		t.Fatalf("expected AverageValue=100 target, got %+v", ext.Target)
	}
}
