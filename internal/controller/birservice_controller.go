package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

const (
	registryURL    = "registry.registry.svc.cluster.local:5000"
	kanikoImage    = "gcr.io/kaniko-project/executor:latest"
	labelBuildTag  = "deploy.easydeploy.io/build-tag"
	labelPurpose   = "deploy.easydeploy.io/purpose"
	annotRebuild   = "deploy.easydeploy.io/rebuild"
	requeueBuild   = 10 * time.Second
)

type BirServiceReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	BaseDomain string
	TargetIP   string
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

	// If repo is a Git URL, handle the build-then-deploy flow.
	if isGitURL(bs.Spec.Repo) {
		return r.reconcileBuild(ctx, req, &bs)
	}

	image, err := resolveImage(&bs)
	if err != nil {
		l.Error(err, "invalid image configuration")
		return ctrl.Result{}, nil
	}

	return r.reconcileDeployment(ctx, req, &bs, image)
}

func (r *BirServiceReconciler) reconcileDeployment(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService, image string) (ctrl.Result, error) {
	replicas := int32(1)
	if bs.Spec.Replicas != nil {
		replicas = *bs.Spec.Replicas
	}

	port := int32(8080)
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
	depKey := types.NamespacedName{Name: depName, Namespace: bs.Namespace}

	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		dep := appsv1.Deployment{}
		dep.Name = depName
		dep.Namespace = bs.Namespace

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, &dep, func() error {
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

			return ctrl.SetControllerReference(bs, &dep, r.Scheme)
		})
		return err
	}); err != nil {
		return ctrl.Result{}, err
	}

	svcName := fmt.Sprintf("%s-svc", bs.Name)
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
					TargetPort: intstr.FromInt(int(containerPort)),
					Protocol:   corev1.ProtocolTCP,
				},
			}

			return ctrl.SetControllerReference(bs, &svc, r.Scheme)
		})
		return err
	}); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileHTTPRoute(ctx, bs, svcName, port); err != nil {
		return ctrl.Result{}, err
	}

	var dep appsv1.Deployment
	if err := r.Get(ctx, depKey, &dep); err != nil {
		return ctrl.Result{}, err
	}

	if bs.Status.AvailableReplicas != dep.Status.AvailableReplicas {
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var latest deployv1alpha1.BirService
			if err := r.Get(ctx, req.NamespacedName, &latest); err != nil {
				return err
			}
			latest.Status.AvailableReplicas = dep.Status.AvailableReplicas
			return r.Status().Update(ctx, &latest)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// reconcileBuild handles Git-based repos: creates a Kaniko build Job, waits for
// completion, then deploys the built image from the local registry.
// It detects tag changes and rebuild annotations to trigger new builds.
func (r *BirServiceReconciler) reconcileBuild(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	tag := bs.Spec.Tag
	if tag == "" {
		tag = "main"
	}
	buildImage := fmt.Sprintf("%s/%s:%s", registryURL, bs.Name, tag)

	needsRebuild := r.needsRebuild(bs, tag)

	if needsRebuild {
		l.Info("rebuild triggered, cleaning old build jobs", "tag", tag)
		if err := r.deleteOldBuildJobs(ctx, bs); err != nil {
			return ctrl.Result{}, err
		}
	}

	jobName := fmt.Sprintf("%s-build-%s", bs.Name, sanitizeK8sName(tag))
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: bs.Namespace}, &job)

	if apierrors.IsNotFound(err) {
		l.Info("creating Kaniko build job", "repo", bs.Spec.Repo, "image", buildImage)

		dockerfile := bs.Spec.Dockerfile
		if dockerfile == "" {
			dockerfile = "Dockerfile"
		}

		gitContext := fmt.Sprintf("git://%s#refs/heads/%s", strings.TrimPrefix(bs.Spec.Repo, "https://"), tag)

		args := []string{
			"--context=" + gitContext,
			"--dockerfile=" + dockerfile,
			"--destination=" + buildImage,
			"--insecure",
			"--cache=false",
		}

		job = batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: bs.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":       bs.Name,
					"app.kubernetes.io/managed-by": "easy-deploy-operator",
					labelPurpose:                   "build",
					labelBuildTag:                  tag,
				},
			},
			Spec: batchv1.JobSpec{
				BackoffLimit: int32Ptr(2),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{
							{
								Name:  "kaniko",
								Image: kanikoImage,
								Args:  args,
							},
						},
					},
				},
			},
		}

		if err := ctrl.SetControllerReference(bs, &job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &job); err != nil {
			return ctrl.Result{}, err
		}

		return r.updateBuildStatus(ctx, req, bs, buildImage, "Building", tag)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if job.Status.Succeeded > 0 {
		if bs.Status.BuildStatus != "Succeeded" || bs.Status.BuildTag != tag {
			if _, err := r.updateBuildStatus(ctx, req, bs, buildImage, "Succeeded", tag); err != nil {
				return ctrl.Result{}, err
			}
		}
		l.Info("build succeeded, deploying", "image", buildImage)
		return r.reconcileDeployment(ctx, req, bs, buildImage)
	}

	if job.Status.Failed > 0 {
		l.Error(nil, "build job failed", "job", jobName)
		return r.updateBuildStatus(ctx, req, bs, buildImage, "Failed", tag)
	}

	l.Info("build job still running", "job", jobName)
	return ctrl.Result{RequeueAfter: requeueBuild}, nil
}

