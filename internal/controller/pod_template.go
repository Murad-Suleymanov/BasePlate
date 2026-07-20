package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

// Pod-template construction shared by every Deployment the operator creates for a
// service: the main one and the temporary next-revision one a progressive rollout
// runs beside it.
//
// This file exists because the pieces below are exactly the pieces that drifted.
// The canary Deployment was written as its own copy of the main one and, over time,
// stopped matching it: no canonical-name/revision labels (so Istio reported canary
// traffic as a SEPARATE service and the SLO gate could not see it at all), no
// build-tag label (so the crash detector's pod lookup skipped it), and no readiness
// probe (so traffic reached pods that were merely Running). A canary nobody can
// measure is worse than none — it carries real user traffic while looking safe.
//
// Sharing the construction makes that class of bug impossible rather than unlikely:
// a new label added for the main Deployment lands on every revision automatically.

// revisionPodLabels builds the pod-template labels for one revision of a service.
//
// selector is the Deployment's immutable selector — the template must be a superset
// of it. Everything added here is template-only on purpose, so it can change on a
// live service without recreating the Deployment.
//
// joinPool decides whether these pods answer on the pool's Service. The main
// Deployment's pods do. A next-revision Deployment's pods deliberately do NOT: the
// pool Service selects on the route-group label alone, so joining would let any
// in-mesh caller that dials the pool Service directly reach the new version by pod
// count — bypassing the weight entirely, and making the measured traffic share
// differ from the declared one. Reachable only through its own Service, the new
// revision takes exactly the share the HTTPRoute gives it.
func revisionPodLabels(bs *deployv1alpha1.BirService, selector map[string]string, tag string, joinPool bool) map[string]string {
	labels := mergeStringMap(map[string]string{}, selector)

	// build-tag lets the rollback path select this version's pods and watch them.
	if tag != "" {
		labels[labelBuildTag] = tag
	}

	if joinPool {
		labels[labelRouteGroup] = routeGroup(bs)
	}

	// Istio canonical service. Derived from app.kubernetes.io/name when absent, which
	// is wrong for anything whose selector is not the bare app name: pool members
	// would each report as their own service, and a next-revision Deployment (whose
	// selector must differ from main's, or the two would fight over the same pods)
	// would report as a service of its own. Pinning it to the app name keeps every
	// revision and every member under one canonical service, which is what makes the
	// SLO queries in slo_rollback.go address the right thing.
	labels[labelCanonicalName] = appName(bs)

	// Canonical revision = the build tag, so Istio stamps
	// destination_canonical_revision on every request metric and one version can be
	// judged apart from another. It must not be a constant: with maxUnavailable:0 —
	// and, during a ramp, with two Deployments live — several versions are in the
	// metric stream at once, and a constant would blend them so a bad new version
	// hides behind the healthy old one's traffic.
	if tag != "" {
		labels[labelCanonicalRevision] = tag
	} else {
		labels[labelCanonicalRevision] = "latest"
	}

	if bsNeedsWaypoint(bs) {
		labels[labelUseWaypoint] = waypointName
	}

	return labels
}

// appContainer builds the application container for one revision.
//
// podAnnotations feeds tracing env resolution and is read, never written.
func appContainer(
	bs *deployv1alpha1.BirService,
	image string,
	containerPort int32,
	resources corev1.ResourceRequirements,
	podAnnotations map[string]string,
) corev1.Container {
	preStopSleep, _ := resolveShutdown(bs)

	container := corev1.Container{
		Name:            "app",
		Image:           image,
		ImagePullPolicy: corev1.PullAlways,
		Ports: []corev1.ContainerPort{
			{ContainerPort: containerPort},
		},
		Resources: resources,
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "-c", fmt.Sprintf("sleep %d", preStopSleep)},
				},
			},
		},
		Env: tracingContainerEnv(bs, podAnnotations),
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
		// Default TCP readiness probe — guarantees zero-downtime rolling updates even
		// when users forget to declare an HTTP probe. Without this, K8s marks pods
		// Ready as soon as the container is Running, causing premature old-pod
		// termination while the new app is still initializing. During a progressive
		// ramp the same probe is what keeps traffic off a new revision until it can
		// actually serve — a step that opened onto a warming pod would read as an SLO
		// breach caused by the deploy mechanism rather than by the code.
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

	return container
}
