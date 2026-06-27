package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
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
	// labelApp groups every instance of one app (same source repo) under a shared
	// value so the build-complete webhook can fan a new image out to all instances.
	labelApp = "deploy.easydeploy.io/app"
	labelPurpose           = "deploy.easydeploy.io/purpose"
	annotRebuild           = "deploy.easydeploy.io/rebuild"
	annotPipelineInj       = "deploy.easydeploy.io/pipeline-injected"
	// pipelineWorkflowVersion is bumped whenever the injected workflow template or
	// the values we feed it change in a way that needs to reach already-onboarded
	// repos. The annotation stores the version last injected; a mismatch forces a
	// re-injection so stale workflows (e.g. ones built with the instance name
	// instead of the app/repo name) get overwritten with the correct content.
	pipelineWorkflowVersion = "2"
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

	// Being deleted: stop reconciling. The operator owns no custom finalizer, so
	// owned children (Deployment, Service, HPA, HTTPRoute, …) are removed by the
	// garbage collector via ownerReferences. Without this guard, reconcile keeps
	// re-creating those children while foreground deletion tries to remove them —
	// the foregroundDeletion finalizer is never satisfied, so the CR is stuck
	// Terminating forever and its pods churn. Bail out and let GC finish.
	if !bs.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
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
		image = fmt.Sprintf("%s/%s:%s", r.effectiveRegistryURL(), appName(bs), bs.Status.StableTag)
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

	// Resolve the node pool before touching the Deployment. An unknown pool blocks
	// the deploy (surfaced in status) rather than silently scheduling on default nodes.
	nodeSelector, tolerations, err := r.resolveNodePool(ctx, bs)
	if err != nil {
		log.FromContext(ctx).Error(err, "nodePool resolution failed", "nodePool", bs.Spec.NodePool)
		_, _ = r.updateBuildStatus(ctx, req, bs, bs.Status.BuildImage, "NodePoolError: "+err.Error(), bs.Status.BuildTag)
		return ctrl.Result{RequeueAfter: requeueBuild}, nil
	}

	// A malformed rps window would produce a bogus recording rule / a metric
	// selector that never matches. Block the deploy with a clear status instead.
	if bs.Spec.HPA != nil && strings.TrimSpace(bs.Spec.HPA.Window) != "" && !validPromDuration(bs.Spec.HPA.Window) {
		msg := fmt.Sprintf("HPAWindowError: invalid hpa.window %q (use a Prometheus duration like 1m, 2m, 5m)", bs.Spec.HPA.Window)
		log.FromContext(ctx).Info(msg, "name", bs.Name)
		_, _ = r.updateBuildStatus(ctx, req, bs, bs.Status.BuildImage, msg, bs.Status.BuildTag)
		return ctrl.Result{RequeueAfter: requeueBuild}, nil
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
				// App-grouping label for build-complete webhook fan-out (metadata only,
				// kept off the immutable selector).
				dep.ObjectMeta.Labels[labelApp] = appName(bs)

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
			// route-group: pods join their pool here (Deployment selector stays
			// app.kubernetes.io/name — immutable — so this is pod-template only).
			// The pool's Service selects this label, spanning every member's pods.
			templateLabels[labelRouteGroup] = routeGroup(bs)
			// Istio canonical service: app.kubernetes.io/name (the immutable selector)
			// is the per-instance name, so without this every pool instance reports as
			// a separate canonical service and pool traffic is mis-attributed (shows as
			// unknown in mesh dashboards). Pin the canonical name to the app so main +
			// testing report under one app; destination_workload still distinguishes
			// each instance. Standalone apps already equal their app name (no-op).
			templateLabels[labelCanonicalName] = appName(bs)
			templateLabels[labelCanonicalRevision] = "latest"
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
			// LabelSelector covers all pods of this service; MatchLabelKeys adds
			// pod-template-hash (K8s 1.25+) so the spread is scoped per ReplicaSet version.
			// Without it, a rolling update counts old and new pods together and the new
			// ReplicaSet piles onto nodes that look empty of old pods; with it, each
			// version spreads independently so the incoming pods land evenly.
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

			// Node pool: pin to the pool's nodes and tolerate its taints. Assigned
			// (not appended) so clearing spec.nodePool reverts to default scheduling.
			templateSpec.NodeSelector = nodeSelector
			templateSpec.Tolerations = tolerations

			return ctrl.SetControllerReference(bs, &dep, r.Scheme)
		})
		return err
	}); err != nil {
		return ctrl.Result{}, err
	}

	// The pool's Service is owned by the primary member only; non-primary members
	// just contribute pods (via the route-group label) and create no Service.
	svcName := poolServiceName(bs)
	if routeIsPrimary(bs) {
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

				// Select on the route-group label so the pool spans every member's
				// pods (main + testing + …), letting the LB pick any of them.
				svc.Spec.Selector = map[string]string{labelRouteGroup: routeGroup(bs)}
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
		// Only the primary instance injects the workflow: every instance of an app
		// shares one repo, so they would otherwise clobber each other's workflow.
		// The workflow builds one image (the repo name) and the build-complete
		// webhook fans it out to all instances. Single-instance apps are always
		// their own primary, so this also covers the non-pool case.
		if routeIsPrimary(bs) && bs.Annotations[annotPipelineInj] != pipelineWorkflowVersion {
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
			} else if err := injector.EnsureWorkflow(creds.GitHubToken, owner, repo, regURL, repo, webhookURL, repo, bs.Namespace, env); err != nil {
				l.Error(err, "pipeline injection failed", "repo", bs.Spec.Repo)
			} else if err := injector.EnsureRepoSecrets(creds.GitHubToken, owner, repo, creds.RegistryUsername, creds.RegistryPassword); err != nil {
				l.Error(err, "repo secrets failed", "repo", bs.Spec.Repo)
			} else {
				l.Info("pipeline injected", "repo", bs.Spec.Repo)
				if bs.Annotations == nil {
					bs.Annotations = make(map[string]string)
				}
				bs.Annotations[annotPipelineInj] = pipelineWorkflowVersion
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
	buildImage := fmt.Sprintf("%s/%s:%s", registryURL, appName(bs), imageTag)

	// After first successful build, use image from registry (pipeline builds on push, notifies via webhook)
	if bs.Status.BuildStatus == "Succeeded" {
		if bs.Spec.Port == nil || *bs.Spec.Port == 0 {
			detected := registry.InspectPort(registryURL, appName(bs), imageTag)
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

	jobName := fmt.Sprintf("%s-build-%s", appName(bs), sanitizeK8sName(imageTag))
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
			detected := registry.InspectPort(registryURL, appName(bs), imageTag)
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

// resolveNodePool turns bs.Spec.NodePool into the nodeSelector + tolerations to
// inject onto the pod template. An empty nodePool returns nils (no constraints —
// the pod schedules onto the default, untainted nodes). A named-but-missing pool
// returns an error so the deploy is blocked instead of silently landing on the
// default nodes.
func (r *BirServiceReconciler) resolveNodePool(ctx context.Context, bs *deployv1alpha1.BirService) (map[string]string, []corev1.Toleration, error) {
	if bs.Spec.NodePool == "" {
		return nil, nil, nil
	}
	var pool deployv1alpha1.Pool
	if err := r.Get(ctx, types.NamespacedName{Name: bs.Spec.NodePool}, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("nodePool %q not found (create a Pool resource or fix the name)", bs.Spec.NodePool)
		}
		return nil, nil, err
	}

	var nodeSelector map[string]string
	if len(pool.Spec.NodeSelector) > 0 {
		nodeSelector = make(map[string]string, len(pool.Spec.NodeSelector))
		for k, v := range pool.Spec.NodeSelector {
			nodeSelector[k] = v
		}
	}

	var tolerations []corev1.Toleration
	for _, t := range pool.Spec.Taints {
		tol := corev1.Toleration{Key: t.Key, Value: t.Value, Effect: t.Effect, Operator: corev1.TolerationOpEqual}
		if t.Value == "" {
			tol.Operator = corev1.TolerationOpExists
		}
		tolerations = append(tolerations, tol)
	}
	return nodeSelector, tolerations, nil
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
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
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

// routeHostname derives a distinct hostname for a secondary route entry:
// <bs.Name>-<routeName>-<env>.<baseDomain>. Used when the route catalog leaves the
// hostname unset, so multiple routes on one pool don't share a hostname.
func (r *BirServiceReconciler) routeHostname(bs *deployv1alpha1.BirService, routeName string) string {
	if r.BaseDomain == "" {
		return ""
	}
	env := strings.TrimSpace(r.Environment)
	if env == "" {
		env = bs.Namespace
	}
	return fmt.Sprintf("%s-%s-%s.%s", bs.Name, routeName, env, r.BaseDomain)
}

// resolveHostnames returns every hostname the HTTPRoute should serve: the primary
// (spec.hostname or the auto-generated name) followed by any spec.hostnames aliases,
// de-duplicated with empties skipped. external-dns creates a record for each.
func (r *BirServiceReconciler) resolveHostnames(bs *deployv1alpha1.BirService) []string {
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	add(r.resolveHostname(bs))
	for _, h := range bs.Spec.Hostnames {
		add(h)
	}
	return out
}

func exposeOrDefault(bs *deployv1alpha1.BirService) bool {
	if bs.Spec.Expose == nil {
		return true
	}
	return *bs.Spec.Expose
}

// routeGroup returns the pool identifier for this instance: the chart-resolved
// route.group, or the BirService name when standalone (its own single-pod pool).
func routeGroup(bs *deployv1alpha1.BirService) string {
	if bs.Spec.Route != nil && bs.Spec.Route.Group != "" {
		return bs.Spec.Route.Group
	}
	return bs.Name
}

// routeIsPrimary reports whether this instance owns its pool's Service and
// HTTPRoutes. Standalone instances (no route) are always their own primary;
// non-primary pool members only contribute pods.
func routeIsPrimary(bs *deployv1alpha1.BirService) bool {
	if bs.Spec.Route != nil {
		return bs.Spec.Route.Primary
	}
	return true
}

// poolServiceName is the Service that fronts a pool: <group>-svc.
func poolServiceName(bs *deployv1alpha1.BirService) string {
	return fmt.Sprintf("%s-svc", routeGroup(bs))
}

// appName is the app/repo identity shared by every instance of a tenant. All
// instances of one app build and pull ONE image (registry/<appName>:<tag>); the
// build-complete webhook fans a new tag out to each instance's deployment. For a
// repo-built app it is the GitHub repo name (so main + testing share it); with no
// repo it falls back to the BirService name (single-instance, image-based apps).
func appName(bs *deployv1alpha1.BirService) string {
	if _, repo, ok := injector.ParseGitHubRepo(bs.Spec.Repo); ok {
		return repo
	}
	return bs.Name
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
	hpaName := fmt.Sprintf("%s-hpa", bs.Name)

	// When spec.replicas is set replicas wins, or HPA is disabled when min/max are
	// missing — either way ensure no stale HPA remains.
	if bs.Spec.Replicas != nil || minReplicas == nil || maxReplicas == nil {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		err := r.Get(ctx, types.NamespacedName{Name: hpaName, Namespace: bs.Namespace}, hpa)
		if err == nil {
			return client.IgnoreNotFound(r.Delete(ctx, hpa))
		}
		return client.IgnoreNotFound(err)
	}

	warnMissingHPAPrereqs(ctx, bs)

	// Keep the shared rps-window recording rules in sync with what services request
	// (adds new windows, prunes unused ones). Best-effort — a failure here must not
	// block HPA creation; Prometheus loads the rule asynchronously anyway.
	if err := r.reconcileRPSWindows(ctx); err != nil {
		log.FromContext(ctx).Error(err, "failed to reconcile rps window recording rules")
	}

	metrics := hpaMetrics(bs, depName)

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	hpa.Name = hpaName
	hpa.Namespace = bs.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
		hpa.ObjectMeta.Labels = mergeStringMap(hpa.ObjectMeta.Labels, labels)
		hpa.Spec = autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       depName,
			},
			MinReplicas: minReplicas,
			MaxReplicas: *maxReplicas,
			Metrics:     metrics,
		}
		return ctrl.SetControllerReference(bs, hpa, r.Scheme)
	})
	return err
}

