package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

const (
	labelIstioInjection    = "istio-injection"
	istioInjectDisabled    = "disabled"
	labelDataplaneMode     = "istio.io/dataplane-mode"
	labelUseWaypoint       = "istio.io/use-waypoint"
	dataplaneAmbient       = "ambient"
	waypointName           = "waypoint"
)

const (
	rolloutRestartAnnotation        = "kubectl.kubernetes.io/restartedAt"
	annotationMeshRolloutGeneration = "deploy.easydeploy.io/mesh-rollout-generation"
)

// reconcileNamespaceIstioInjection sets namespace istio-injection when spec.traffic is set (mesh intent).
// Provider empty or "istio" applies the label; other provider values skip. Build Jobs opt out via annotation.
// Returns true if the label was just set to enabled (existing workloads may need a rollout for sidecars).
func (r *BirServiceReconciler) reconcileNamespaceIstioInjection(ctx context.Context, bs *deployv1alpha1.BirService) (labelJustEnabled bool, err error) {
	l := log.FromContext(ctx)
	if bs.Spec.Traffic == nil {
		return false, nil
	}
	prov := strings.ToLower(strings.TrimSpace(bs.Spec.Traffic.Provider))
	if prov != "" && prov != "istio" {
		l.Info("traffic.provider is not istio; skipping namespace istio-injection label", "provider", bs.Spec.Traffic.Provider)
		return false, nil
	}

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var ns corev1.Namespace
		if err := r.Get(ctx, types.NamespacedName{Name: bs.Namespace}, &ns); err != nil {
			if apierrors.IsNotFound(err) {
				l.Info("namespace not found, skipping istio-injection label", "namespace", bs.Namespace)
				return nil
			}
			return err
		}
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		if ns.Labels[labelDataplaneMode] == dataplaneAmbient {
			return nil
		}
		labelJustEnabled = true
		ns.Labels[labelIstioInjection] = istioInjectDisabled
		ns.Labels[labelDataplaneMode] = dataplaneAmbient
		ns.Labels[labelUseWaypoint] = waypointName
		return r.Update(ctx, &ns)
	})
	return labelJustEnabled, err
}

// meshNeedsRolloutForSidecar returns true when Istio mesh is desired but Running pods lack istio-proxy,
// and we have not already rolled out for this BirService spec generation (chart may pre-set namespace label).
func (r *BirServiceReconciler) meshNeedsRolloutForSidecar(ctx context.Context, bs *deployv1alpha1.BirService) (bool, error) {
	if bs.Spec.Traffic == nil {
		return false, nil
	}
	prov := strings.ToLower(strings.TrimSpace(bs.Spec.Traffic.Provider))
	if prov != "" && prov != "istio" {
		return false, nil
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: bs.Namespace}, &ns); err != nil {
		return false, err
	}
	if ns.Labels[labelDataplaneMode] != dataplaneAmbient {
		return false, nil
	}
	depName := fmt.Sprintf("%s-deploy", bs.Name)
	key := types.NamespacedName{Name: depName, Namespace: bs.Namespace}
	var dep appsv1.Deployment
	if err := r.Get(ctx, key, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	genStr := strconv.FormatInt(bs.Generation, 10)
	if dep.Annotations != nil && dep.Annotations[annotationMeshRolloutGeneration] == genStr {
		return false, nil
	}
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(bs.Namespace), client.MatchingLabels{"app.kubernetes.io/name": bs.Name}); err != nil {
		return false, err
	}
	// ambient mode-da sidecar yoxdur, ztunnel node-da işləyir
	// pod-ların restart-a ehtiyacı yoxdur
	_ = podList
	return false, nil
}

