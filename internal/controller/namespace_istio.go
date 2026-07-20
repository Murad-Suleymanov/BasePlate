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
	labelIstioInjection = "istio-injection"
	istioInjectDisabled = "disabled"
	labelDataplaneMode  = "istio.io/dataplane-mode"
	labelUseWaypoint    = "istio.io/use-waypoint"
	dataplaneAmbient    = "ambient"
	waypointName        = "waypoint"
	// labelRouteGroup marks the pool a pod belongs to. The pool's Service selects
	// this label, so instances sharing a route name are load-balanced as one pool.
	labelRouteGroup = "deploy.easydeploy.io/route-group"
	// labelCanonicalName / labelCanonicalRevision pin a pod's Istio canonical
	// service for telemetry. The Deployment selector uses app.kubernetes.io/name
	// (immutable) set to the per-instance name, which would make every pool
	// instance its own canonical service and mis-attribute traffic. Setting these
	// pod-template labels to the app name groups all instances of one app under it.
	labelCanonicalName     = "service.istio.io/canonical-name"
	labelCanonicalRevision = "service.istio.io/canonical-revision"
	// waypointOptionsCM is the ConfigMap referenced by the waypoint Gateway's
	// spec.infrastructure.parametersRef. Its horizontalPodAutoscaler key carries the
	// HPA spec Istio applies to the auto-generated waypoint Deployment.
	waypointOptionsCM = "waypoint-options"
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
	// In ambient mode there are no sidecars — ztunnel runs node-wide,
	// so pods don't need to be restarted when the namespace gets the label.
	_ = podList
	return false, nil
}

// reconcileWaypointGateway ensures the namespace's Gateway/waypoint resource matches intent.
// Created when at least one BirService in the namespace has spec.traffic (istio mesh intent);
// deleted when no BirService needs it. Multiple BirServices in the same namespace share one
// Gateway, so we count siblings before deleting. Istio spawns a waypoint Pod for the Gateway,
// which handles L7 traffic and emits Zipkin trace spans.
// Replica count is auto-computed on every reconcile: min=2 (HA floor),
// max=meshServiceCount×2 (HPA ceiling). These are written into the waypoint-options
// ConfigMap (horizontalPodAutoscaler key); the Gateway's spec.infrastructure.parametersRef
// points Istio at it, and Istio applies the HPA to the auto-generated waypoint Deployment.
func (r *BirServiceReconciler) reconcileWaypointGateway(ctx context.Context, bs *deployv1alpha1.BirService) error {
	desired := bsNeedsWaypoint(bs) && bs.DeletionTimestamp.IsZero()
	if !desired {
		siblingsNeed, err := r.anySiblingNeedsWaypoint(ctx, bs)
		if err != nil {
			return fmt.Errorf("list BirServices in %s: %w", bs.Namespace, err)
		}
		if !siblingsNeed {
			return r.deleteWaypointGateway(ctx, bs.Namespace)
		}
		// Siblings still need waypoint — fall through to upsert so replica count
		// is recalculated now that this BirService is leaving the mesh.
	}
	minR, maxR, err := r.computeWaypointReplicas(ctx, bs.Namespace)
	if err != nil {
		return err
	}
	return r.upsertWaypointGateway(ctx, bs.Namespace, minR, maxR)
}

// computeWaypointReplicas returns (minReplicas, maxReplicas) for the waypoint Gateway.
// minReplicas is always 2: the HA floor that keeps L7 traffic alive during a node drain.
// maxReplicas = 2 × meshServiceCount (floor 4): gives the CPU-based HPA enough headroom
// to scale proportionally to the number of services in the namespace. The actual replica
// count at runtime is determined by Istio's HPA on CPU metrics — not by pod counts,
// which would require knowing per-service RPS that we don't have statically.
func (r *BirServiceReconciler) computeWaypointReplicas(ctx context.Context, namespace string) (minR, maxR int32, err error) {
	var list deployv1alpha1.BirServiceList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return 2, 4, fmt.Errorf("list BirServices for waypoint sizing: %w", err)
	}
	meshCount := int32(0)
	for i := range list.Items {
		sib := &list.Items[i]
		if bsNeedsWaypoint(sib) && sib.DeletionTimestamp.IsZero() {
			meshCount++
		}
	}
	minR = 2
	maxR = meshCount * 2
	if maxR < 4 {
		maxR = 4
	}
	return minR, maxR, nil
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

func (r *BirServiceReconciler) upsertWaypointGateway(ctx context.Context, namespace string, minReplicas, maxReplicas int32) error {
	// The ConfigMap must exist before the Gateway references it via parametersRef.
	if err := r.upsertWaypointOptions(ctx, namespace, minReplicas, maxReplicas); err != nil {
		return err
	}

	gw := waypointGatewayObject(namespace)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, gw, func() error {
		annotations := gw.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		// "all" covers both service-VIP and workload (pod-IP) traffic. Required because
		// the ingress gateway connects directly to pod IPs (resolved Endpoints), so a
		// service-only waypoint never sees ingress traffic.
		annotations["istio.io/waypoint-for"] = "all"
		gw.SetAnnotations(annotations)
		if err := unstructured.SetNestedField(gw.Object, "istio-waypoint", "spec", "gatewayClassName"); err != nil {
			return err
		}
		// parametersRef points Istio at the ConfigMap holding the HPA spec for the
		// auto-generated waypoint Deployment. group "" = core (ConfigMap).
		paramsRef := map[string]interface{}{
			"group": "",
			"kind":  "ConfigMap",
			"name":  waypointOptionsCM,
		}
		if err := unstructured.SetNestedField(gw.Object, paramsRef, "spec", "infrastructure", "parametersRef"); err != nil {
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
	log.FromContext(ctx).Info("waypoint Gateway upserted", "namespace", namespace, "minReplicas", minReplicas, "maxReplicas", maxReplicas)
	return nil
}

// upsertWaypointOptions reconciles the ConfigMap referenced by the waypoint Gateway's
// parametersRef. Istio reads the horizontalPodAutoscaler key and applies it to the
// auto-generated waypoint Deployment (filling scaleTargetRef itself). We supply min/max
// replicas plus a CPU utilization target so the waypoint scales on actual proxy load.
func (r *BirServiceReconciler) upsertWaypointOptions(ctx context.Context, namespace string, minReplicas, maxReplicas int32) error {
	hpaSpec := fmt.Sprintf(`spec:
  minReplicas: %d
  maxReplicas: %d
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 80
`, minReplicas, maxReplicas)

	cm := &corev1.ConfigMap{}
	cm.Name = waypointOptionsCM
	cm.Namespace = namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"horizontalPodAutoscaler": hpaSpec}
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert waypoint options ConfigMap: %w", err)
	}
	return nil
}

func (r *BirServiceReconciler) deleteWaypointGateway(ctx context.Context, namespace string) error {
	cm := &corev1.ConfigMap{}
	cm.Name = waypointOptionsCM
	cm.Namespace = namespace
	if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete waypoint options ConfigMap: %w", err)
	}

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