// workerMetricName is the Prometheus series the "worker" HPA signal scales on: a
// language-agnostic per-pod worker-saturation percentage (0-100), produced by the
// app-worker-scaling recording rule (100 * busy / max workers, normalized across
// runtimes) and surfaced as a Pods custom metric by prometheus-adapter. Because it
// is already a percentage, the HPA target is a utilization % just like cpu/memory.
const workerMetricName = "app_worker_utilization"

// warnMissingHPAPrereqs logs when a configured scaling signal can't actually
// fire because its data source is absent: rps needs a waypoint (spec.traffic) for
// L7 request metrics, and worker needs a ServiceMonitor (spec.metrics) to scrape
// the php-fpm exporter. The HPA is still created — these signals just stay idle.
func warnMissingHPAPrereqs(ctx context.Context, bs *deployv1alpha1.BirService) {
	if bs.Spec.HPA == nil {
		return
	}
	logger := log.FromContext(ctx)

	scaleType := strings.ToLower(strings.TrimSpace(bs.Spec.HPA.ScaleType))
	wantsRPS := scaleType == "rps" || (scaleType == "" && bs.Spec.HPA.TargetRPS != nil && *bs.Spec.HPA.TargetRPS > 0)
	wantsWorker := scaleType == "worker" || scaleType == "workers"

	if wantsRPS && bs.Spec.Traffic == nil {
		logger.Info("hpa rps signal is set but spec.traffic is nil; no waypoint means "+
			"no L7 request metrics — RPS-based scaling will not fire", "name", bs.Name)
	}
	if wantsWorker && (bs.Spec.Metrics == nil || !bs.Spec.Metrics.Enabled) {
		logger.Info("hpa worker signal is set but spec.metrics is disabled; without a "+
			"ServiceMonitor the php-fpm metric is never scraped — worker scaling will not fire", "name", bs.Name)
	}
}

