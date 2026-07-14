package controller

import (
	"context"
	"crypto/sha256"
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
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

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
	labelApp         = "deploy.easydeploy.io/app"
	labelPurpose     = "deploy.easydeploy.io/purpose"
	annotRebuild     = "deploy.easydeploy.io/rebuild"
	annotPipelineInj = "deploy.easydeploy.io/pipeline-injected"
	// annotHealthyTag / annotRolledBackTag drive operator auto-rollback. They live on
	// the Deployment (operator-owned, not in git, so ArgoCD never fights them):
	// healthy-tag is the last image tag whose rollout became fully available;
	// rolled-back-tag is a tag quarantined after its pods crash-looped, so the
	// operator keeps serving healthy-tag until a new build tag arrives.
	annotHealthyTag    = "deploy.easydeploy.io/healthy-tag"
	annotRolledBackTag = "deploy.easydeploy.io/rolled-back-tag"
	// pipelineWorkflowVersion is bumped whenever the injected workflow template or
	// the values we feed it change in a way that needs to reach already-onboarded
	// repos. The annotation stores the version last injected; a mismatch forces a
	// re-injection so stale workflows (e.g. ones built with the instance name
	// instead of the app/repo name) get overwritten with the correct content.
	pipelineWorkflowVersion = "2"
	requeueBuild            = 10 * time.Second
	// requeueRollout re-checks an in-progress rollout so a crash-looping new version
	// is caught (and rolled back) within ~30-60s without waiting on external events.
	requeueRollout = 15 * time.Second
)

type BirServiceReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	BaseDomain  string
	TargetIP    string
	RegistryURL string
	Environment string
	// PromURL is the in-cluster Prometheus base URL the SLO rollback gate queries
	// (e.g. http://prometheus-operated.monitoring:9090). Empty disables the gate —
	// crash-loop rollback still works, since it reads pod status, not metrics.
	PromURL string
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

	// Auto-rollback: if the desired tag was previously quarantined for crash-looping,
	// keep serving the last healthy tag instead. State lives on the Deployment
	// annotations (operator-owned). Disabled while canary is active — canary drives
	// its own image lifecycle. effectiveTag is what actually gets deployed.
	autoRollback := bs.Spec.Canary == nil || !bs.Spec.Canary.Enabled
	desiredTag := tagFromImage(image)
	healthyTag := preExist.Annotations[annotHealthyTag]
	rolledBackTag := preExist.Annotations[annotRolledBackTag]
	effectiveTag := desiredTag
	if autoRollback && desiredTag != "" && desiredTag == rolledBackTag && healthyTag != "" && healthyTag != desiredTag {
		image = swapImageTag(image, healthyTag)
		effectiveTag = healthyTag
	}

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

	// Resolve the node pool before touching the Deployment. A real API failure blocks
	// and retries; an unknown pool does not block — it pins the pod to an
	// unsatisfiable nodeSelector (so it stays Pending, a visible failure) and records
	// a Warning event rather than silently scheduling on default nodes.
	nodeSelector, tolerations, poolWarn, err := r.resolveNodePool(ctx, bs)
	if err != nil {
		log.FromContext(ctx).Error(err, "nodePool resolution failed", "nodePool", bs.Spec.NodePool)
		_, _ = r.updateBuildStatus(ctx, req, bs, bs.Status.BuildImage, "NodePoolError: "+err.Error(), bs.Status.BuildTag)
		return ctrl.Result{RequeueAfter: requeueBuild}, nil
	}
	if poolWarn != "" {
		log.FromContext(ctx).Info(poolWarn, "nodePool", bs.Spec.NodePool)
		if r.Recorder != nil {
			r.Recorder.Event(bs, corev1.EventTypeWarning, "NodePoolMissing", poolWarn)
		}
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
			// build-tag on the pod (template-only, off the immutable selector) lets the
			// auto-rollback check select this version's pods and watch them for crashes.
			if effectiveTag != "" {
				templateLabels[labelBuildTag] = effectiveTag
			}
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
			// Canonical revision = the deployed build tag, so Istio stamps
			// destination_canonical_revision on every request metric and the SLO gate can
			// attribute errors to ONE version. It must be the build tag, not a constant:
			// maxUnavailable:0 keeps the old version serving while the new one rolls out,
			// so both are in the metric stream at once and a constant revision would blend
			// them — a bad new version would hide behind the healthy old one's traffic.
			// Falls back to "latest" for untagged images (nothing to attribute).
			if effectiveTag != "" {
				templateLabels[labelCanonicalRevision] = effectiveTag
			} else {
				templateLabels[labelCanonicalRevision] = "latest"
			}
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

	// Service topology follows whether the pool weighs its members:
	//
	//   unweighted → ONE shared Service, owned by the primary, selecting the route-group
	//                label. Every member's pods sit behind it, so each member's share of
	//                the traffic is just its share of the pod count — which the HPA moves
	//                around at will.
	//   weighted   → one Service per member, each selecting only its own pods. A weight
	//                can only be attached to a backendRef, so the members have to be
	//                separately addressable before the primary can split traffic by hand.
	//
	// svcName is what the HTTPRoute and ServiceMonitor reference — non-primary members of
	// an unweighted pool reference the pool's Service without owning it.
	weighted := routeIsWeighted(bs)
	svcName := poolServiceName(bs)
	if weighted {
		svcName = instanceServiceName(bs)
	}
	if weighted || routeIsPrimary(bs) {
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

				if weighted {
					// This member's pods only. The weight on the primary's backendRef is what
					// decides this instance's share, so the Service must not reach past itself
					// into the rest of the pool — that would hand it a second, unweighted door.
					svc.Spec.Selector = mergeStringMap(map[string]string{}, labels)
				} else {
					// Select on the route-group label so the pool spans every member's
					// pods (main + testing + …), letting the LB pick any of them.
					svc.Spec.Selector = map[string]string{labelRouteGroup: routeGroup(bs)}
				}
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

	// Adding or removing weights flips the topology, which strands the Service from the
	// other shape: it keeps its ClusterIP and keeps selecting live pods, so any caller
	// still holding it goes on bypassing the split. Delete the one we no longer want.
	if stale := otherTopologyService(bs, weighted); stale != "" && stale != svcName {
		if err := r.deleteOwnedService(ctx, bs, stale); err != nil {
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

	// A weighted pool's route names every member's Service, but each member reconciles on
	// its own clock and a newly added one may not have created its Service yet. Pointing a
	// backendRef at a Service that isn't there 5xx's that member's whole share, so drop it
	// from the split and requeue instead: weights are relative, so until it shows up the
	// members that ARE up divide the traffic among themselves in their declared proportions.
	backends, backendsPending, err := r.resolveRouteBackends(ctx, bs)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileHTTPRoute(ctx, bs, svcName, port, cSvcName, cWeight, backends); err != nil {
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

	if backendsPending {
		// A pool member's Service isn't there yet and its share is currently being served by
		// the other members. Come back and fold it in once it appears.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if autoRollback && desiredTag != "" {
		requeue, err := r.evaluateAutoRollback(ctx, bs, depName, desiredTag, effectiveTag, healthyTag, rolledBackTag)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue > 0 {
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
	}

	return ctrl.Result{}, nil
}

// tagFromImage returns the image tag (the part after the last colon of the final
// path segment), or "" when the reference is untagged. The registry host may contain
// a colon (host:port), so we only look after the last slash.
func tagFromImage(image string) string {
	ref := image
	if slash := strings.LastIndex(image, "/"); slash >= 0 {
		ref = image[slash+1:]
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		return ref[i+1:]
	}
	return ""
}

// swapImageTag replaces the tag of image with newTag, preserving the registry/repo.
func swapImageTag(image, newTag string) string {
	repo := ""
	name := image
	if slash := strings.LastIndex(image, "/"); slash >= 0 {
		repo = image[:slash+1]
		name = image[slash+1:]
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[:i]
	}
	return repo + name + ":" + newTag
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podCrashing reports a pod whose new version can't come up. Two failure shapes:
//   - crash-looping: CrashLoopBackOff, or restarted enough times to be clearly unstable
//   - backed-off start failure the kubelet has already given up retrying: a bad/corrupt
//     image, an invalid reference, or a missing configMap/secret
//
// All are terminal for this rollout — the version won't recover on its own — so they
// warrant a rollback. We key on the *BackOff/*Error states (not the first-attempt
// ErrImagePull) so a transient blip doesn't trip a rollback. Init containers count too:
// a failing init container blocks the pod just the same.
func podCrashing(p *corev1.Pod) bool {
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if cs.RestartCount >= 3 {
			return true
		}
		if cs.State.Waiting == nil {
			continue
		}
		switch cs.State.Waiting.Reason {
		case "CrashLoopBackOff", // app starts then crashes
			"ImagePullBackOff",           // image can't be pulled (corrupt/missing/no creds)
			"InvalidImageName",           // malformed image reference
			"CreateContainerConfigError", // missing configMap/secret referenced by the pod
			"CreateContainerError":       // container runtime rejected the spec
			return true
		}
	}
	return false
}

// podMaxRestarts returns the highest restart count across a pod's containers.
func podMaxRestarts(p *corev1.Pod) int32 {
	var m int32
	for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.RestartCount > m {
			m = cs.RestartCount
		}
	}
	return m
}

// assessTagPods inspects the pods of one image tag (selected via the build-tag pod
// label): how many are Ready, whether any is failing, the highest restart count seen,
// and how long this version has existed. maxRestarts distinguishes a stable version (0)
// from one that flaps Ready but keeps getting killed (>0) — the latter must not be
// recorded as a healthy baseline. age is measured from the OLDEST pod of the tag (when
// the version first appeared) and gates the SLO grace period, so a version is not judged
// on the requests it served while still warming up.
func (r *BirServiceReconciler) assessTagPods(ctx context.Context, ns, name, tag string) (ready int32, crashing bool, maxRestarts int32, age time.Duration, err error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{
		"app.kubernetes.io/name": name,
		labelBuildTag:            tag,
	}); err != nil {
		return 0, false, 0, 0, err
	}
	var oldest time.Time
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if podReady(p) {
			ready++
		}
		if podCrashing(p) {
			crashing = true
		}
		if rc := podMaxRestarts(p); rc > maxRestarts {
			maxRestarts = rc
		}
		if ct := p.CreationTimestamp.Time; !ct.IsZero() && (oldest.IsZero() || ct.Before(oldest)) {
			oldest = ct
		}
	}
	if !oldest.IsZero() {
		age = time.Since(oldest)
	}
	return ready, crashing, maxRestarts, age, nil
}

// evaluateAutoRollback runs the rollback state machine for the current rollout and
// persists healthy-tag / rolled-back-tag on the Deployment. It returns a requeue delay
// while a rollout is still in flight (so a crash is caught within ~30-60s), or 0 once the
// state has settled.
//
// Two independent failure signals feed it:
//   - crash-loop (pod status): the version never comes up. Always enabled.
//   - SLO breach (Istio metrics): the version comes up Ready but burns its error budget.
//     Configured via spec.traffic.autoRollback; only "enforce" mode can roll back.
func (r *BirServiceReconciler) evaluateAutoRollback(ctx context.Context, bs *deployv1alpha1.BirService, depName, desiredTag, effectiveTag, healthyTag, rolledBackTag string) (time.Duration, error) {
	l := log.FromContext(ctx)

	// Already serving the healthy fallback (desiredTag is quarantined): nothing to
	// advance until a new desiredTag — a fix — arrives.
	if effectiveTag != desiredTag {
		return 0, nil
	}

	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: depName, Namespace: bs.Namespace}, &dep); err != nil {
		return 0, client.IgnoreNotFound(err)
	}

	ready, crashing, maxRestarts, versionAge, err := r.assessTagPods(ctx, bs.Namespace, bs.Name, effectiveTag)
	if err != nil {
		return 0, err
	}

	// Fully rolled out = the new template is observed and every replica is available.
	rolledOut := dep.Status.ObservedGeneration >= dep.Generation &&
		dep.Status.UpdatedReplicas == dep.Status.Replicas &&
		dep.Status.UnavailableReplicas == 0 &&
		dep.Status.AvailableReplicas > 0 && ready > 0

	// SLO gate. Only ask Prometheus once the version is actually serving — before that
	// there is nothing to measure. Enforcement additionally requires mode: enforce, so
	// the default (monitor) observes and reports without ever changing the outcome.
	cfg := resolveAutoRollback(bs)
	sloActive := cfg.mode != autoRollbackModeOff && r.PromURL != ""
	enforcing := sloActive && cfg.mode == autoRollbackModeEnforce

	var verdict sloVerdict
	if sloActive && rolledOut {
		verdict = r.evaluateSLO(ctx, bs, cfg, effectiveTag, versionAge)
	}
	sloBreached := verdict.evaluated && verdict.breached
	if sloBreached {
		sloBreachTotal.WithLabelValues(bs.Namespace, bs.Name, cfg.mode).Inc()
		l.Info("SLO gate: version is breaching its objective",
			"tag", desiredTag, "mode", cfg.mode, "reason", verdict.reason)
		if !enforcing && r.Recorder != nil {
			// monitor mode: report only. This is the dry run — the state machine below
			// proceeds exactly as it would without the gate.
			r.Recorder.Eventf(bs, corev1.EventTypeWarning, "SLOBreach",
				"tag %s is breaching its SLO (%s); autoRollback.mode is %q, so no rollback was performed",
				desiredTag, verdict.reason, cfg.mode)
		}
	}

	// A Ready version is not trustworthy the moment its pods go green — a bad build serves
	// errors while looking perfectly healthy to the kubelet. Keep watching until the SLO
	// window has actually elapsed, and only then let it become the rollback baseline.
	// Without this, the baseline would be recorded ~30s in, and a breach detected at ~2m
	// would find its only fallback IS the bad version ("no previous healthy version").
	// When the gate is off (or Prometheus is absent) there is nothing to wait for.
	observationDone := !sloActive || versionAge >= cfg.observationPeriod()

	// Only a STABLE rollout becomes the healthy baseline: every replica available with
	// zero restarts. A version that flaps Ready but keeps getting killed (liveness
	// failures → restarts) must not poison the baseline. "latest" is never a valid
	// baseline — the pipeline pushes immutable SHA tags, so latest is a non-deployable
	// fallback, not a real version.
	stableHealthy := rolledOut && !crashing && maxRestarts == 0 && desiredTag != "latest" && observationDone

	newHealthy, newRolledBack := healthyTag, rolledBackTag

	switch {
	case crashing && ready == 0:
		// Desired version is failing and never became ready. Prefer the recorded
		// healthy tag; if none was ever recorded (first sight of the service, or it was
		// healthy before auto-rollback existed) fall back to the tag of a pod that is
		// still Ready right now — the previous version maxUnavailable:0 kept serving.
		newHealthy, newRolledBack = r.quarantineTag(ctx, bs, desiredTag, healthyTag, newRolledBack,
			fmt.Sprintf("tag %s failed to roll out", desiredTag))
	case enforcing && sloBreached:
		// Desired version came up Ready but is burning its error budget. Same quarantine
		// as a crash: revert to the last healthy tag and keep serving it until a new build
		// arrives. Unlike the crash path there are no old pods left to bootstrap from (the
		// rollout completed), so this relies on the healthy-tag recorded by the previous
		// good deploy — which is exactly why observationDone gates that recording.
		newHealthy, newRolledBack = r.quarantineTag(ctx, bs, desiredTag, healthyTag, newRolledBack,
			fmt.Sprintf("tag %s breached its SLO (%s)", desiredTag, verdict.reason))
	case stableHealthy:
		// Desired version is stably healthy AND has survived its SLO window — lock it in
		// as the rollback target.
		newHealthy = desiredTag
		if newRolledBack == desiredTag {
			newRolledBack = ""
		}
	default:
		// Still progressing, or Ready but still inside its SLO observation window.
		// Persist unchanged state and re-check shortly.
		if err := r.setDeploymentRollbackAnnotations(ctx, depName, bs.Namespace, newHealthy, newRolledBack); err != nil {
			return 0, err
		}
		return requeueRollout, nil
	}

	if err := r.setDeploymentRollbackAnnotations(ctx, depName, bs.Namespace, newHealthy, newRolledBack); err != nil {
		return 0, err
	}
	// Freshly quarantined → requeue so the next pass redeploys the healthy tag.
	// Not yet settled healthy → keep polling.
	if newRolledBack != rolledBackTag || newHealthy != desiredTag {
		return requeueRollout, nil
	}
	return 0, nil
}

