package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

func canaryDepName(bs *deployv1alpha1.BirService) string {
	return bs.Name + "-canary"
}

func canarySvcName(bs *deployv1alpha1.BirService) string {
	return bs.Name + "-canary-svc"
}

// resolveCanaryImage returns the image for the canary deployment.
// Priority: spec.canary.image > spec.canary.tag (replaces tag in stableImage) > status.BuildTag (pipeline).
// Returns "" if no canary image can be determined yet.
func (r *BirServiceReconciler) resolveCanaryImage(bs *deployv1alpha1.BirService, stableImage string) string {
	if bs.Spec.Canary.Image != "" {
		return bs.Spec.Canary.Image
	}
	if bs.Spec.Canary.Tag != "" {
		parts := strings.SplitN(stableImage, ":", 2)
		return parts[0] + ":" + bs.Spec.Canary.Tag
	}
	// Pipeline-based: latest build goes to canary, stable stays on StableTag.
	// BuildTag is updated by pipeline on every push; StableTag is locked to the tag
	// that was current when canary was first enabled.
	if bs.Status.BuildTag != "" && bs.Status.BuildTag != bs.Status.StableTag {
		return fmt.Sprintf("%s/%s:%s", r.effectiveRegistryURL(), bs.Name, bs.Status.BuildTag)
	}
	return ""
}

func canaryWeight(bs *deployv1alpha1.BirService) int32 {
	if bs.Spec.Canary.Weight != nil {
		return *bs.Spec.Canary.Weight
	}
	return 10
}

// reconcileCanary creates/updates the canary Deployment+Service when enabled,
// or deletes them when disabled/absent. Returns (canaySvcName, weight, err);
// weight=0 means canary is inactive and HTTPRoute uses a single backend.
func (r *BirServiceReconciler) reconcileCanary(
	ctx context.Context,
	bs *deployv1alpha1.BirService,
	stableImage string,
	port int32,
) (string, int32, error) {
	depName := canaryDepName(bs)
	svcName := canarySvcName(bs)

	if bs.Spec.Canary == nil || !bs.Spec.Canary.Enabled {
		return "", 0, r.deleteCanaryResources(ctx, bs, depName, svcName)
	}

	image := r.resolveCanaryImage(bs, stableImage)
	if image == "" {
		// No canary image yet (pipeline hasn't built a new version since canary was enabled).
		return "", 0, r.deleteCanaryResources(ctx, bs, depName, svcName)
	}

	weight := canaryWeight(bs)

	// Canary pods use a distinct primary label so the stable Deployment selector
	// (app.kubernetes.io/name: <name>) never matches them.
	labels := map[string]string{
		"app.kubernetes.io/name":       bs.Name + "-canary",
		"app.kubernetes.io/managed-by": "easy-deploy-operator",
		"deploy.easydeploy.io/tenant":  bs.Namespace,
		"deploy.easydeploy.io/variant": "canary",
	}

	resourceReqs, err := resolveContainerResources(bs)
	if err != nil {
		return "", 0, err
	}

	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		dep := appsv1.Deployment{}
		dep.Name = depName
		dep.Namespace = bs.Namespace

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, &dep, func() error {
			dep.ObjectMeta.Labels = mergeStringMap(dep.ObjectMeta.Labels, labels)
			one := int32(1)
			dep.Spec.Replicas = &one
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
			dep.Spec.Template.ObjectMeta.Labels = labels

			templateSpec := &dep.Spec.Template.Spec
			if strings.HasPrefix(image, r.effectiveRegistryURL()+"/") {
				if r.ensureRegistryPushSecret(ctx, bs.Namespace) == nil {
					templateSpec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: registryPushSecretName}}
				}
			}
			templateSpec.Containers = []corev1.Container{
				{
					Name:            "app",
					Image:           image,
					ImagePullPolicy: corev1.PullAlways,
					Ports:           []corev1.ContainerPort{{ContainerPort: port}},
					Resources:       resourceReqs,
				},
			}
			return ctrl.SetControllerReference(bs, &dep, r.Scheme)
		})
		return err
	}); err != nil {
		return "", 0, err
	}

	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		svc := corev1.Service{}
		svc.Name = svcName
		svc.Namespace = bs.Namespace

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, &svc, func() error {
			svc.ObjectMeta.Labels = mergeStringMap(svc.ObjectMeta.Labels, labels)
			svc.Spec.Selector = labels
			svc.Spec.Type = corev1.ServiceTypeClusterIP
			svc.Spec.Ports = []corev1.ServicePort{
				{
					Name:       "http",
					Port:       port,
					TargetPort: intstr.FromInt(int(port)),
					Protocol:   corev1.ProtocolTCP,
				},
			}
			return ctrl.SetControllerReference(bs, &svc, r.Scheme)
		})
		return err
	}); err != nil {
		return "", 0, err
	}

	// Update status.CanaryImage for display.
	if bs.Status.CanaryImage != image {
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var latest deployv1alpha1.BirService
			if err := r.Get(ctx, types.NamespacedName{Name: bs.Name, Namespace: bs.Namespace}, &latest); err != nil {
				return err
			}
			latest.Status.CanaryImage = image
			return r.Status().Update(ctx, &latest)
		}); err != nil {
			return "", 0, err
		}
	}

	return svcName, weight, nil
}

func (r *BirServiceReconciler) deleteCanaryResources(ctx context.Context, bs *deployv1alpha1.BirService, depName, svcName string) error {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: depName, Namespace: bs.Namespace}, dep); err == nil {
		if err := r.Delete(ctx, dep); client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: bs.Namespace}, svc); err == nil {
		if err := r.Delete(ctx, svc); client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	// Clear CanaryImage from status when canary is torn down.
	if bs.Status.CanaryImage != "" {
		_ = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var latest deployv1alpha1.BirService
			if err := r.Get(ctx, types.NamespacedName{Name: bs.Name, Namespace: bs.Namespace}, &latest); err != nil {
				return err
			}
			latest.Status.CanaryImage = ""
			return r.Status().Update(ctx, &latest)
		})
	}

	return nil
}