// defaultUtilization is the CPU/memory utilization target used when the developer
// names a cpu/memory signal without an explicit target (and the platform fallback).
const defaultUtilization int32 = 80

// hpaMetrics builds the autoscaling/v2 metric for a BirService HPA.
//
// The developer names a single signal in spec.hpa.scaleType with a per-pod
// spec.hpa.target; this resolves it into the matching autoscaling/v2 source:
//
//	cpu    → Resource (CPU)    utilization %   — metrics-server
//	memory → Resource (memory) utilization %   — metrics-server
//	rps    → External istio_requests_per_second (AverageValue, req/s) — prometheus-adapter
//	worker → Pods app_worker_utilization        (AverageValue, %)     — prometheus-adapter
//
// cpu/memory/worker are utilization percentages and default to 80% when target is
// omitted; rps is an absolute req/s and has no default. The legacy
// spec.hpa.targetRPS is honored as an rps signal when scaleType is empty. When
// nothing resolves, the HPA falls back to CPU utilization at 80% (the default).
func hpaMetrics(bs *deployv1alpha1.BirService, depName string) []autoscalingv2.MetricSpec {
	if bs.Spec.HPA != nil {
		target := bs.Spec.HPA.Target
		switch strings.ToLower(strings.TrimSpace(bs.Spec.HPA.ScaleType)) {
		case "cpu":
			return []autoscalingv2.MetricSpec{resourceUtilizationMetric(corev1.ResourceCPU, orDefault(target, defaultUtilization))}
		case "memory", "mem":
			return []autoscalingv2.MetricSpec{resourceUtilizationMetric(corev1.ResourceMemory, orDefault(target, defaultUtilization))}
		case "rps":
			if target > 0 {
				return []autoscalingv2.MetricSpec{rpsExternalMetric(depName, target, hpaWindowOrDefault(bs))}
			}
		case "worker", "workers":
			// app_worker_utilization is a percentage; reuse the cpu/memory default.
			return []autoscalingv2.MetricSpec{podsAverageMetric(workerMetricName, orDefault(target, defaultUtilization))}
		}

		// Backward compatibility: the legacy targetRPS knob is an rps signal,
		// used only when scaleType didn't already select one.
		if bs.Spec.HPA.TargetRPS != nil && *bs.Spec.HPA.TargetRPS > 0 {
			return []autoscalingv2.MetricSpec{rpsExternalMetric(depName, *bs.Spec.HPA.TargetRPS, hpaWindowOrDefault(bs))}
		}
	}

	return []autoscalingv2.MetricSpec{resourceUtilizationMetric(corev1.ResourceCPU, defaultUtilization)}
}

