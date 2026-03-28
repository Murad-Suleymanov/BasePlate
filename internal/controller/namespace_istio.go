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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

const (
	labelIstioInjection = "istio-injection"
	istioInjectEnabled  = "enabled"
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
		if ns.Labels[labelIstioInjection] == istioInjectEnabled {
			return nil
		}
		labelJustEnabled = true
		ns.Labels[labelIstioInjection] = istioInjectEnabled
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
	if ns.Labels[labelIstioInjection] != istioInjectEnabled {
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
	for _, p := range podList.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		hasProxy := false
		for _, c := range p.Spec.Containers {
			if c.Name == "istio-proxy" {
				hasProxy = true
				break
			}
		}
		if !hasProxy {
			return true, nil
		}
	}
	return false, nil
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
