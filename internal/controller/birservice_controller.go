package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"easy-deploy/internal/credentials"
	"easy-deploy/internal/injector"
	"easy-deploy/internal/registry"
	"os"
)

const (
	defaultRegistryURL     = "registry.registry.svc.cluster.local:5000"
	kanikoImage            = "gcr.io/kaniko-project/executor:latest"
	registryPushSecretName = "registry-push"
	labelBuildTag          = "deploy.easydeploy.io/build-tag"
	labelPurpose           = "deploy.easydeploy.io/purpose"
	annotRebuild           = "deploy.easydeploy.io/rebuild"
	annotPipelineInj       = "deploy.easydeploy.io/pipeline-injected"
	requeueBuild           = 10 * time.Second
)

type BirServiceReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	BaseDomain  string
	TargetIP    string
	RegistryURL string
	Environment string
}

func (r *BirServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	l := log.FromContext(ctx)
	start := time.Now()
	reconcileInflight.WithLabelValues(controllerName).Inc()
	defer func() {
		reconcileInflight.WithLabelValues(controllerName).Dec()
		result := classifyReconcileResult(res, err)
		reconcileTotal.WithLabelValues(controllerName, result).Inc()
		reconcileDuration.WithLabelValues(controllerName, result).Observe(time.Since(start).Seconds())
	}()

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
	// Sync StableTag:
	// - Canary just enabled and StableTag not yet set → lock current BuildTag as stable.
	// - Canary disabled and StableTag is set → promote: clear StableTag (BuildTag is now stable).
	if bs.Spec.Canary != nil && bs.Spec.Canary.Enabled {
		if bs.Status.StableTag == "" && bs.Status.BuildTag != "" {
			if err := r.updateStableTag(ctx, req, bs, bs.Status.BuildTag); err != nil {
				return ctrl.Result{}, err
			}
			bs.Status.StableTag = bs.Status.BuildTag
		}
	} else if bs.Status.StableTag != "" {
		if err := r.updateStableTag(ctx, req, bs, ""); err != nil {
			return ctrl.Result{}, err
		}
		bs.Status.StableTag = ""
	}

	// When canary is active, stable deployment must stay on the locked StableTag,
	// not on the latest build image that the pipeline just pushed.
	if bs.Spec.Canary != nil && bs.Spec.Canary.Enabled && bs.Status.StableTag != "" {
		image = fmt.Sprintf("%s/%s:%s", r.effectiveRegistryURL(), bs.Name, bs.Status.StableTag)
	}

	depName := fmt.Sprintf("%s-deploy", bs.Name)
	depKey := types.NamespacedName{Name: depName, Namespace: bs.Namespace}
	var preExist appsv1.Deployment
	deploymentExisted := r.Get(ctx, depKey, &preExist) == nil

	labelJustEnabled, err := r.reconcileNamespaceIstioInjection(ctx, bs)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileWaypointGateway(ctx, bs); err != nil {
		return ctrl.Result{}, err
	}

	minReplicas, maxReplicas, useHPA := resolveHPAConfig(bs)
	var replicas *int32
	if !useHPA {
		r := int32(1)
		if bs.Spec.Replicas != nil {
			r = *bs.Spec.Replicas
		}
		replicas = &r
	}
	resourceReqs, err := resolveContainerResources(bs)
	if err != nil {
		return ctrl.Result{}, err
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

	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		dep := appsv1.Deployment{}
		dep.Name = depName
		dep.Namespace = bs.Namespace

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, &dep, func() error {
			dep.ObjectMeta.Labels = mergeStringMap(dep.ObjectMeta.Labels, labels)

			// When HPA is in charge, we don't set replicas — HPA owns it.
			if replicas != nil {
				dep.Spec.Replicas = replicas
			}
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
			// Pod template labels are a superset of selector labels (selector is immutable).
			// In ambient mode the use-waypoint label must be on the pod, not just the service:
			// the ingress gateway resolves Endpoints and connects to pod IPs, which bypasses
			// the service-VIP waypoint binding. Pod-level label routes pod-IP traffic via waypoint too.
			templateLabels := mergeStringMap(map[string]string{}, labels)
			if bsNeedsWaypoint(bs) {
				templateLabels[labelUseWaypoint] = waypointName
			}
			dep.Spec.Template.ObjectMeta.Labels = templateLabels
			dep.Spec.Template.ObjectMeta.Annotations = r.tracingAnnotations(ctx, bs, dep.Spec.Template.ObjectMeta.Annotations)

			// If the image lives in our internal registry, mount the registry-push secret so pods can pull it.
			templateSpec := &dep.Spec.Template.Spec
			if strings.HasPrefix(image, r.effectiveRegistryURL()+"/") {
				if r.ensureRegistryPushSecret(ctx, bs.Namespace) == nil {
					templateSpec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: registryPushSecretName}}
				}
			}

			dep.Spec.Strategy = resolveDeploymentStrategy(bs)
			dep.Spec.MinReadySeconds = 5
			dep.Spec.ProgressDeadlineSeconds = int32Ptr(600)
			dep.Spec.RevisionHistoryLimit = int32Ptr(5)

			// Topology spread: best-effort distribution of this workload's pods across
			// nodes. Soft (ScheduleAnyway) by design — on a single-node cluster it is a
			// no-op and never blocks scheduling or HPA scale-up; once more nodes join,
			// new pods prefer emptier nodes automatically. Hard (DoNotSchedule) would
			// strand replicas as Pending on small clusters, so we never use it.
			//
			// LabelSelector covers all pods of this service (all ReplicaSet versions).
			// MatchLabelKeys (K8s 1.25+) would scope the spread to per-version ReplicaSets
			// so rolling updates land evenly independently — not available with k8s.io/api
			// v0.20; upgrade the dependency to enable it.
			templateSpec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           1,
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
					MatchLabelKeys:    []string{"pod-template-hash"},
				},
			}

			preStopSleep, drainBuffer := resolveShutdown(bs)
			gracePeriod := int64(preStopSleep) + int64(drainBuffer)
			templateSpec.TerminationGracePeriodSeconds = &gracePeriod

			container := corev1.Container{
				Name:            "app",
				Image:           image,
				ImagePullPolicy: corev1.PullAlways,
				Ports: []corev1.ContainerPort{
					{ContainerPort: containerPort},
				},
				Resources: resourceReqs,
				Lifecycle: &corev1.Lifecycle{
					PreStop: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{
							Command: []string{"/bin/sh", "-c", fmt.Sprintf("sleep %d", preStopSleep)},
						},
					},
				},
				Env: tracingContainerEnv(bs, dep.Spec.Template.ObjectMeta.Annotations),
			}

			if bs.Spec.ReadinessProbe != nil {
				probePort := containerPort
				if bs.Spec.ReadinessProbe.Port != nil {
					probePort = *bs.Spec.ReadinessProbe.Port
				}
				container.ReadinessProbe = &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: bs.Spec.ReadinessProbe.Path,
							Port: intstr.FromInt(int(probePort)),
						},
					},
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
					FailureThreshold:    3,
				}
			} else {
				// Default TCP readiness probe — guarantees zero-downtime rolling updates
				// even when users forget to declare an HTTP probe. Without this, K8s marks
				// pods Ready as soon as the container is Running, causing premature old-pod
				// termination while the new app is still initializing.
				container.ReadinessProbe = &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(int(containerPort))},
					},
					InitialDelaySeconds: 3,
					PeriodSeconds:       5,
					FailureThreshold:    3,
				}
			}

			if bs.Spec.LivenessProbe != nil {
				probePort := containerPort
				if bs.Spec.LivenessProbe.Port != nil {
					probePort = *bs.Spec.LivenessProbe.Port
				}
				container.LivenessProbe = &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: bs.Spec.LivenessProbe.Path,
							Port: intstr.FromInt(int(probePort)),
						},
					},
					InitialDelaySeconds: 15,
					PeriodSeconds:       10,
					FailureThreshold:    3,
				}
			}

			templateSpec.Containers = []corev1.Container{container}

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
			// In Istio ambient mode, services need this label to route through the
			// namespace's waypoint Pod for L7 features (HTTP routing, tracing, RBAC).
			// Namespace-level use-waypoint label alone is not always honored by ztunnel
			// for service-VIP traffic; service-level label is the authoritative signal.
			// Toggle symmetrically with spec.traffic so removing traffic also removes the
			// label (mergeStringMap only adds — explicit delete is required for removal).
			if bsNeedsWaypoint(bs) {
				if svc.ObjectMeta.Labels == nil {
					svc.ObjectMeta.Labels = map[string]string{}
				}
				svc.ObjectMeta.Labels[labelUseWaypoint] = waypointName
			} else {
				delete(svc.ObjectMeta.Labels, labelUseWaypoint)
			}

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

	if err := r.reconcileHPA(ctx, bs, depName, labels, minReplicas, maxReplicas); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcilePDB(ctx, bs, labels, replicas, minReplicas); err != nil {
		return ctrl.Result{}, err
	}

	cSvcName, cWeight, err := r.reconcileCanary(ctx, bs, image, port)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileHTTPRoute(ctx, bs, svcName, port, cSvcName, cWeight); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileServiceMonitor(ctx, bs, svcName, port); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileEnvoyFilterRateLimit(ctx, bs); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileDestinationRule(ctx, bs); err != nil {
		return ctrl.Result{}, err
	}

	meshNeedRollout, err := r.meshNeedsRolloutForSidecar(ctx, bs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if (labelJustEnabled && deploymentExisted) || meshNeedRollout {
		if err := r.rolloutRestartWorkload(ctx, bs); err != nil {
			return ctrl.Result{}, err
		}
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
// When injectPipeline is true, ensures GitHub Actions workflow exists in the repo.
func (r *BirServiceReconciler) reconcileBuild(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// Always inject pipeline for GitHub repos — workflow builds & pushes to registry, then notifies deploy
	if owner, repo, ok := injector.ParseGitHubRepo(bs.Spec.Repo); ok {
		if bs.Annotations == nil || bs.Annotations[annotPipelineInj] == "" {
			creds := credentials.ResolvePipelineCreds(ctx, r.Client)
			regURL := os.Getenv("REGISTRY_URL")
			if regURL == "" {
				regURL = "registry.easysolution.work"
			}
			webhookURL := os.Getenv("WEBHOOK_URL")
			if webhookURL == "" {
				webhookURL = "https://webhook.easysolution.work"
			}
			env := os.Getenv("ENVIRONMENT")
			if env == "" {
				l.Error(fmt.Errorf("ENVIRONMENT env var is not set"), "cannot inject pipeline without ENVIRONMENT")
			} else if err := injector.EnsureWorkflow(creds.GitHubToken, owner, repo, regURL, bs.Name, webhookURL, bs.Name, bs.Namespace, env); err != nil {
				l.Error(err, "pipeline injection failed", "repo", bs.Spec.Repo)
			} else if err := injector.EnsureRepoSecrets(creds.GitHubToken, owner, repo, creds.RegistryUsername, creds.RegistryPassword); err != nil {
				l.Error(err, "repo secrets failed", "repo", bs.Spec.Repo)
			} else {
				l.Info("pipeline injected", "repo", bs.Spec.Repo)
				if bs.Annotations == nil {
					bs.Annotations = make(map[string]string)
				}
				bs.Annotations[annotPipelineInj] = "true"
				_ = r.Update(ctx, bs)
			}
		}
	}

	tag := bs.Spec.Tag
	imageTag := tag
	if imageTag == "" {
		imageTag = "latest"
	}
	// Pipeline mode: spec.imageTag (rollback) > status.BuildTag > latest
	if bs.Spec.ImageTag != "" {
		imageTag = bs.Spec.ImageTag
	} else if bs.Status.BuildStatus == "Succeeded" && bs.Status.BuildTag != "" {
		imageTag = bs.Status.BuildTag
	}
	registryURL := r.effectiveRegistryURL()
	buildImage := fmt.Sprintf("%s/%s:%s", registryURL, bs.Name, imageTag)

	// After first successful build, use image from registry (pipeline builds on push, notifies via webhook)
	if bs.Status.BuildStatus == "Succeeded" {
		if bs.Spec.Port == nil || *bs.Spec.Port == 0 {
			detected := registry.InspectPort(registryURL, bs.Name, imageTag)
			if detected == 0 {
				owner, repo, ok := injector.ParseGitHubRepo(bs.Spec.Repo)
				if ok {
					detected = registry.PortFromDockerfile(owner, repo, bs.Spec.Tag, bs.Spec.Dockerfile)
					if detected > 0 {
						l.Info("auto-detected port from Dockerfile", "port", detected)
					}
				}
			} else {
				l.Info("auto-detected port from image", "port", detected)
			}
			if detected > 0 {
				cp := detected
				bs.Spec.ContainerPort = &cp
				if err := r.Update(ctx, bs); err != nil {
					l.Error(err, "failed to persist detected port to BirService")
				}
			}
		}
		l.Info("using image from registry", "image", buildImage, "tag", imageTag)
		return r.reconcileDeployment(ctx, req, bs, buildImage)
	}

	needsRebuild := r.needsRebuild(bs, imageTag)

	if needsRebuild {
		l.Info("first build, creating Kaniko job", "tag", imageTag)
		if err := r.deleteOldBuildJobs(ctx, bs); err != nil {
			return ctrl.Result{}, err
		}
	}

	jobName := fmt.Sprintf("%s-build-%s", bs.Name, sanitizeK8sName(imageTag))
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: bs.Namespace}, &job)

	if apierrors.IsNotFound(err) {
		l.Info("creating Kaniko build job", "repo", bs.Spec.Repo, "image", buildImage)

		if err := r.ensureRegistryPushSecret(ctx, bs.Namespace); err != nil {
			l.Error(err, "failed to ensure registry push secret")
			return ctrl.Result{RequeueAfter: requeueBuild}, nil
		}

		dockerfile := bs.Spec.Dockerfile
		if dockerfile == "" {
			dockerfile = "Dockerfile"
		}

		repoPath := strings.TrimPrefix(bs.Spec.Repo, "https://")
		var gitContext string
		if tag != "" {
			gitContext = fmt.Sprintf("git://%s#refs/heads/%s", repoPath, tag)
		} else {
			gitContext = fmt.Sprintf("git://%s", repoPath)
		}

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
					labelBuildTag:                  imageTag,
				},
			},
			Spec: batchv1.JobSpec{
				BackoffLimit: int32Ptr(2),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							"sidecar.istio.io/inject": "false",
						},
					},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{
							{
								Name:  "kaniko",
								Image: kanikoImage,
								Args:  args,
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      "docker-config",
										MountPath: "/kaniko/.docker",
										ReadOnly:  true,
									},
								},
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: "docker-config",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: registryPushSecretName,
										Items: []corev1.KeyToPath{
											{Key: corev1.DockerConfigJsonKey, Path: "config.json"},
										},
									},
								},
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

		return r.updateBuildStatus(ctx, req, bs, buildImage, "Building", imageTag)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if job.Status.Succeeded > 0 {
		if bs.Status.BuildStatus != "Succeeded" || bs.Status.BuildTag != imageTag {
			if _, err := r.updateBuildStatus(ctx, req, bs, buildImage, "Succeeded", imageTag); err != nil {
				return ctrl.Result{}, err
			}
		}

		if bs.Spec.Port == nil || *bs.Spec.Port == 0 {
			detected := registry.InspectPort(registryURL, bs.Name, imageTag)
			if detected == 0 {
				owner, repo, ok := injector.ParseGitHubRepo(bs.Spec.Repo)
				if ok {
					detected = registry.PortFromDockerfile(owner, repo, bs.Spec.Tag, bs.Spec.Dockerfile)
					if detected > 0 {
						l.Info("auto-detected port from Dockerfile", "port", detected)
					}
				}
			} else {
				l.Info("auto-detected port from image", "port", detected)
			}
			if detected > 0 {
				cp := detected
				bs.Spec.ContainerPort = &cp
				if err := r.Update(ctx, bs); err != nil {
					l.Error(err, "failed to persist detected port to BirService")
				}
			}
		}

		l.Info("build succeeded, deploying", "image", buildImage)
		return r.reconcileDeployment(ctx, req, bs, buildImage)
	}

	if job.Status.Failed > 0 {
		l.Error(nil, "build job failed", "job", jobName)
		return r.updateBuildStatus(ctx, req, bs, buildImage, "Failed", imageTag)
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

func (r *BirServiceReconciler) ensureRegistryPushSecret(ctx context.Context, ns string) error {
	creds := credentials.ResolvePipelineCreds(ctx, r.Client)
	if strings.TrimSpace(creds.RegistryUsername) == "" || strings.TrimSpace(creds.RegistryPassword) == "" {
		return fmt.Errorf("registry credentials are empty: set REGISTRY_USERNAME and REGISTRY_PASSWORD in github-pipeline-secret")
	}
	registryURL := r.effectiveRegistryURL()
	auth := base64.StdEncoding.EncodeToString([]byte(creds.RegistryUsername + ":" + creds.RegistryPassword))
	cfg := map[string]interface{}{
		"auths": map[string]interface{}{
			registryURL: map[string]string{
				"auth": auth,
			},
		},
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryPushSecretName, Namespace: ns},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: cfgJSON},
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		existing := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: registryPushSecretName, Namespace: ns}, existing)
		if err == nil {
			existing.Type = corev1.SecretTypeDockerConfigJson
			if string(existing.Data[corev1.DockerConfigJsonKey]) == string(cfgJSON) {
				return nil
			}
			existing.Data[corev1.DockerConfigJsonKey] = cfgJSON
			return r.Update(ctx, existing)
		}
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, secret)
		}
		return err
	})
}