// orDefault returns v when it is a positive value, else def.
func orDefault(v, def int32) int32 {
	if v > 0 {
		return v
	}
	return def
}

// resourceUtilizationMetric builds a Resource metric (CPU or memory) targeting an
// average utilization percentage of the container's request, served by metrics-server.
func resourceUtilizationMetric(name corev1.ResourceName, targetPercent int32) autoscalingv2.MetricSpec {
	pct := targetPercent
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: name,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: &pct,
			},
		},
	}
}

// rpsExternalMetric builds the External istio_requests_per_second metric for a
// per-pod requests/sec target. The metric is served by prometheus-adapter from
// the mesh's istio_requests_total counter; destination_workload picks this
// workload's series while the namespace label is injected by the adapter from the
// HPA's namespace (resource override), so two same-named workloads in different
// namespaces don't cross-count. AverageValue divides total mesh RPS by the target.
func rpsExternalMetric(depName string, targetRPS int32, window string) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ExternalMetricSourceType,
		External: &autoscalingv2.ExternalMetricSource{
			Metric: autoscalingv2.MetricIdentifier{
				Name: "istio_requests_per_second",
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"destination_workload": depName,
						// window selects the matching pre-computed rate series; the
						// operator ensures a recording rule exists for this window.
						"window": window,
					},
				},
			},
			Target: autoscalingv2.MetricTarget{
				Type:         autoscalingv2.AverageValueMetricType,
				AverageValue: resource.NewQuantity(int64(targetRPS), resource.DecimalSI),
			},
		},
	}
}