// quarantineTag condemns desiredTag and returns the (healthy, rolled-back) annotation
// values that revert to the last good version: the next reconcile sees desiredTag ==
// rolled-back-tag and swaps the image back to healthy, which keeps serving until a NEW
// build tag — a fix — arrives.
//
// Shared by the crash-loop and SLO paths: they differ only in how a version was condemned,
// never in what happens next. reason is a sentence fragment describing the condemnation
// ("tag abc123 failed to roll out"), used verbatim in the event.
//
// When there is no good version to fall back to, nothing is quarantined — rolling back to
// a version we have never seen working would trade one outage for another. The caller's
// state is returned unchanged and a DeployFailed event says so.
func (r *BirServiceReconciler) quarantineTag(ctx context.Context, bs *deployv1alpha1.BirService, desiredTag, healthyTag, rolledBackTag, reason string) (string, string) {
	l := log.FromContext(ctx)

	fallback := healthyTag
	if fallback == "" {
		fallback = r.bootstrapHealthyTag(ctx, bs, desiredTag)
	}
	if fallback == "" || fallback == desiredTag {
		if r.Recorder != nil {
			r.Recorder.Eventf(bs, corev1.EventTypeWarning, "DeployFailed",
				"%s and there is no previous healthy version to roll back to", reason)
		}
		return healthyTag, rolledBackTag
	}

	l.Info("auto-rollback: quarantining tag", "tag", desiredTag, "rollbackTo", fallback, "reason", reason)
	if r.Recorder != nil {
		r.Recorder.Eventf(bs, corev1.EventTypeWarning, "AutoRollback",
			"%s; rolling back to healthy tag %s", reason, fallback)
	}
	return fallback, desiredTag
}

