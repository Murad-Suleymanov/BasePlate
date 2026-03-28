package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

var envoyFilterGVK = schema.GroupVersionKind{
	Group:   "networking.istio.io",
	Version: "v1alpha3",
	Kind:    "EnvoyFilter",
}

func envoyFilterRateLimitName(bs *deployv1alpha1.BirService) string {
	return fmt.Sprintf("%s-ratelimit", bs.Name)
}

func wantsLocalRateLimit(bs *deployv1alpha1.BirService) bool {
	if bs.Spec.Traffic == nil || bs.Spec.Traffic.RateLimit == nil || !bs.Spec.Traffic.RateLimit.Enabled {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(bs.Spec.Traffic.RateLimit.Mode))
	if mode == "" {
		mode = "local"
	}
	if mode != "local" {
		return false
	}
	if bs.Spec.Traffic.RateLimit.Local == nil || bs.Spec.Traffic.RateLimit.Local.RequestsPerSecond < 1 {
		return false
	}
	return true
}

// reconcileEnvoyFilterRateLimit creates or updates an Istio EnvoyFilter with Envoy HTTP local rate limit
// for inbound traffic to the BirService pods, or deletes it when disabled / unsupported.
func (r *BirServiceReconciler) reconcileEnvoyFilterRateLimit(ctx context.Context, bs *deployv1alpha1.BirService) error {
	l := log.FromContext(ctx)
	name := envoyFilterRateLimitName(bs)
	key := types.NamespacedName{Name: name, Namespace: bs.Namespace}

	if bs.Spec.Traffic != nil && bs.Spec.Traffic.RateLimit != nil && bs.Spec.Traffic.RateLimit.Enabled {
		mode := strings.ToLower(strings.TrimSpace(bs.Spec.Traffic.RateLimit.Mode))
		if mode == "" {
			mode = "local"
		}
		if mode != "local" {
			l.Info("spec.traffic.rateLimit.mode is not supported; removing EnvoyFilter if present", "mode", mode)
			return r.deleteEnvoyFilterIfExists(ctx, key)
		}
		if bs.Spec.Traffic.RateLimit.Local == nil || bs.Spec.Traffic.RateLimit.Local.RequestsPerSecond < 1 {
			l.Info("spec.traffic.rateLimit.local.requestsPerSecond must be >= 1; removing EnvoyFilter if present")
			return r.deleteEnvoyFilterIfExists(ctx, key)
		}
	}

	if !wantsLocalRateLimit(bs) {
		return r.deleteEnvoyFilterIfExists(ctx, key)
	}

	rl := bs.Spec.Traffic.RateLimit.Local
	rps := rl.RequestsPerSecond
	var burst int32
	if rl.Burst != nil {
		burst = *rl.Burst
	}
	maxTokens := rps + burst
	if maxTokens < 1 {
		maxTokens = 1
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(envoyFilterGVK)
	u.SetName(name)
	u.SetNamespace(bs.Namespace)
	u.SetLabels(map[string]string{
		"app.kubernetes.io/name":       bs.Name,
		"app.kubernetes.io/managed-by": "easy-deploy-operator",
	})

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, u, func() error {
		u.SetLabels(map[string]string{
			"app.kubernetes.io/name":       bs.Name,
			"app.kubernetes.io/managed-by": "easy-deploy-operator",
		})
		if err := unstructured.SetNestedField(u.Object, buildEnvoyFilterSpec(bs, rps, maxTokens), "spec"); err != nil {
			return err
		}
		return ctrl.SetControllerReference(bs, u, r.Scheme)
	})
	return err
}

func (r *BirServiceReconciler) deleteEnvoyFilterIfExists(ctx context.Context, key types.NamespacedName) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(envoyFilterGVK)
	if err := r.Get(ctx, key, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return r.Delete(ctx, u)
}

func buildEnvoyFilterSpec(bs *deployv1alpha1.BirService, tokensPerFill, maxTokens int32) map[string]interface{} {
	statPrefix := strings.ReplaceAll(bs.Name, "-", "_") + "_local_ratelimit"
	return map[string]interface{}{
		"workloadSelector": map[string]interface{}{
			"labels": map[string]interface{}{
				"app.kubernetes.io/name": bs.Name,
			},
		},
		"configPatches": []interface{}{
			map[string]interface{}{
				"applyTo": "HTTP_FILTER",
				"match": map[string]interface{}{
					"context": "SIDECAR_INBOUND",
					"listener": map[string]interface{}{
						"filterChain": map[string]interface{}{
							"filter": map[string]interface{}{
								"name": "envoy.filters.network.http_connection_manager",
								"subFilter": map[string]interface{}{
									"name": "envoy.filters.http.router",
								},
							},
						},
					},
				},
				"patch": map[string]interface{}{
					"operation": "INSERT_BEFORE",
					"value": map[string]interface{}{
						"name": "envoy.filters.http.local_ratelimit",
						"typed_config": map[string]interface{}{
							"@type": "type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit",
							"stat_prefix": statPrefix,
							"token_bucket": map[string]interface{}{
								// unstructured JSON dərin kopiyası int32 qəbul etmir — int64 istifadə et
								"max_tokens":      int64(maxTokens),
								"tokens_per_fill": int64(tokensPerFill),
								"fill_interval": map[string]interface{}{
									"seconds": int64(1),
								},
							},
							"filter_enabled": map[string]interface{}{
								"runtime_key": "local_rate_limit_enabled",
								"default_value": map[string]interface{}{
									"numerator":   int64(100),
									"denominator": "HUNDRED",
								},
							},
							"filter_enforced": map[string]interface{}{
								"runtime_key": "local_rate_limit_enforced",
								"default_value": map[string]interface{}{
									"numerator":   int64(100),
									"denominator": "HUNDRED",
								},
							},
						},
					},
				},
			},
		},
	}
}