const (
	// defaultRPSWindow is the rate-averaging window used when spec.hpa.window is
	// empty. The operator always keeps a recording rule for this window so the
	// default rps metric always has a backing series.
	defaultRPSWindow = "1m"
	// rpsWindowsRuleName is the single, operator-managed PrometheusRule that holds
	// one recording rule per distinct window in use across all BirServices.
	rpsWindowsRuleName = "easy-deploy-rps-windows"
	// rpsWindowedMetric is the recording-rule output the rps external metric reads.
	rpsWindowedMetric = "istio_requests:rate_windowed"
)

var prometheusRuleGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "PrometheusRule",
}

// promDurationRe matches a Prometheus range duration (e.g. 30s, 1m, 2m30s, 1h).
var promDurationRe = regexp.MustCompile(`^([0-9]+(ms|s|m|h|d|w|y))+$`)

func validPromDuration(s string) bool {
	return promDurationRe.MatchString(strings.TrimSpace(s))
}

// hpaWindowOrDefault returns the rps rate window for a BirService, defaulting to 1m.
func hpaWindowOrDefault(bs *deployv1alpha1.BirService) string {
	if bs.Spec.HPA != nil {
		if w := strings.TrimSpace(bs.Spec.HPA.Window); w != "" {
			return w
		}
	}
	return defaultRPSWindow
}