// bootstrapHealthyTag derives a rollback target when none was recorded yet: the app
// image tag of a pod that is still Ready right now and runs a different tag than the
// (failing) desired one. This covers a service that was healthy before auto-rollback
// existed, or the operator's first sight of it — the previous version is still serving
// (maxUnavailable:0 kept it), so its tag is a safe fallback. "" if there is none.
func (r *BirServiceReconciler) bootstrapHealthyTag(ctx context.Context, bs *deployv1alpha1.BirService, desiredTag string) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(bs.Namespace), client.MatchingLabels{
		"app.kubernetes.io/name": bs.Name,
	}); err != nil {
		return ""
	}
	repoPrefix := fmt.Sprintf("%s/%s:", r.effectiveRegistryURL(), appName(bs))
	// Pick the newest Ready pod's tag: if several healthy versions still have pods
	// (e.g. a prior rollout mid-termination), roll back to the most recent known-good
	// one, not an arbitrary older one. Pod list order is not guaranteed.
	var bestTag string
	var bestTime time.Time
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil || !podReady(p) {
			continue
		}
		for _, c := range p.Spec.Containers {
			if !strings.HasPrefix(c.Image, repoPrefix) {
				continue
			}
			tag := strings.TrimPrefix(c.Image, repoPrefix)
			// Skip the (non-deployable) latest fallback and the failing tag itself.
			if tag == "" || tag == desiredTag || tag == "latest" {
				continue
			}
			if bestTag == "" || p.CreationTimestamp.Time.After(bestTime) {
				bestTag, bestTime = tag, p.CreationTimestamp.Time
			}
		}
	}
	return bestTag
}

