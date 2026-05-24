package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

var destinationRuleGVK = schema.GroupVersionKind{
	Group:   "networking.istio.io",
	Version: "v1beta1",
	Kind:    "DestinationRule",
}

// destinationRuleName keeps the legacy "-outlier" suffix so existing DRs are
// adopted in place when ownership transitions across operator versions.
func destinationRuleName(bs *deployv1alpha1.BirService) string {
	return fmt.Sprintf("%s-outlier", bs.Name)
}

// reconcileDestinationRule composes the per-service Istio DestinationRule from all
// mesh traffic policies the operator manages (outlier detection, load-balancer
// algorithm, …). One DR per service: each section is added only when its driver
// is enabled, and the whole DR is deleted when no section is needed (or mesh is off).
func (r *BirServiceReconciler) reconcileDestinationRule(ctx context.Context, bs *deployv1alpha1.BirService) error {
	l := log.FromContext(ctx)
	name := destinationRuleName(bs)
	key := types.NamespacedName{Name: name, Namespace: bs.Namespace}

	if !bsNeedsWaypoint(bs) {
		return r.deleteDestinationRuleIfExists(ctx, key)
	}

	trafficPolicy := buildTrafficPolicy(bs)
	if len(trafficPolicy) == 0 {
		return r.deleteDestinationRuleIfExists(ctx, key)
	}

	host := fmt.Sprintf("%s-svc.%s.svc.cluster.local", bs.Name, bs.Namespace)
	l.Info("applying Istio DestinationRule", "name", name, "namespace", bs.Namespace, "host", host)

	dr := &unstructured.Unstructured{}
	dr.SetGroupVersionKind(destinationRuleGVK)
	dr.SetName(name)
	dr.SetNamespace(bs.Namespace)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dr, func() error {
		dr.SetLabels(map[string]string{
			"app.kubernetes.io/name":       bs.Name,
			"app.kubernetes.io/managed-by": "easy-deploy-operator",
		})
		dr.Object["spec"] = map[string]interface{}{
			"host":          host,
			"trafficPolicy": trafficPolicy,
		}
		return ctrl.SetControllerReference(bs, dr, r.Scheme)
	})
	return err
}

func buildTrafficPolicy(bs *deployv1alpha1.BirService) map[string]interface{} {
	policy := map[string]interface{}{}

	// Outlier detection: on by default for mesh workloads, opt-out via ejectUnhealthy: false.
	if bs.Spec.Traffic.EjectUnhealthy == nil || *bs.Spec.Traffic.EjectUnhealthy {
		policy["outlierDetection"] = map[string]interface{}{
			"consecutive5xxErrors":     int64(5),
			"consecutiveGatewayErrors": int64(3),
			"interval":                 "10s",
			"baseEjectionTime":         "30s",
			"maxEjectionPercent":       int64(50),
			"minHealthPercent":         int64(0),
		}
	}

	// Load balancer: omit entirely when latencyAware is false/unset so Istio uses its
	// default (round-robin). Setting LEAST_REQUEST switches to power-of-two-choices.
	if bs.Spec.Traffic.LatencyAware != nil && *bs.Spec.Traffic.LatencyAware {
		policy["loadBalancer"] = map[string]interface{}{
			"simple": "LEAST_REQUEST",
		}
	}

	return policy
}

func (r *BirServiceReconciler) deleteDestinationRuleIfExists(ctx context.Context, key types.NamespacedName) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(destinationRuleGVK)
	if err := r.Get(ctx, key, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return r.Delete(ctx, u)
}