// hpaUsesRPS reports whether the BirService scales on the rps signal (explicit
// scaleType: rps, or the legacy targetRPS knob with no other scaleType).
func hpaUsesRPS(bs *deployv1alpha1.BirService) bool {
	if bs.Spec.HPA == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(bs.Spec.HPA.ScaleType)) {
	case "rps":
		return true
	case "":
		return bs.Spec.HPA.TargetRPS != nil && *bs.Spec.HPA.TargetRPS > 0
	}
	return false
}

func (r *BirServiceReconciler) operatorNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	return "easy-deploy-system"
}

// reconcileRPSWindows keeps a single PrometheusRule in sync with the set of rate
// windows requested across all BirServices. Each distinct window becomes one
// recording rule that pre-computes istio rps and tags it with a `window` label,
// so a per-service HPA can select its window via the metric selector. Services
// sharing a window share one rule (reuse); a new window adds a rule (create);
// CreateOrUpdate is a no-op when the window set is unchanged.
func (r *BirServiceReconciler) reconcileRPSWindows(ctx context.Context) error {
	var list deployv1alpha1.BirServiceList
	if err := r.List(ctx, &list); err != nil {
		return err
	}

	windowSet := map[string]bool{defaultRPSWindow: true}
	for i := range list.Items {
		bs := &list.Items[i]
		if !bs.DeletionTimestamp.IsZero() || !hpaUsesRPS(bs) {
			continue
		}
		if w := hpaWindowOrDefault(bs); validPromDuration(w) {
			windowSet[w] = true
		}
	}

	windows := make([]string, 0, len(windowSet))
	for w := range windowSet {
		windows = append(windows, w)
	}
	sort.Strings(windows)

	rules := make([]interface{}, 0, len(windows))
	for _, w := range windows {
		rules = append(rules, map[string]interface{}{
			"record": rpsWindowedMetric,
			"expr": fmt.Sprintf(
				`sum(rate(istio_requests_total{destination_workload!="",destination_workload_namespace!=""}[%s])) by (destination_workload, destination_workload_namespace)`, w),
			"labels": map[string]interface{}{"window": w},
		})
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pr := &unstructured.Unstructured{}
		pr.SetGroupVersionKind(prometheusRuleGVK)
		pr.SetName(rpsWindowsRuleName)
		pr.SetNamespace(r.operatorNamespace())

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pr, func() error {
			pr.Object["spec"] = map[string]interface{}{
				"groups": []interface{}{
					map[string]interface{}{
						"name":  "easy-deploy-rps-windows",
						"rules": rules,
					},
				},
			}
			pr.SetLabels(mergeStringMap(pr.GetLabels(), map[string]string{
				"app.kubernetes.io/managed-by": "easy-deploy-operator",
			}))
			return nil
		})
		return err
	})
}

// podsAverageMetric builds a Pods custom metric targeting an average per-pod value
// (e.g. worker-utilization %). prometheus-adapter serves the named series under
// custom.metrics.k8s.io, associated to the workload's pods; the HPA averages it
// across the pool and divides by the target to pick a replica count.
func podsAverageMetric(metricName string, target int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.PodsMetricSourceType,
		Pods: &autoscalingv2.PodsMetricSource{
			Metric: autoscalingv2.MetricIdentifier{Name: metricName},
			Target: autoscalingv2.MetricTarget{
				Type:         autoscalingv2.AverageValueMetricType,
				AverageValue: resource.NewQuantity(int64(target), resource.DecimalSI),
			},
		},
	}
}

// buildHTTPRouteRules constructs the single catch-all rule for a pool's HTTPRoute:
// all traffic goes to the pool Service (load-balanced across every member's pods),
// optionally split with the canary, with an optional per-request timeout.
func buildHTTPRouteRules(svcName string, svcPort int32, canarySvcName string, canaryWeight int32, timeout string) []interface{} {
	catchAllBackends := []interface{}{
		map[string]interface{}{
			"name": svcName,
			"port": int64(svcPort),
		},
	}
	if canarySvcName != "" && canaryWeight > 0 {
		stableWeight := int64(100 - canaryWeight)
		if stableWeight < 0 {
			stableWeight = 0
		}
		catchAllBackends = []interface{}{
			map[string]interface{}{
				"name":   svcName,
				"port":   int64(svcPort),
				"weight": stableWeight,
			},
			map[string]interface{}{
				"name":   canarySvcName,
				"port":   int64(svcPort),
				"weight": int64(canaryWeight),
			},
		}
	}

	rule := map[string]interface{}{"backendRefs": catchAllBackends}
	if timeout != "" {
		rule["timeouts"] = map[string]interface{}{"request": timeout}
	}
	return []interface{}{rule}
}