// setDeploymentRollbackAnnotations writes (or clears) the auto-rollback state
// annotations on the Deployment, a no-op when unchanged.
func (r *BirServiceReconciler) setDeploymentRollbackAnnotations(ctx context.Context, depName, ns, healthy, rolledBack string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var dep appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: depName, Namespace: ns}, &dep); err != nil {
			return client.IgnoreNotFound(err)
		}
		if dep.Annotations == nil {
			dep.Annotations = map[string]string{}
		}
		changed := false
		setOrDelete := func(key, val string) {
			if val == "" {
				if _, ok := dep.Annotations[key]; ok {
					delete(dep.Annotations, key)
					changed = true
				}
			} else if dep.Annotations[key] != val {
				dep.Annotations[key] = val
				changed = true
			}
		}
		setOrDelete(annotHealthyTag, healthy)
		setOrDelete(annotRolledBackTag, rolledBack)
		if !changed {
			return nil
		}
		return r.Update(ctx, &dep)
	})
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
// the pod schedules onto the default, untainted nodes).
//
// A named-but-missing pool does NOT block the deploy and does NOT silently land on
// the default nodes: it returns a best-effort nodeSelector {nodePool: <name>} (the
// platform's node-label convention) plus a warning. With no node carrying that label
// the pod stays Pending — a visible failure — and it self-heals once the Pool
// resource (and a matching node) appears, since the next reconcile takes the
// resolved path. The string return is that warning (empty on a clean resolve); err is
// reserved for real API failures (transient — the caller blocks and retries those).
func (r *BirServiceReconciler) resolveNodePool(ctx context.Context, bs *deployv1alpha1.BirService) (map[string]string, []corev1.Toleration, string, error) {
	if bs.Spec.NodePool == "" {
		return nil, nil, "", nil
	}
	var pool deployv1alpha1.Pool
	if err := r.Get(ctx, types.NamespacedName{Name: bs.Spec.NodePool}, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			warn := fmt.Sprintf("nodePool %q not found; pinned to nodePool=%s so pods stay Pending until a Pool resource and a matching node exist", bs.Spec.NodePool, bs.Spec.NodePool)
			return map[string]string{"nodePool": bs.Spec.NodePool}, nil, warn, nil
		}
		return nil, nil, "", err
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
	return nodeSelector, tolerations, "", nil
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
	// Ensure the operator-owned prometheus-adapter config exists at startup, even
	// with zero BirServices — otherwise the adapter pod blocks on the missing
	// ConfigMap volume (the chart mounts it via rules.existing).
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return fmt.Errorf("cache sync failed before adapter config bootstrap")
		}
		if err := r.reconcileAdapterConfig(ctx); err != nil {
			log.FromContext(ctx).Error(err, "initial prometheus-adapter config bootstrap failed")
		}
		return nil
	})); err != nil {
		return err
	}
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