func (r *BirServiceReconciler) effectiveRegistryURL() string {
	v := strings.TrimSpace(r.RegistryURL)
	if v == "" {
		return defaultRegistryURL
	}
	return strings.TrimSuffix(v, "/")
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

func (r *BirServiceReconciler) updateStableTag(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService, tag string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latest deployv1alpha1.BirService
		if err := r.Get(ctx, req.NamespacedName, &latest); err != nil {
			return err
		}
		latest.Status.StableTag = tag
		return r.Status().Update(ctx, &latest)
	})
}

func (r *BirServiceReconciler) updateBuildStatus(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService, image, status, tag string) (ctrl.Result, error) {
	buildStatusTotal.WithLabelValues(strings.ToLower(status)).Inc()
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
	registerControllerMetrics()
	return ctrl.NewControllerManagedBy(mgr).
		For(&deployv1alpha1.BirService{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv1.HorizontalPodAutoscaler{}).
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

// tracingAnnotations returns the pod template annotations with the OpenTelemetry
// Operator inject-* annotation set. Tracing is always on platform-wide — no per-workload
// config needed. The annotation value points at the cluster-wide Instrumentation CR
// "observability/default" so workloads in any namespace get the same exporter and
// sampler config. Existing annotations are preserved; stale inject-* annotations are
// cleared so a runtime change does not leave both.
func (r *BirServiceReconciler) tracingAnnotations(ctx context.Context, bs *deployv1alpha1.BirService, existing map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range existing {
		if strings.HasPrefix(k, "instrumentation.opentelemetry.io/inject-") {
			continue
		}
		out[k] = v
	}
	runtime := r.resolveTracingRuntime(ctx, bs)
	if runtime == "" {
		return out
	}
	out[fmt.Sprintf("instrumentation.opentelemetry.io/inject-%s", runtime)] = "observability/default"
	out["instrumentation.opentelemetry.io/container-names"] = "app"
	return out
}

// tracingContainerEnv returns runtime-specific env vars that the auto-instrumentation
// SDK reads to capture custom ActivitySources/sources. The OTel Operator injects the
// SDK + endpoint/propagator/sampler env, but custom source registration is per-runtime:
// .NET requires OTEL_DOTNET_AUTO_TRACES_ADDITIONAL_SOURCES (no wildcard support in v1.x).
// Convention: ActivitySource name == BirService name, so we inject bs.Name.
// Returns nil when no inject-* annotation is present (auto-instrumentation skipped).
func tracingContainerEnv(bs *deployv1alpha1.BirService, annotations map[string]string) []corev1.EnvVar {
	var env []corev1.EnvVar
	if _, ok := annotations["instrumentation.opentelemetry.io/inject-dotnet"]; ok {
		env = append(env, corev1.EnvVar{
			Name:  "OTEL_DOTNET_AUTO_TRACES_ADDITIONAL_SOURCES",
			Value: bs.Name,
		})
	}
	return env
}

// resolveTracingRuntime detects the OTel inject-* runtime from the image config
// (registry inspect) or Dockerfile FROM line. Returns "" when runtime cannot be
// determined and OTel injection is skipped.
func (r *BirServiceReconciler) resolveTracingRuntime(ctx context.Context, bs *deployv1alpha1.BirService) string {
	l := log.FromContext(ctx)
	registryURL := r.effectiveRegistryURL()
	imageTag := bs.Status.BuildTag
	if imageTag == "" {
		imageTag = bs.Spec.Tag
	}
	if imageTag == "" {
		imageTag = "latest"
	}
	if rt := registry.InspectRuntime(registryURL, bs.Name, imageTag); rt != "" {
		return rt
	}
	if owner, repo, ok := injector.ParseGitHubRepo(bs.Spec.Repo); ok {
		if rt := registry.RuntimeFromDockerfile(owner, repo, bs.Spec.Tag, bs.Spec.Dockerfile); rt != "" {
			return rt
		}
	}
	l.Info("tracing runtime could not be auto-detected; skipping inject annotation", "birservice", bs.Name)
	return ""
}

// resolveShutdown returns (preStopSleep, drainBuffer) seconds. Defaults: sleep=15, buffer=5.
// terminationGracePeriodSeconds is preStopSleep + drainBuffer.
func resolveShutdown(bs *deployv1alpha1.BirService) (int32, int32) {
	sleep := int32(15)
	buffer := int32(5)
	if bs.Spec.Shutdown != nil {
		if bs.Spec.Shutdown.PreStopSleepSeconds != nil && *bs.Spec.Shutdown.PreStopSleepSeconds >= 0 {
			sleep = *bs.Spec.Shutdown.PreStopSleepSeconds
		}
		if bs.Spec.Shutdown.DrainBufferSeconds != nil && *bs.Spec.Shutdown.DrainBufferSeconds >= 0 {
			buffer = *bs.Spec.Shutdown.DrainBufferSeconds
		}
	}
	return sleep, buffer
}

// resolveDeploymentStrategy maps the domain-level singleton flag to an apps/v1
// DeploymentStrategy. Singleton apps cannot run two versions concurrently → Recreate
// (brief downtime). Otherwise RollingUpdate with platform-managed budgets: zero
// degradation (maxUnavailable=0) plus 25% burst headroom — industry-standard SRE defaults.
func resolveDeploymentStrategy(bs *deployv1alpha1.BirService) appsv1.DeploymentStrategy {
	if isSingleton(bs) {
		return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}
	maxUnavailable := intstr.FromInt(0)
	maxSurge := intstr.FromString("25%")
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
}

func isSingleton(bs *deployv1alpha1.BirService) bool {
	return bs.Spec.Singleton != nil && *bs.Spec.Singleton
}

// resolveMaxDown returns the PDB maxUnavailable count. Honors spec.maxDown when set
// (including 0 for "block all voluntary disruption"). Default: floor(N/2), but at
// least 1 so the PDB never blocks every drain.
func resolveMaxDown(bs *deployv1alpha1.BirService, effectiveReplicas int32) int32 {
	if bs.Spec.MaxDown != nil {
		v := *bs.Spec.MaxDown
		if v < 0 {
			v = 0
		}
		return v
	}
	half := effectiveReplicas / 2
	if half < 1 {
		half = 1
	}
	return half
}

// reconcilePDB ensures a PodDisruptionBudget exists for the workload when the
// effective minimum replica count is >= 2 (replicas>=2 OR HPA minReplicas>=2).
// Otherwise any existing PDB is deleted. PDB is also skipped for Recreate strategy
// since voluntary-disruption protection is meaningless when the rollout is all-or-nothing.
// Uses unstructured policy/v1 to avoid pulling a newer k8s.io/api dependency.
func (r *BirServiceReconciler) reconcilePDB(ctx context.Context, bs *deployv1alpha1.BirService, podLabels map[string]string, replicas *int32, hpaMin *int32) error {
	pdbName := fmt.Sprintf("%s-pdb", bs.Name)
	gvk := schema.GroupVersionKind{Group: "policy", Version: "v1", Kind: "PodDisruptionBudget"}
	key := types.NamespacedName{Name: pdbName, Namespace: bs.Namespace}

	deletePDB := func() error {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(gvk)
		err := r.Get(ctx, key, existing)
		if err == nil {
			return client.IgnoreNotFound(r.Delete(ctx, existing))
		}
		return client.IgnoreNotFound(err)
	}

	// Singleton apps run Recreate — voluntary-disruption protection is meaningless when
	// the rollout is all-or-nothing and only one replica is ever running.
	if isSingleton(bs) {
		return deletePDB()
	}

	// Effective min replicas
	effectiveMin := int32(0)
	if replicas != nil {
		effectiveMin = *replicas
	} else if hpaMin != nil {
		effectiveMin = *hpaMin
	}
	if effectiveMin < 2 {
		return deletePDB()
	}

	maxDown := resolveMaxDown(bs, effectiveMin)
	if maxDown >= effectiveMin {
		// PDB would never block anything — skip rather than create a noop.
		return deletePDB()
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pdb := &unstructured.Unstructured{}
		pdb.SetGroupVersionKind(gvk)
		pdb.SetName(pdbName)
		pdb.SetNamespace(bs.Namespace)

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
			matchLabels := map[string]interface{}{}
			for k, v := range podLabels {
				matchLabels[k] = v
			}
			pdb.Object["spec"] = map[string]interface{}{
				"maxUnavailable": int64(maxDown),
				"selector": map[string]interface{}{
					"matchLabels": matchLabels,
				},
			}
			pdb.SetLabels(mergeStringMap(pdb.GetLabels(), podLabels))
			return ctrl.SetControllerReference(bs, pdb, r.Scheme)
		})
		return err
	})
}

