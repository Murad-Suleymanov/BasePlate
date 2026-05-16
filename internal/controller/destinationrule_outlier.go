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

func outlierDetectionName(bs *deployv1alpha1.BirService) string {
	return fmt.Sprintf("%s-outlier", bs.Name)
}

// reconcileOutlierDetection applies a DestinationRule with sane platform defaults so
// failing pods are temporarily ejected from the load-balancing pool by the waypoint Envoy.
// Always-on for mesh-enabled BirServices; no developer knobs (mirrors the tracing model).
// Deleted when the workload is not mesh-enabled.
func (r *BirServiceReconciler) reconcileOutlierDetection(ctx context.Context, bs *deployv1alpha1.BirService) error {
	l := log.FromContext(ctx)
	name := outlierDetectionName(bs)
	key := types.NamespacedName{Name: name, Namespace: bs.Namespace}

	if !bsNeedsWaypoint(bs) {
		return r.deleteDestinationRuleIfExists(ctx, key)
	}

	// Escape hatch: spec.traffic.ejectUnhealthy: false disables outlier detection
	// for workloads that legitimately return 5xx (webhooks, batch endpoints).
	if bs.Spec.Traffic.EjectUnhealthy != nil && !*bs.Spec.Traffic.EjectUnhealthy {
		return r.deleteDestinationRuleIfExists(ctx, key)
	}

	host := fmt.Sprintf("%s-svc.%s.svc.cluster.local", bs.Name, bs.Namespace)
	l.Info("applying Istio DestinationRule for outlier detection", "name", name, "namespace", bs.Namespace, "host", host)

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
			"host": host,
			"trafficPolicy": map[string]interface{}{
				"outlierDetection": map[string]interface{}{
					"consecutive5xxErrors": int64(5),
					"interval":             "10s",
					"baseEjectionTime":     "30s",
					"maxEjectionPercent":   int64(50),
				},
			},
		}
		return ctrl.SetControllerReference(bs, dr, r.Scheme)
	})
	return err
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