// poolServiceName is the Service that fronts a whole pool: <group>-svc. It selects the
// route-group label, so it spans every member's pods.
func poolServiceName(bs *deployv1alpha1.BirService) string {
	return fmt.Sprintf("%s-svc", routeGroup(bs))
}

// backendServiceName is the Service fronting one pool member, named after its BirService.
// The chart puts BirService names in route.backends; this is how they resolve to Services.
func backendServiceName(instance string) string {
	return fmt.Sprintf("%s-svc", instance)
}

// instanceServiceName is the Service that fronts ONE instance's pods: <name>-svc. Weighted
// pools use these — a weight can only be applied to a backendRef, so each member has to be
// separately addressable. (Canary Services are <name>-canary-svc, so they never collide.)
func instanceServiceName(bs *deployv1alpha1.BirService) string {
	return backendServiceName(bs.Name)
}

// routeIsWeighted reports whether this instance's pool splits traffic by weight. The chart
// sets it on every member of the pool, so members agree without reading each other.
func routeIsWeighted(bs *deployv1alpha1.BirService) bool {
	return bs.Spec.Route != nil && bs.Spec.Route.Weighted
}

// otherTopologyService names the Service this instance would own under the OPPOSITE
// weighting — the one left stranded when weights are added or removed. A non-primary
// member never owns a pool Service, so the only thing it can strand is its per-instance
// one; returning "" for it keeps a member from deleting the primary's Service.
func otherTopologyService(bs *deployv1alpha1.BirService, weighted bool) string {
	if !weighted {
		return instanceServiceName(bs)
	}
	if routeIsPrimary(bs) {
		return poolServiceName(bs)
	}
	return ""
}

// resolveRouteBackends returns the weighted backends this instance's HTTPRoute should
// carry, keeping only the members whose Service already exists, plus whether any were
// held back. Non-primary members and unweighted pools have none, and fall through to the
// pool Service (or the canary split) instead.
func (r *BirServiceReconciler) resolveRouteBackends(ctx context.Context, bs *deployv1alpha1.BirService) ([]deployv1alpha1.RouteBackend, bool, error) {
	if !routeIsWeighted(bs) || !routeIsPrimary(bs) || bs.Spec.Route == nil {
		return nil, false, nil
	}
	var ready []deployv1alpha1.RouteBackend
	pending := false
	for _, b := range bs.Spec.Route.Backends {
		var svc corev1.Service
		key := types.NamespacedName{Name: backendServiceName(b.Name), Namespace: bs.Namespace}
		switch err := r.Get(ctx, key, &svc); {
		case err == nil:
			ready = append(ready, b)
		case apierrors.IsNotFound(err):
			pending = true
		default:
			return nil, false, err
		}
	}
	return ready, pending, nil
}

// deleteOwnedService removes a Service this operator created, if it is still there and
// still ours. The managed-by check matters: a pool whose primary instance is named after
// its route resolves both Service names to the same string, and more generally a tenant
// may have hand-made a Service under a name we now want to reclaim — neither should turn
// a topology switch into someone else's outage.
func (r *BirServiceReconciler) deleteOwnedService(ctx context.Context, bs *deployv1alpha1.BirService, name string) error {
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: bs.Namespace}, svc); err != nil {
		return client.IgnoreNotFound(err)
	}
	if svc.Labels["app.kubernetes.io/managed-by"] != "easy-deploy-operator" {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, svc))
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

	// Keep the operator-owned prometheus-adapter config in sync with the rps windows
	// requested across all services (adds/prunes external rules, rolls the adapter on
	// change). Best-effort — a failure here must not block HPA creation.
	if err := r.reconcileAdapterConfig(ctx); err != nil {
		log.FromContext(ctx).Error(err, "failed to reconcile prometheus-adapter config")
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
//	rps    → External istio_requests_per_second_<window> (AverageValue, req/s) — prometheus-adapter
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

// rpsExternalMetric builds the External istio_requests_per_second_<window> metric
// for a per-pod requests/sec target. prometheus-adapter serves a distinct external
// metric per rate window (istio_requests_per_second_1m, _2m, …), each backed by an
// operator-generated rule that computes sum(rate(istio_requests_total[window])) live
// — no recording rule, no evaluation delay. destination_workload picks this
// workload's series while the namespace label is injected by the adapter from the
// HPA's namespace (resource override), so two same-named workloads in different
// namespaces don't cross-count. AverageValue divides total mesh RPS by the target.
func rpsExternalMetric(depName string, targetRPS int32, window string) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ExternalMetricSourceType,
		External: &autoscalingv2.ExternalMetricSource{
			Metric: autoscalingv2.MetricIdentifier{
				Name: rpsMetricNameForWindow(window),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"destination_workload": depName,
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
	// empty. The operator always emits an adapter rule for this window so the
	// default rps metric always has a backing series.
	defaultRPSWindow = "1m"
	// adapterConfigMapName is the operator-owned ConfigMap holding the entire
	// prometheus-adapter rules config (config.yaml). The adapter mounts it via the
	// chart's rules.existing setting, so the operator — not Helm — owns the rules.
	adapterConfigMapName = "easy-deploy-adapter-config"
	// adapterConfigKey is the key the adapter reads (--config=/etc/adapter/config.yaml).
	adapterConfigKey = "config.yaml"
	// adapterDeploymentName is the prometheus-adapter Deployment the operator rolls
	// when the config changes (the adapter reads its config only at startup).
	adapterDeploymentName = "prometheus-adapter"
	// adapterConfigChecksumAnnotation carries a hash of the rendered config on the
	// adapter pod template; bumping it forces a rollout onto the new config. ArgoCD
	// ignores this path (RespectIgnoreDifferences) so it doesn't fight the operator.
	adapterConfigChecksumAnnotation = "easy-deploy.io/config-checksum"
)

// adapterNamespace is where prometheus-adapter and its config ConfigMap live.
func adapterNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("ADAPTER_NAMESPACE")); ns != "" {
		return ns
	}
	return "monitoring"
}