var httpRouteGVK = schema.GroupVersionKind{
	Group:   "gateway.networking.k8s.io",
	Version: "v1",
	Kind:    "HTTPRoute",
}

var serviceMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "ServiceMonitor",
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
		env := strings.TrimSpace(r.Environment)
		if env == "" {
			env = bs.Namespace
		}
		return fmt.Sprintf("%s-%s.%s", bs.Name, env, r.BaseDomain)
	}
	return ""
}

func exposeOrDefault(bs *deployv1alpha1.BirService) bool {
	if bs.Spec.Expose == nil {
		return true
	}
	return *bs.Spec.Expose
}

func resolveHPAConfig(bs *deployv1alpha1.BirService) (*int32, *int32, bool) {
	// Strict format: spec.hpa.{minReplicas,maxReplicas}
	if bs.Spec.HPA != nil && bs.Spec.HPA.MinReplicas != nil && bs.Spec.HPA.MaxReplicas != nil {
		return bs.Spec.HPA.MinReplicas, bs.Spec.HPA.MaxReplicas, true
	}
	return nil, nil, false
}

func resolveContainerResources(bs *deployv1alpha1.BirService) (corev1.ResourceRequirements, error) {
	// Defaults requested by product: requests cpu=75m, memory=200Mi.
	reqCPU := resource.MustParse("75m")
	reqMem := resource.MustParse("200Mi")

	if bs.Spec.Resources != nil && bs.Spec.Resources.Requests != nil {
		if cpu := strings.TrimSpace(bs.Spec.Resources.Requests.CPU); cpu != "" {
			parsed, err := resource.ParseQuantity(cpu)
			if err != nil {
				return corev1.ResourceRequirements{}, fmt.Errorf("invalid spec.resources.requests.cpu: %w", err)
			}
			reqCPU = parsed
		}
		if mem := strings.TrimSpace(bs.Spec.Resources.Requests.Memory); mem != "" {
			parsed, err := resource.ParseQuantity(mem)
			if err != nil {
				return corev1.ResourceRequirements{}, fmt.Errorf("invalid spec.resources.requests.memory: %w", err)
			}
			reqMem = parsed
		}
	}

	// Limits default to 2x of final requests, unless explicitly provided.
	limCPU := *resource.NewMilliQuantity(reqCPU.MilliValue()*2, resource.DecimalSI)
	limMem := *resource.NewQuantity(reqMem.Value()*2, resource.BinarySI)

	if bs.Spec.Resources != nil && bs.Spec.Resources.Limits != nil {
		if cpu := strings.TrimSpace(bs.Spec.Resources.Limits.CPU); cpu != "" {
			parsed, err := resource.ParseQuantity(cpu)
			if err != nil {
				return corev1.ResourceRequirements{}, fmt.Errorf("invalid spec.resources.limits.cpu: %w", err)
			}
			limCPU = parsed
		}
		if mem := strings.TrimSpace(bs.Spec.Resources.Limits.Memory); mem != "" {
			parsed, err := resource.ParseQuantity(mem)
			if err != nil {
				return corev1.ResourceRequirements{}, fmt.Errorf("invalid spec.resources.limits.memory: %w", err)
			}
			limMem = parsed
		}
	}

	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    reqCPU,
			corev1.ResourceMemory: reqMem,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    limCPU,
			corev1.ResourceMemory: limMem,
		},
	}, nil
}

