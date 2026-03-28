package controller

import (
	"context"
	"fmt"
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

const rolloutRestartAnnotation = "kubectl.kubernetes.io/restartedAt"

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
	return r.Patch(ctx, &dep, client.MergeFrom(base))
}