func (r *BirServiceReconciler) reconcileHTTPRoute(ctx context.Context, bs *deployv1alpha1.BirService, svcName string, svcPort int32, canarySvcName string, canaryWeight int32) error {
	// Build the desired set of HTTPRoutes (one per route entry). Only the pool's
	// primary owns routes; non-primary members and unexposed services own none.
	desired := map[string]map[string]interface{}{}

	if routeIsPrimary(bs) && exposeOrDefault(bs) {
		entries := []deployv1alpha1.RouteEntry{{Name: "default"}}
		if bs.Spec.Route != nil && len(bs.Spec.Route.Entries) > 0 {
			entries = bs.Spec.Route.Entries
		}
		for i, e := range entries {
			// Hostname: explicit if the catalog set one; else auto-derived per cluster.
			// The first (primary) route uses the service's default host (+ spec.hostnames
			// aliases); additional routes get a distinct <name>-<route>-<env> host so two
			// routes on one pool never collide on the same hostname.
			var hostnames []string
			switch {
			case e.Hostname != "":
				hostnames = []string{e.Hostname}
			case i == 0:
				hostnames = r.resolveHostnames(bs)
			default:
				if h := r.routeHostname(bs, e.Name); h != "" {
					hostnames = []string{h}
				}
			}
			if len(hostnames) == 0 {
				continue
			}
			name := fmt.Sprintf("%s-%s-route", bs.Name, e.Name)
			desired[name] = map[string]interface{}{
				"parentRefs": []interface{}{
					map[string]interface{}{"name": gatewayName, "namespace": gatewayNamespace, "sectionName": "http"},
					map[string]interface{}{"name": gatewayName, "namespace": gatewayNamespace, "sectionName": "https"},
				},
				"hostnames": toInterfaceSlice(hostnames),
				"rules":     buildHTTPRouteRules(svcName, svcPort, canarySvcName, canaryWeight, e.Timeout),
			}
		}
	}

	// Delete any HTTPRoutes we own that are no longer desired (entries removed,
	// route dropped, instance demoted to non-primary, or expose turned off).
	listGVK := httpRouteGVK
	listGVK.Kind = httpRouteGVK.Kind + "List"
	existing := &unstructured.UnstructuredList{}
	existing.SetGroupVersionKind(listGVK)
	if err := r.List(ctx, existing, client.InNamespace(bs.Namespace),
		client.MatchingLabels{"app.kubernetes.io/name": bs.Name}); err != nil {
		return client.IgnoreNotFound(err)
	}
	for i := range existing.Items {
		item := &existing.Items[i]
		if _, ok := desired[item.GetName()]; !ok {
			if err := r.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}

	// Create/update the desired routes.
	for name, spec := range desired {
		name, spec := name, spec
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			route := &unstructured.Unstructured{}
			route.SetGroupVersionKind(httpRouteGVK)
			route.SetName(name)
			route.SetNamespace(bs.Namespace)

			_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
				route.Object["spec"] = spec
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
		}); err != nil {
			return err
		}
	}
	return nil
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

	// Non-primary pool members own no Service; the primary's ServiceMonitor already
	// scrapes the whole pool (all members' pods), so they need no monitor of their own.
	if !enabled || !routeIsPrimary(bs) {
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

// toInterfaceSlice converts a []string to []interface{} for unstructured spec maps.
func toInterfaceSlice(in []string) []interface{} {
	out := make([]interface{}, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
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