func (r *BirServiceReconciler) reconcileHPA(ctx context.Context, bs *deployv1alpha1.BirService, depName string, labels map[string]string, minReplicas *int32, maxReplicas *int32) error {
	// When spec.replicas is set, there is no HPA — replicas wins.
	if bs.Spec.Replicas != nil {
		hpa := &autoscalingv1.HorizontalPodAutoscaler{}
		hpaName := fmt.Sprintf("%s-hpa", bs.Name)
		err := r.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: bs.Namespace}, hpa)
		if err == nil {
			return client.IgnoreNotFound(r.Delete(ctx, hpa))
		}
		return client.IgnoreNotFound(err)
	}

	if minReplicas == nil || maxReplicas == nil {
		// HPA was disabled — delete any existing HPA.
		hpa := &autoscalingv1.HorizontalPodAutoscaler{}
		hpaName := fmt.Sprintf("%s-hpa", bs.Name)
		err := r.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: bs.Namespace}, hpa)
		if err == nil {
			return client.IgnoreNotFound(r.Delete(ctx, hpa))
		}
		return client.IgnoreNotFound(err)
	}

	hpa := &autoscalingv1.HorizontalPodAutoscaler{}
	hpa.Name = fmt.Sprintf("%s-hpa", bs.Name)
	hpa.Namespace = bs.Namespace

	targetCPU := int32(80)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
		hpa.ObjectMeta.Labels = mergeStringMap(hpa.ObjectMeta.Labels, labels)
		hpa.Spec = autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       depName,
			},
			MinReplicas:                    minReplicas,
			MaxReplicas:                    *maxReplicas,
			TargetCPUUtilizationPercentage: &targetCPU,
		}
		return ctrl.SetControllerReference(bs, hpa, r.Scheme)
	})
	return err
}

