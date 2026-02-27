package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

type BirServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *BirServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var bs deployv1alpha1.BirService
	if err := r.Get(ctx, req.NamespacedName, &bs); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	image, err := resolveImage(&bs)
	if err != nil {
		l.Error(err, "invalid image configuration")
		return ctrl.Result{}, nil
	}

	replicas := int32(1)
	if bs.Spec.Replicas != nil {
		replicas = *bs.Spec.Replicas
	}

	port := int32(80)
	if bs.Spec.Port != nil && *bs.Spec.Port > 0 {
		port = *bs.Spec.Port
	}

	containerPort := port
	if bs.Spec.ContainerPort != nil && *bs.Spec.ContainerPort > 0 {
		containerPort = *bs.Spec.ContainerPort
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       bs.Name,
		"app.kubernetes.io/managed-by": "easy-deploy-operator",
		"deploy.easydeploy.io/tenant":  bs.Namespace,
	}

	depName := fmt.Sprintf("%s-deploy", bs.Name)
	var dep appsv1.Deployment
	depKey := types.NamespacedName{Name: depName, Namespace: bs.Namespace}
	if err := r.Get(ctx, depKey, &dep); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, &dep, func() error {
		dep.ObjectMeta.Name = depName
		dep.ObjectMeta.Namespace = bs.Namespace
		dep.ObjectMeta.Labels = mergeStringMap(dep.ObjectMeta.Labels, labels)

		replicasCopy := replicas
		dep.Spec.Replicas = &replicasCopy
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template.ObjectMeta.Labels = labels
		dep.Spec.Template.Spec.Containers = []corev1.Container{
			{
				Name:  "app",
				Image: image,
				Ports: []corev1.ContainerPort{
					{ContainerPort: containerPort},
				},
			},
		}

		return ctrl.SetControllerReference(&bs, &dep, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	svcName := fmt.Sprintf("%s-svc", bs.Name)
	var svc corev1.Service
	svcKey := types.NamespacedName{Name: svcName, Namespace: bs.Namespace}
	if err := r.Get(ctx, svcKey, &svc); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, &svc, func() error {
		svc.ObjectMeta.Name = svcName
		svc.ObjectMeta.Namespace = bs.Namespace
		svc.ObjectMeta.Labels = mergeStringMap(svc.ObjectMeta.Labels, labels)

		svc.Spec.Selector = labels
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "http",
				Port:       port,
				TargetPort: intstr.FromInt(int(containerPort)),
				Protocol:   corev1.ProtocolTCP,
			},
		}

		return ctrl.SetControllerReference(&bs, &svc, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// Refresh deployment to get status.
	if err := r.Get(ctx, depKey, &dep); err != nil {
		return ctrl.Result{}, err
	}

	if bs.Status.AvailableReplicas != dep.Status.AvailableReplicas {
		bs.Status.AvailableReplicas = dep.Status.AvailableReplicas
		if err := r.Status().Update(ctx, &bs); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *BirServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&deployv1alpha1.BirService{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

func resolveImage(bs *deployv1alpha1.BirService) (string, error) {
	if bs.Spec.Image != "" {
		return bs.Spec.Image, nil
	}
	if bs.Spec.Repo == "" {
		return "", fmt.Errorf("spec.image is empty and spec.repo is empty")
	}
	tag := bs.Spec.Tag
	if tag == "" {
		tag = "latest"
	}
	return fmt.Sprintf("%s:%s", bs.Spec.Repo, tag), nil
}

func mergeStringMap(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