// reconcileWaypointGateway ensures the namespace's Gateway/waypoint resource matches intent.
// Created when at least one BirService in the namespace has spec.traffic (istio mesh intent);
// deleted when no BirService needs it. Multiple BirServices in the same namespace share one
// Gateway, so we count siblings before deleting. Istio spawns a waypoint Pod for the Gateway,
// which handles L7 traffic and emits Zipkin trace spans.
func (r *BirServiceReconciler) reconcileWaypointGateway(ctx context.Context, bs *deployv1alpha1.BirService) error {
	desired := bsNeedsWaypoint(bs) && bs.DeletionTimestamp.IsZero()
	if desired {
		return r.upsertWaypointGateway(ctx, bs.Namespace)
	}

	siblingsNeed, err := r.anySiblingNeedsWaypoint(ctx, bs)
	if err != nil {
		return fmt.Errorf("list BirServices in %s: %w", bs.Namespace, err)
	}
	if siblingsNeed {
		return nil
	}
	return r.deleteWaypointGateway(ctx, bs.Namespace)
}

// bsNeedsWaypoint returns true when this BirService's spec implies mesh intent
// (spec.traffic set + provider empty or "istio").
func bsNeedsWaypoint(bs *deployv1alpha1.BirService) bool {
	if bs.Spec.Traffic == nil {
		return false
	}
	prov := strings.ToLower(strings.TrimSpace(bs.Spec.Traffic.Provider))
	return prov == "" || prov == "istio"
}

func (r *BirServiceReconciler) anySiblingNeedsWaypoint(ctx context.Context, bs *deployv1alpha1.BirService) (bool, error) {
	var list deployv1alpha1.BirServiceList
	if err := r.List(ctx, &list, client.InNamespace(bs.Namespace)); err != nil {
		return false, err
	}
	for i := range list.Items {
		sib := &list.Items[i]
		if sib.UID == bs.UID {
			continue
		}
		if !sib.DeletionTimestamp.IsZero() {
			continue
		}
		if bsNeedsWaypoint(sib) {
			return true, nil
		}
	}
	return false, nil
}

func waypointGatewayObject(namespace string) *unstructured.Unstructured {
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "Gateway",
	})
	gw.SetName(waypointName)
	gw.SetNamespace(namespace)
	return gw
}

func (r *BirServiceReconciler) upsertWaypointGateway(ctx context.Context, namespace string) error {
	gw := waypointGatewayObject(namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, gw, func() error {
		annotations := gw.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["istio.io/waypoint-for"] = "service"
		gw.SetAnnotations(annotations)
		if err := unstructured.SetNestedField(gw.Object, "istio-waypoint", "spec", "gatewayClassName"); err != nil {
			return err
		}
		listeners := []interface{}{
			map[string]interface{}{
				"name":     "mesh",
				"port":     int64(15008),
				"protocol": "HBONE",
			},
		}
		return unstructured.SetNestedSlice(gw.Object, listeners, "spec", "listeners")
	})
	if err != nil {
		return fmt.Errorf("upsert waypoint Gateway: %w", err)
	}
	log.FromContext(ctx).Info("waypoint Gateway upserted", "namespace", namespace)
	return nil
}

func (r *BirServiceReconciler) deleteWaypointGateway(ctx context.Context, namespace string) error {
	gw := waypointGatewayObject(namespace)
	if err := r.Delete(ctx, gw); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete waypoint Gateway: %w", err)
	}
	log.FromContext(ctx).Info("waypoint Gateway deleted (no BirService needs it)", "namespace", namespace)
	return nil
}

// rolloutRestartWorkload sets restartedAt on the app Deployment pod template (same as kubectl rollout restart).
func (r *BirServiceReconciler) rolloutRestartWorkload(ctx context.Context, bs *deployv1alpha1.BirService) error {
	depName := fmt.Sprintf("%s-deploy", bs.Name)
	key := types.NamespacedName{Name: depName, Namespace: bs.Namespace}
	var dep appsv1.Deployment
	if err := r.Get(ctx, key, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	base := dep.DeepCopy()
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[rolloutRestartAnnotation] = time.Now().UTC().Format(time.RFC3339)
	if dep.ObjectMeta.Annotations == nil {
		dep.ObjectMeta.Annotations = map[string]string{}
	}
	dep.ObjectMeta.Annotations[annotationMeshRolloutGeneration] = strconv.FormatInt(bs.Generation, 10)
	return r.Patch(ctx, &dep, client.MergeFrom(base))
}