// rpsMetricNameForWindow returns the external metric name the adapter serves for a
// rate window (e.g. 1m -> istio_requests_per_second_1m). A Prometheus duration's
// chars ([0-9smhdwy]) are all valid in a metric name, so no sanitisation is needed.
func rpsMetricNameForWindow(window string) string {
	return "istio_requests_per_second_" + strings.TrimSpace(window)
}

// promDurationRe matches a Prometheus range duration (e.g. 30s, 1m, 2m30s, 1h).
var promDurationRe = regexp.MustCompile(`^([0-9]+(ms|s|m|h|d|w|y))+$`)

func validPromDuration(s string) bool {
	return promDurationRe.MatchString(strings.TrimSpace(s))
}

// adapterWindowRe captures the rate window from a generated external metric name
// (istio_requests_per_second_<window>) in an existing rendered adapter config.
var adapterWindowRe = regexp.MustCompile(`istio_requests_per_second_([0-9a-z]+)`)

// existingAdapterWindows returns the rate windows already present in a rendered
// adapter config, so the operator can keep (never prune) windows it has served.
func existingAdapterWindows(config string) []string {
	if config == "" {
		return nil
	}
	var out []string
	for _, m := range adapterWindowRe.FindAllStringSubmatch(config, -1) {
		if w := m[1]; validPromDuration(w) {
			out = append(out, w)
		}
	}
	return out
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

// reconcileAdapterConfig renders the entire prometheus-adapter rules config from the
// set of rate windows requested across all BirServices and writes it to the
// operator-owned ConfigMap (adapterConfigMapName, mounted via the chart's
// rules.existing). Each distinct window becomes one external rule that computes istio
// rps live — sum(rate(istio_requests_total[w])) — so there is no recording-rule
// indirection or evaluation delay; the static worker custom rule is always included.
// When the rendered config changes the adapter Deployment is rolled (it reads its
// config only at startup).
//
// Windows accumulate and are never pruned: the set is the union of windows already
// present in the config with those currently requested. A window that no service
// uses anymore (e.g. a service switched 5m -> 10m) keeps its rule, so steady-state
// edits don't churn the adapter and any HPA still referencing the old metric name
// keeps working. Only a genuinely new window adds a rule and triggers a rollout.
func (r *BirServiceReconciler) reconcileAdapterConfig(ctx context.Context) error {
	ns := adapterNamespace()

	windowSet := map[string]bool{defaultRPSWindow: true}

	// Seed with windows already written, so switching/removing a service's window
	// never drops its rule.
	existing := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: adapterConfigMapName, Namespace: ns}, existing); err == nil {
		for _, w := range existingAdapterWindows(existing.Data[adapterConfigKey]) {
			windowSet[w] = true
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	var list deployv1alpha1.BirServiceList
	if err := r.List(ctx, &list); err != nil {
		return err
	}
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

	config := renderAdapterConfig(windows)

	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm := &corev1.ConfigMap{}
		cm.Name = adapterConfigMapName
		cm.Namespace = ns
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			cm.Labels = mergeStringMap(cm.Labels, map[string]string{
				"app.kubernetes.io/managed-by": "easy-deploy-operator",
			})
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			cm.Data[adapterConfigKey] = config
			return nil
		})
		return err
	}); err != nil {
		return err
	}

	return r.rolloutAdapter(ctx, ns, config)
}