func (r *BirServiceReconciler) needsRebuild(bs *deployv1alpha1.BirService, desiredTag string) bool {
	if bs.Status.BuildTag != "" && bs.Status.BuildTag != desiredTag {
		return true
	}
	rebuildAnnot := bs.Annotations[annotRebuild]
	if rebuildAnnot != "" && rebuildAnnot != bs.Status.LastRebuild {
		return true
	}
	return false
}

func (r *BirServiceReconciler) deleteOldBuildJobs(ctx context.Context, bs *deployv1alpha1.BirService) error {
	sel, _ := labels.Parse(fmt.Sprintf(
		"app.kubernetes.io/name=%s,%s=build",
		bs.Name, labelPurpose,
	))
	var jobList batchv1.JobList
	if err := r.List(ctx, &jobList, &client.ListOptions{
		Namespace:     bs.Namespace,
		LabelSelector: sel,
	}); err != nil {
		return err
	}
	bg := metav1.DeletePropagationBackground
	for i := range jobList.Items {
		if err := r.Delete(ctx, &jobList.Items[i], &client.DeleteOptions{
			PropagationPolicy: &bg,
		}); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func (r *BirServiceReconciler) updateBuildStatus(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService, image, status, tag string) (ctrl.Result, error) {
	return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latest deployv1alpha1.BirService
		if err := r.Get(ctx, req.NamespacedName, &latest); err != nil {
			return err
		}
		latest.Status.BuildImage = image
		latest.Status.BuildStatus = status
		latest.Status.BuildTag = tag
		if ann := latest.Annotations[annotRebuild]; ann != "" {
			latest.Status.LastRebuild = ann
		}
		return r.Status().Update(ctx, &latest)
	})
}

var k8sNameRegex = regexp.MustCompile(`[^a-z0-9-]`)

func sanitizeK8sName(s string) string {
	s = strings.ToLower(s)
	s = k8sNameRegex.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

func (r *BirServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&deployv1alpha1.BirService{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

func isGitURL(repo string) bool {
	if repo == "" {
		return false
	}
	return strings.HasPrefix(repo, "https://github.com/") ||
		strings.HasPrefix(repo, "https://gitlab.com/") ||
		strings.HasPrefix(repo, "https://bitbucket.org/") ||
		strings.HasSuffix(repo, ".git")
}

func resolveImage(bs *deployv1alpha1.BirService) (string, error) {
	if bs.Spec.Image != "" {
		return bs.Spec.Image, nil
	}
	if bs.Status.BuildImage != "" {
		return bs.Status.BuildImage, nil
	}
	if bs.Spec.Repo == "" {
		return "", fmt.Errorf("one of spec.image or spec.repo is required")
	}
	tag := bs.Spec.Tag
	if tag == "" {
		tag = "latest"
	}
	return fmt.Sprintf("%s:%s", bs.Spec.Repo, tag), nil
}

func int32Ptr(i int32) *int32 { return &i }

var httpRouteGVK = schema.GroupVersionKind{
	Group:   "gateway.networking.k8s.io",
	Version: "v1",
	Kind:    "HTTPRoute",
}

const (
	gatewayName      = "main-gateway"
	gatewayNamespace = "nginx-gateway"
)

func (r *BirServiceReconciler) resolveHostname(bs *deployv1alpha1.BirService) string {
	if bs.Spec.Hostname != "" {
		return bs.Spec.Hostname
	}
	if r.BaseDomain != "" {
		return fmt.Sprintf("%s-%s.%s", bs.Name, bs.Namespace, r.BaseDomain)
	}
	return ""
}

func (r *BirServiceReconciler) reconcileHTTPRoute(ctx context.Context, bs *deployv1alpha1.BirService, svcName string, svcPort int32) error {
	routeName := fmt.Sprintf("%s-route", bs.Name)
	hostname := r.resolveHostname(bs)

	if hostname == "" {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(httpRouteGVK)
		err := r.Get(ctx, types.NamespacedName{Name: routeName, Namespace: bs.Namespace}, existing)
		if err == nil {
			return r.Delete(ctx, existing)
		}
		return client.IgnoreNotFound(err)
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(httpRouteGVK)
		route.SetName(routeName)
		route.SetNamespace(bs.Namespace)

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
			route.Object["spec"] = map[string]interface{}{
				"parentRefs": []interface{}{
					map[string]interface{}{
						"name":        gatewayName,
						"namespace":   gatewayNamespace,
						"sectionName": "http",
					},
					map[string]interface{}{
						"name":        gatewayName,
						"namespace":   gatewayNamespace,
						"sectionName": "https",
					},
				},
				"hostnames": []interface{}{hostname},
				"rules": []interface{}{
					map[string]interface{}{
						"backendRefs": []interface{}{
							map[string]interface{}{
								"name": svcName,
								"port": int64(svcPort),
							},
						},
					},
				},
			}

			route.SetLabels(mergeStringMap(route.GetLabels(), map[string]string{
				"app.kubernetes.io/name":       bs.Name,
				"app.kubernetes.io/managed-by": "easy-deploy-operator",
			}))

			if r.TargetIP != "" {
				route.SetAnnotations(mergeStringMap(route.GetAnnotations(), map[string]string{
					"external-dns.alpha.kubernetes.io/target": r.TargetIP,
				}))
			}

			return ctrl.SetControllerReference(bs, route, r.Scheme)
		})
		return err
	})
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