func (r *BirServiceReconciler) reconcileHTTPRoute(ctx context.Context, bs *deployv1alpha1.BirService, svcName string, svcPort int32, canaySvcName string, canaryWeight int32) error {
	routeName := fmt.Sprintf("%s-route", bs.Name)
	hostname := r.resolveHostname(bs)

	if !exposeOrDefault(bs) || hostname == "" {
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
			var backendRefs []interface{}
			if canaySvcName != "" && canaryWeight > 0 {
				stableWeight := int64(100 - canaryWeight)
				if stableWeight < 0 {
					stableWeight = 0
				}
				backendRefs = []interface{}{
					map[string]interface{}{
						"name":   svcName,
						"port":   int64(svcPort),
						"weight": stableWeight,
					},
					map[string]interface{}{
						"name":   canaySvcName,
						"port":   int64(svcPort),
						"weight": int64(canaryWeight),
					},
				}
			} else {
				backendRefs = []interface{}{
					map[string]interface{}{
						"name": svcName,
						"port": int64(svcPort),
					},
				}
			}

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
						"backendRefs": backendRefs,
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

func (r *BirServiceReconciler) metricsEnabled(bs *deployv1alpha1.BirService) (bool, string) {
	// Default true — metrics scraping is on; an explicit false disables it.
	if bs.Spec.Metrics != nil && !bs.Spec.Metrics.Enabled {
		return false, ""
	}
	path := "/metrics"
	if bs.Spec.Metrics != nil && bs.Spec.Metrics.Path != "" {
		path = bs.Spec.Metrics.Path
	}
	return true, path
}

func (r *BirServiceReconciler) reconcileServiceMonitor(ctx context.Context, bs *deployv1alpha1.BirService, svcName string, port int32) error {
	monitorName := fmt.Sprintf("%s-monitor", bs.Name)
	enabled, path := r.metricsEnabled(bs)

	if !enabled {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(serviceMonitorGVK)
		err := r.Get(ctx, types.NamespacedName{Name: monitorName, Namespace: bs.Namespace}, existing)
		if err == nil {
			return r.Delete(ctx, existing)
		}
		return client.IgnoreNotFound(err)
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		sm := &unstructured.Unstructured{}
		sm.SetGroupVersionKind(serviceMonitorGVK)
		sm.SetName(monitorName)
		sm.SetNamespace(bs.Namespace)

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
			sm.Object["spec"] = map[string]interface{}{
				"selector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"app.kubernetes.io/name": bs.Name,
					},
				},
				"endpoints": []interface{}{
					map[string]interface{}{
						"port":     "http",
						"path":     path,
						"interval": "30s",
					},
				},
			}
			sm.SetLabels(mergeStringMap(sm.GetLabels(), map[string]string{
				"app.kubernetes.io/name":       bs.Name,
				"app.kubernetes.io/managed-by": "easy-deploy-operator",
			}))
			return ctrl.SetControllerReference(bs, sm, r.Scheme)
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

func classifyReconcileResult(res ctrl.Result, err error) string {
	if err != nil {
		return "error"
	}
	if res.Requeue || res.RequeueAfter > 0 {
		return "requeue"
	}
	return "success"
}