// renderAdapterConfig builds the prometheus-adapter config.yaml: the static worker
// custom-metric rule plus one external rule per rate window. The format is the
// adapter's own (rules = custom metrics, externalRules = external metrics), not the
// Helm chart's values shape.
func renderAdapterConfig(windows []string) string {
	var b strings.Builder
	b.WriteString(`rules:
- seriesQuery: 'app_worker_utilization{namespace!="",pod!=""}'
  resources:
    overrides:
      namespace: {resource: namespace}
      pod: {resource: pod}
  name:
    matches: "^app_worker_utilization$"
    as: "app_worker_utilization"
  metricsQuery: 'avg(<<.Series>>{<<.LabelMatchers>>}) by (<<.GroupBy>>)'
externalRules:
`)
	for _, w := range windows {
		fmt.Fprintf(&b, `- seriesQuery: 'istio_requests_total{destination_workload!="",destination_workload_namespace!=""}'
  resources:
    overrides:
      destination_workload_namespace: {resource: namespace}
  name:
    matches: "^istio_requests_total$"
    as: "%s"
  metricsQuery: 'sum(rate(istio_requests_total{<<.LabelMatchers>>}[%s])) by (destination_workload, destination_workload_namespace)'
`, rpsMetricNameForWindow(w), w)
	}
	return b.String()
}

// rolloutAdapter bumps the config-checksum annotation on the prometheus-adapter
// Deployment's pod template when the rendered config changes, forcing a rollout onto
// the new config (the adapter does not hot-reload). A no-op when the checksum already
// matches, so steady-state reconciles don't churn the adapter; a missing Deployment
// (e.g. fresh cluster, adapter not yet synced) is ignored — the ConfigMap is enough.
func (r *BirServiceReconciler) rolloutAdapter(ctx context.Context, ns, config string) error {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(config)))
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: adapterDeploymentName, Namespace: ns}, dep); err != nil {
			return client.IgnoreNotFound(err)
		}
		if dep.Spec.Template.Annotations[adapterConfigChecksumAnnotation] == sum {
			return nil
		}
		dep.Spec.Template.Annotations = mergeStringMap(dep.Spec.Template.Annotations, map[string]string{
			adapterConfigChecksumAnnotation: sum,
		})
		return r.Update(ctx, dep)
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

// buildHTTPRouteRules constructs the single catch-all rule for a pool's HTTPRoute. The
// backends take one of three shapes, in order of precedence:
//
//   - weighted pool: one backendRef per member, each pinned to its declared share. A
//     member keeps that share however many pods it runs, which is the whole point —
//     under the unweighted shape a member's share is its share of the pod count, so an
//     HPA scaling one member quietly re-splits everyone's traffic.
//   - canary: the classic stable/canary two-way split.
//   - neither: one backendRef to the pool Service.
//
// Weights are relative, not absolute, so a short backends list still adds up: if a
// member's Service is missing the caller leaves it out and the rest split its share.
func buildHTTPRouteRules(svcName string, svcPort int32, canarySvcName string, canaryWeight int32, timeout string, backends []deployv1alpha1.RouteBackend) []interface{} {
	var catchAllBackends []interface{}
	switch {
	case len(backends) > 0:
		for _, b := range backends {
			catchAllBackends = append(catchAllBackends, map[string]interface{}{
				"name":   backendServiceName(b.Name),
				"port":   int64(svcPort),
				"weight": int64(b.Weight),
			})
		}
	case canarySvcName != "" && canaryWeight > 0:
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
	default:
		catchAllBackends = []interface{}{
			map[string]interface{}{
				"name": svcName,
				"port": int64(svcPort),
			},
		}
	}

	rule := map[string]interface{}{"backendRefs": catchAllBackends}
	if timeout != "" {
		rule["timeouts"] = map[string]interface{}{"request": timeout}
	}
	return []interface{}{rule}
}

func (r *BirServiceReconciler) reconcileHTTPRoute(ctx context.Context, bs *deployv1alpha1.BirService, svcName string, svcPort int32, canarySvcName string, canaryWeight int32, backends []deployv1alpha1.RouteBackend) error {
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
				"rules":     buildHTTPRouteRules(svcName, svcPort, canarySvcName, canaryWeight, e.Timeout, backends),
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
