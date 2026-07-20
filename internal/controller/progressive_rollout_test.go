package controller

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func progressiveBS(ns string) *deployv1alpha1.BirService {
	bs := &deployv1alpha1.BirService{}
	bs.Name = "app"
	bs.Namespace = ns
	bs.Status.BuildStatus = "Succeeded"
	return bs
}

func progressiveReq(bs *deployv1alpha1.BirService) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: bs.Name, Namespace: bs.Namespace}}
}

func newProgressiveClient(t *testing.T, objs ...client.Object) *BirServiceReconciler {
	t.Helper()
	s := rollbackScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&deployv1alpha1.BirService{}).
		WithObjects(objs...).
		Build()
	return &BirServiceReconciler{Client: cl, Scheme: s}
}

// mainDepAtTag is the instance's Deployment serving a given tag. runningInstanceTag
// reads the pod-template build-tag label off exactly this.
func mainDepAtTag(ns, tag string) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-deploy", Namespace: ns, Generation: 1}}
	d.Spec.Template.Labels = map[string]string{labelBuildTag: tag}
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 4, UpdatedReplicas: 4, AvailableReplicas: 4}
	return d
}

func TestSanitizeRolloutSteps(t *testing.T) {
	cases := []struct {
		name string
		in   []int32
		want int
	}{
		{"valid ascending", []int32{5, 25, 50}, 3},
		{"single step", []int32{10}, 1},
		{"not increasing", []int32{25, 5}, 0},
		{"duplicate", []int32{10, 10}, 0},
		{"zero", []int32{0, 10}, 0},
		{"negative", []int32{-5, 10}, 0},
		// 100 means "the temporary Deployment serves everything", on replicas sized
		// for a fraction of the service. Promotion exists for that.
		{"hundred rejected", []int32{5, 100}, 0},
		{"over hundred", []int32{5, 150}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := len(sanitizeRolloutSteps(c.in)); got != c.want {
				t.Errorf("sanitizeRolloutSteps(%v) length = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// No spec.rollout is the default and must leave the deploy path completely alone.
func TestResolveRolloutDefaultsToImmediate(t *testing.T) {
	bs := progressiveBS("t")
	if got := resolveRollout(bs); got.progressive {
		t.Fatal("a service with no spec.rollout must not be progressive")
	}
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "immediate"}
	if got := resolveRollout(bs); got.progressive {
		t.Fatal("strategy: immediate must not be progressive")
	}
}

func TestResolveRolloutProgressiveAndOverrides(t *testing.T) {
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{
		Strategy:        "progressive",
		Steps:           []int32{10, 40},
		StepDuration:    "3m",
		MaxStepDuration: "20m",
		Analysis:        "enforce",
	}
	got := resolveRollout(bs)
	if !got.progressive || !got.enforce {
		t.Fatalf("expected progressive+enforce, got %+v", got)
	}
	if len(got.steps) != 2 || got.steps[0] != 10 || got.steps[1] != 40 {
		t.Errorf("steps = %v, want [10 40]", got.steps)
	}
	if got.stepDur != 3*time.Minute || got.maxStepDur != 20*time.Minute {
		t.Errorf("durations = %v/%v, want 3m/20m", got.stepDur, got.maxStepDur)
	}
}

// A bad step list or duration must fall back to the safe default, never to zero:
// a zero soak would walk a version through every step without measuring it.
func TestResolveRolloutInvalidInputFallsBackToDefaults(t *testing.T) {
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{
		Strategy:     "progressive",
		Steps:        []int32{50, 10},
		StepDuration: "banana",
	}
	got := resolveRollout(bs)
	if got.stepDur != stepDurationFor(got.sloWindow) {
		t.Errorf("stepDur = %v, want the derived default %v", got.stepDur, stepDurationFor(got.sloWindow))
	}
	if len(got.steps) != len(defaultRolloutSteps) {
		t.Errorf("steps = %v, want the default %v", got.steps, defaultRolloutSteps)
	}
}

// The soak has to outlast the SLO window, or the query that judges a step still
// contains the previous step's traffic and dilutes the very signal being read. A
// hardcoded default could not hold that property — it silently equalled the default
// window — so the soak is derived from the window instead.
func TestStepDurationOutlastsSLOWindow(t *testing.T) {
	// Default service: window comes from the autoRollback default (2m).
	bs := meshBS(nil)
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	got := resolveRollout(bs)
	if got.sloWindow != 2*time.Minute {
		t.Fatalf("sloWindow = %v, want the 2m platform default", got.sloWindow)
	}
	if got.stepDur <= got.sloWindow {
		t.Errorf("stepDur %v must exceed the SLO window %v", got.stepDur, got.sloWindow)
	}
	if got.dilutedWindow {
		t.Error("the derived default must not be diluted")
	}

	// A service that widens its window must get a longer soak automatically.
	wide := meshBS(&deployv1alpha1.AutoRollbackSpec{Window: "10m"})
	wide.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	got = resolveRollout(wide)
	if got.sloWindow != 10*time.Minute {
		t.Fatalf("sloWindow = %v, want 10m", got.sloWindow)
	}
	if got.stepDur <= got.sloWindow {
		t.Errorf("stepDur %v must track the widened window %v", got.stepDur, got.sloWindow)
	}
}

// An explicit soak shorter than the window is honoured, but flagged — the developer
// may have a reason, and silently overriding them would be worse than telling them.
func TestExplicitShortStepDurationIsFlaggedNotOverridden(t *testing.T) {
	bs := meshBS(&deployv1alpha1.AutoRollbackSpec{Window: "5m"})
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive", StepDuration: "2m"}

	got := resolveRollout(bs)
	if got.stepDur != 2*time.Minute {
		t.Errorf("stepDur = %v, want the explicit 2m to be honoured", got.stepDur)
	}
	if !got.dilutedWindow {
		t.Error("a 2m soak against a 5m window must be flagged as diluted")
	}
}

// maxStepDuration below stepDuration would hold every step before it could ever be
// judged, so it is floored at the soak.
func TestResolveRolloutMaxStepDurationFloor(t *testing.T) {
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{
		Strategy: "progressive", StepDuration: "5m", MaxStepDuration: "1m",
	}
	if got := resolveRollout(bs); got.maxStepDur != got.stepDur {
		t.Errorf("maxStepDur = %v, want it floored to stepDur %v", got.maxStepDur, got.stepDur)
	}
}

// The replica formula equalizes PER-POD load. Getting this wrong is what makes a
// healthy version look slow: one pod holding 50%% next to four sharing the other 50%%
// carries four times the traffic each of them does, and fails a latency objective it
// would meet at the same per-pod load.
func TestNextRevisionReplicasEqualizesPerPodLoad(t *testing.T) {
	cases := []struct {
		stable, weight, want int32
	}{
		{4, 5, 1},   // 4*5/95 = 0.21 → 1
		{4, 25, 2},  // 4*25/75 = 1.33 → 2
		{4, 50, 4},  // 4*50/50 = 4
		{10, 10, 2}, // 10*10/90 = 1.11 → 2
		{1, 50, 1},
		{4, 0, 1},   // no traffic yet, still needs a pod to become Ready
		{4, 100, 4}, // never reached in practice; must not divide by zero
		{0, 25, 1},  // unknown stable count degrades safely
	}
	for _, c := range cases {
		if got := nextRevisionReplicas(c.stable, c.weight); got != c.want {
			t.Errorf("nextRevisionReplicas(stable=%d, weight=%d) = %d, want %d",
				c.stable, c.weight, got, c.want)
		}
	}
}

// The next-revision pods must be measurable by the same SLO queries that judge the
// instance. This is the exact drift that made the canary Deployment invisible.
func TestRevisionPodLabelsCarryIstioIdentity(t *testing.T) {
	bs := progressiveBS("t")
	selector := map[string]string{"app.kubernetes.io/name": "app-next"}

	labels := revisionPodLabels(bs, selector, "v2", false)

	// Canonical name must be the APP, not the Deployment's selector name — otherwise
	// Istio reports the new revision as a separate service and the SLO gate, which
	// queries destination_canonical_service, cannot see it at all.
	if got := labels[labelCanonicalName]; got != appName(bs) {
		t.Errorf("canonical-name = %q, want %q", got, appName(bs))
	}
	// Canonical revision is what separates this version's metrics from the running
	// one's while both serve at the same time.
	if got := labels[labelCanonicalRevision]; got != "v2" {
		t.Errorf("canonical-revision = %q, want v2", got)
	}
	// build-tag is how the crash detector's pod lookup finds these pods.
	if got := labels[labelBuildTag]; got != "v2" {
		t.Errorf("build-tag = %q, want v2", got)
	}
	// joinPool=false: the pool Service selects on route-group alone, so carrying it
	// would let in-mesh callers reach the new revision by pod count and bypass the
	// weight entirely.
	if _, ok := labels[labelRouteGroup]; ok {
		t.Error("next-revision pods must NOT carry route-group, or they answer on the pool Service outside the weighted split")
	}

	// The instance's own pods do join the pool.
	mainLabels := revisionPodLabels(bs, map[string]string{"app.kubernetes.io/name": "app"}, "v1", true)
	if mainLabels[labelRouteGroup] != routeGroup(bs) {
		t.Error("main pods must carry route-group")
	}
}

// Traffic must never reach a pod that is merely Running. The canary Deployment had
// no probe at all; every revision gets one whether or not the developer declared it.
func TestAppContainerAlwaysHasReadinessProbe(t *testing.T) {
	bs := progressiveBS("t")
	c := appContainer(bs, "reg/app:v2", 8080, corev1.ResourceRequirements{}, nil)
	if c.ReadinessProbe == nil {
		t.Fatal("every revision must have a readiness probe, declared or defaulted")
	}
	if c.ReadinessProbe.TCPSocket == nil {
		t.Error("with no declared probe the default should be a TCP check on the container port")
	}

	bs.Spec.ReadinessProbe = &deployv1alpha1.ProbeSpec{Path: "/healthz"}
	c = appContainer(bs, "reg/app:v2", 8080, corev1.ResourceRequirements{}, nil)
	if c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Error("a declared HTTP probe should be honoured")
	}
}

// The default path must not create a next-revision Deployment or touch status.
func TestPlanProgressiveRolloutImmediateIsNoop(t *testing.T) {
	r := newProgressiveClient(t, mainDepAtTag("t", "v1"))
	bs := progressiveBS("t")
	bs.Status.BuildTag = "v2"

	ramping, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v2")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ramping {
		t.Fatal("a service with no spec.rollout must never ramp")
	}
	if bs.Status.Rollout != nil {
		t.Fatalf("immediate must not write rollout state, got %+v", bs.Status.Rollout)
	}
}

// A progressive service with a previous version starts a ramp at step 0 and pins the
// instance to the tag it is already running.
func TestPlanProgressiveRolloutStartsRamp(t *testing.T) {
	r := newProgressiveClient(t, mainDepAtTag("t", "v1"))
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	bs.Status.BuildTag = "v2"
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ramping, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v2")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !ramping {
		t.Fatal("expected a ramp to start")
	}
	if bs.Status.Rollout == nil || bs.Status.Rollout.Phase != rolloutPhaseProgressing {
		t.Fatalf("expected Progressing, got %+v", bs.Status.Rollout)
	}
	if bs.Status.Rollout.Tag != "v2" || bs.Status.Rollout.Weight != defaultRolloutSteps[0] {
		t.Errorf("expected tag v2 at %d%%, got %+v", defaultRolloutSteps[0], bs.Status.Rollout)
	}
	// The instance must hold still on the version it is already serving.
	if bs.Status.StableTag != "v1" {
		t.Errorf("StableTag = %q, want v1 so the instance stays put during the ramp", bs.Status.StableTag)
	}
}

// The very first deploy has nothing to shift traffic away from — ramping would send
// 5% to the new version and 95% to nothing.
func TestPlanProgressiveRolloutFirstDeployIsImmediate(t *testing.T) {
	r := newProgressiveClient(t) // no Deployment: nothing has ever been deployed
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	bs.Status.BuildTag = "v1"

	ramping, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ramping {
		t.Fatal("the first ever deploy must go straight out, not ramp against nothing")
	}
}

// A human driving spec.canary owns the weight; the automatic ramp stands down rather
// than both bidding for the same backendRef.
func TestPlanProgressiveRolloutCanaryWins(t *testing.T) {
	r := newProgressiveClient(t, mainDepAtTag("t", "v1"))
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	bs.Spec.Canary = &deployv1alpha1.CanarySpec{Enabled: true}
	bs.Status.BuildTag = "v2"

	ramping, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v2")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ramping {
		t.Fatal("an explicitly enabled canary must win over the automatic ramp")
	}
}

// A tag already aborted must not be re-ramped — that would put a version judged bad
// back in front of users on a loop.
func TestPlanProgressiveRolloutDoesNotRetryRolledBackTag(t *testing.T) {
	r := newProgressiveClient(t, mainDepAtTag("t", "v1"))
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	bs.Status.BuildTag = "v2"
	bs.Status.StableTag = "v1"
	bs.Status.Rollout = &deployv1alpha1.RolloutStatus{Phase: rolloutPhaseRolledBack, Tag: "v2"}

	holdsImage, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v2")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// No fresh ramp: re-ramping a version already judged bad would put it back in
	// front of users on a loop.
	if bs.Status.Rollout.Phase != rolloutPhaseRolledBack {
		t.Fatalf("phase = %q, want it to stay RolledBack until a new build arrives", bs.Status.Rollout.Phase)
	}
	if bs.Status.Rollout.Step != 0 {
		t.Error("a rolled-back tag must not resume stepping")
	}
	// But the image stays pinned: staying rolled back means the INSTANCE keeps
	// serving the healthy version, not merely that the ramp stops stepping.
	if !holdsImage {
		t.Fatal("a rolled-back tag must keep the image pinned to the healthy version")
	}
}

func rampingBS(t *testing.T, ns string, weight int32, startedAgo time.Duration) *deployv1alpha1.BirService {
	t.Helper()
	bs := meshBS(nil) // mesh-enabled so the SLO gate is not off
	bs.Name = "app"
	bs.Namespace = ns
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive", Analysis: "enforce"}
	bs.Status.BuildStatus = "Succeeded"
	bs.Status.BuildTag = "v2"
	bs.Status.StableTag = "v1"
	bs.Status.Rollout = &deployv1alpha1.RolloutStatus{
		Phase:         rolloutPhaseProgressing,
		Tag:           "v2",
		Step:          0,
		Weight:        weight,
		StepStartedAt: time.Now().Add(-startedAgo).UTC().Format(time.RFC3339),
	}
	return bs
}

// A step that has not soaked yet holds its weight and decides nothing.
func TestAdvanceRolloutStepSoaksBeforeJudging(t *testing.T) {
	r := newProgressiveClient(t)
	bs := rampingBS(t, "t", 5, 10*time.Second)

	weight, requeue, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if weight != 5 {
		t.Errorf("weight = %d, want it held at 5 during the soak", weight)
	}
	if requeue == 0 {
		t.Error("a soaking step must ask to be looked at again")
	}
	if bs.Status.Rollout.Step != 0 {
		t.Error("a soaking step must not advance")
	}
}

// No signal is NOT a pass. A meshed service whose queries come back empty — too
// little traffic in the window — holds rather than promoting an unwatched version.
// This is the asymmetry that stops a low-traffic service from walking a bad build to
// 100% precisely because nobody hit it.
func TestAdvanceRolloutStepHoldsWhenUnjudgeable(t *testing.T) {
	srv := fakeProm(t, map[string]string{}) // reachable, but every query is empty
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	bs := rampingBS(t, "t", 5, 5*time.Minute)
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	weight, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if weight != 5 {
		t.Errorf("weight = %d, want it held at 5 with no signal", weight)
	}
	if bs.Status.Rollout.Step != 0 {
		t.Fatal("an unjudged step must never advance")
	}
}

// Past maxStepDuration with still no signal the ramp stops and asks for a human —
// but only when a signal was actually expected. See the non-mesh test below for the
// case where none was ever possible.
func TestAdvanceRolloutStepEntersHeldAfterMaxStepDuration(t *testing.T) {
	srv := fakeProm(t, map[string]string{}) // reachable, but every query is empty
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	bs := rampingBS(t, "t", 5, 30*time.Minute)
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if bs.Status.Rollout.Phase != rolloutPhaseHeld {
		t.Fatalf("phase = %q, want Held", bs.Status.Rollout.Phase)
	}
	if bs.Status.Rollout.Message == "" {
		t.Error("a Held rollout must explain itself — a human has to act on it")
	}
}

// A clean step advances to the next weight and restarts the soak clock.
func TestAdvanceRolloutStepAdvancesOnCleanSignal(t *testing.T) {
	srv := fakeProm(t, map[string]string{"increase": "1000", "response_code": "0.0001"})
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	bs := rampingBS(t, "t", defaultRolloutSteps[0], 5*time.Minute)
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	weight, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if weight != defaultRolloutSteps[1] {
		t.Fatalf("weight = %d, want the next step %d", weight, defaultRolloutSteps[1])
	}
	if bs.Status.Rollout.Step != 1 {
		t.Errorf("step = %d, want 1", bs.Status.Rollout.Step)
	}
}

// A breach in enforce mode drops the weight to 0 immediately — the abort is a route
// edit, so the bad version stops receiving traffic without any pod operation.
func TestAdvanceRolloutStepAbortsOnBreach(t *testing.T) {
	srv := fakeProm(t, map[string]string{"increase": "1000", "response_code": "0.05"})
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	bs := rampingBS(t, "t", 5, 5*time.Minute)
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	weight, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if weight != 0 {
		t.Fatalf("weight = %d, want 0 — an abort must cut traffic at once", weight)
	}
	if bs.Status.Rollout.Phase != rolloutPhaseRolledBack {
		t.Fatalf("phase = %q, want RolledBack", bs.Status.Rollout.Phase)
	}
}

// In monitor mode the same breach reports but does not act, so the signal can be
// trusted before it is given the trigger.
func TestAdvanceRolloutStepMonitorModeDoesNotAbort(t *testing.T) {
	srv := fakeProm(t, map[string]string{"increase": "1000", "response_code": "0.05"})
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	bs := rampingBS(t, "t", defaultRolloutSteps[0], 5*time.Minute)
	bs.Spec.Rollout.Analysis = "monitor"
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	weight, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if bs.Status.Rollout.Phase == rolloutPhaseRolledBack {
		t.Fatal("monitor mode must never abort a rollout")
	}
	// And it does not merely refrain from aborting — it ADVANCES through the breach.
	// This is the whole difference between the two modes, and it is the thing to
	// change if the platform ever decides progressive should imply enforcement: in
	// monitor the SLO is observed and reported, it does not gate the weight.
	if weight != defaultRolloutSteps[1] {
		t.Errorf("weight = %d, want %d — monitor reports a breach but still steps up",
			weight, defaultRolloutSteps[1])
	}
}

// A service can gate its ramp on the SLO WITHOUT configuring autoRollback at all:
// rollout.analysis is read first and only falls back to autoRollback's mode when it
// is unset. autoRollback governs the post-rollout gate; this governs the ramp.
func TestRolloutAnalysisEnforcesWithoutAutoRollback(t *testing.T) {
	srv := fakeProm(t, map[string]string{"increase": "1000", "response_code": "0.05"})
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	bs := rampingBS(t, "t", defaultRolloutSteps[0], 5*time.Minute)
	bs.Spec.Traffic = &deployv1alpha1.TrafficSpec{} // mesh on, autoRollback deliberately nil
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive", Analysis: "enforce"}
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if !resolveRollout(bs).enforce {
		t.Fatal("rollout.analysis: enforce must not require autoRollback to be configured")
	}
	weight, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if weight != 0 || bs.Status.Rollout.Phase != rolloutPhaseRolledBack {
		t.Fatalf("expected the breach to abort the ramp; weight=%d phase=%s", weight, bs.Status.Rollout.Phase)
	}
}

// Unset autoRollback resolves to monitor, NOT to off — so the SLO is still measured
// during a ramp. Only a service with no mesh has nothing to measure.
func TestRolloutEvaluatesSLOWithoutAutoRollback(t *testing.T) {
	srv := fakeProm(t, map[string]string{"increase": "1000", "response_code": "0.05"})
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	bs := rampingBS(t, "t", defaultRolloutSteps[0], 5*time.Minute)
	bs.Spec.Traffic = &deployv1alpha1.TrafficSpec{} // no AutoRollback field at all

	if !r.sloAnalysisPossible(bs) {
		t.Fatal("a meshed service must be analyzable without autoRollback being configured")
	}
	v := r.evaluateSLO(context.Background(), bs, resolveAutoRollback(bs), "v2", 3*time.Minute)
	if !v.evaluated {
		t.Fatal("the SLO must be evaluated during a ramp even with no autoRollback config")
	}
	if !v.breached {
		t.Fatalf("5%% errors against a 1%% budget should breach; got %+v", v)
	}
}

// After the last step the ramp hands over to promotion, which is what moves the
// instance itself onto the new tag.
func TestAdvanceRolloutStepPromotesAfterLastStep(t *testing.T) {
	srv := fakeProm(t, map[string]string{"increase": "1000", "response_code": "0.0001"})
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	bs := rampingBS(t, "t", defaultRolloutSteps[len(defaultRolloutSteps)-1], 5*time.Minute)
	bs.Status.Rollout.Step = int32(len(defaultRolloutSteps) - 1)
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if bs.Status.Rollout.Phase != rolloutPhasePromoting {
		t.Fatalf("phase = %q, want Promoting", bs.Status.Rollout.Phase)
	}
}

// Promotion must not tear the temporary pair down until the instance is actually
// serving the new tag — it is still carrying its share of traffic.
func TestMainRolloutCompleteRequiresTheNewTag(t *testing.T) {
	ns := "t"
	s := rollbackScheme(t)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-deploy", Namespace: ns, Generation: 1}}
	dep.Spec.Template.Labels = map[string]string{labelBuildTag: "v1"} // still the old tag
	dep.Status = appsv1.DeploymentStatus{
		ObservedGeneration: 1, Replicas: 4, UpdatedReplicas: 4, AvailableReplicas: 4,
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}
	bs := progressiveBS(ns)

	done, err := r.mainRolloutComplete(context.Background(), bs, "v2")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if done {
		t.Fatal("promotion must not complete while the instance still runs the old tag")
	}

	dep.Spec.Template.Labels[labelBuildTag] = "v2"
	if err := cl.Update(context.Background(), dep); err != nil {
		t.Fatalf("update: %v", err)
	}
	done, err = r.mainRolloutComplete(context.Background(), bs, "v2")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !done {
		t.Fatal("expected promotion to complete once the instance serves the new tag")
	}
}

// Tearing down is called on every reconcile of every non-ramping service, which is
// most of them, so it has to be safe when there is nothing there.
func TestDeleteNextRevisionIsIdempotent(t *testing.T) {
	r := newProgressiveClient(t)
	bs := progressiveBS("t")
	for i := 0; i < 2; i++ {
		if err := r.deleteNextRevision(context.Background(), bs, nextRevisionDepName(bs), nextRevisionSvcName(bs)); err != nil {
			t.Fatalf("delete #%d on absent objects should be a no-op, got %v", i+1, err)
		}
	}
}

// An image-based service (spec.image, no pipeline build) must ramp too. Deriving the
// desired tag from status.BuildTag alone made progressive silently inert for these —
// the knob is accepted, the developer believes they are protected, and every deploy
// still cuts over instantly.
func TestPlanProgressiveRolloutWorksWithoutBuildTag(t *testing.T) {
	r := newProgressiveClient(t, mainDepAtTag("t", "v1"))
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	bs.Status.BuildTag = ""    // never set: no pipeline in this flow
	bs.Status.BuildStatus = "" // ditto
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ramping, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v2")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !ramping {
		t.Fatal("an image-based service must ramp; the tag comes from the resolved image")
	}
	if bs.Status.StableTag != "v1" {
		t.Errorf("StableTag = %q, want v1 read off the running Deployment", bs.Status.StableTag)
	}
}

// A mutable tag cannot be told apart from what is already running, so there is
// nothing to judge one version against the other with.
func TestRampingTagIgnoresLatest(t *testing.T) {
	bs := progressiveBS("t")
	if got := rampingTag(bs, "latest"); got != "" {
		t.Errorf("rampingTag(latest) = %q, want empty", got)
	}
	if got := rampingTag(bs, ""); got != "" {
		t.Errorf("rampingTag(empty) = %q, want empty", got)
	}
}

// Once a promotion has completed the instance runs the ramped tag, and every later
// reconcile passes back through the start-ramp path. It must settle silently rather
// than re-arming, or a promoted version would ramp against itself forever.
func TestPlanProgressiveRolloutSettlesAfterPromotion(t *testing.T) {
	r := newProgressiveClient(t, mainDepAtTag("t", "v2")) // instance now serves v2
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	bs.Status.BuildTag = "v2"
	bs.Status.StableTag = "" // cleared by promotion
	bs.Status.Rollout = nil  // cleared when the temporary pair was removed

	ramping, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v2")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ramping {
		t.Fatal("a tag the instance already serves must not start a new ramp")
	}
	if bs.Status.Rollout != nil {
		t.Fatalf("expected no rollout state after settling, got %+v", bs.Status.Rollout)
	}
	if bs.Status.StableTag != "" {
		t.Errorf("StableTag = %q, want it to stay clear", bs.Status.StableTag)
	}
}

// A service that can NEVER be judged must not stall. Holding forever would mean
// every deploy of a non-mesh service parks at the first step until a human rescues
// it — a safety feature turned into an outage. The ramp still paces the rollout, it
// just cannot catch a version that starts and then misbehaves.
func TestAdvanceRolloutStepRampsOnTimeWhenAnalysisImpossible(t *testing.T) {
	r := newProgressiveClient(t)
	r.PromURL = "" // operator has no Prometheus at all

	bs := rampingBS(t, "t", defaultRolloutSteps[0], 5*time.Minute)
	bs.Spec.Traffic = nil // no mesh: there are no request metrics for this service
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	weight, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if bs.Status.Rollout.Phase == rolloutPhaseHeld {
		t.Fatal("a service with no possible SLO signal must ramp on time, not stall at Held")
	}
	if weight != defaultRolloutSteps[1] {
		t.Errorf("weight = %d, want the soak to advance it to %d", weight, defaultRolloutSteps[1])
	}
}

// The distinction the fix turns on: "no signal is possible" vs "a signal was
// expected and did not arrive".
func TestSLOAnalysisPossible(t *testing.T) {
	meshed := meshBS(nil)
	plain := &deployv1alpha1.BirService{}

	cases := []struct {
		name    string
		promURL string
		bs      *deployv1alpha1.BirService
		want    bool
	}{
		{"mesh + prometheus", "http://prom", meshed, true},
		{"mesh, no prometheus", "", meshed, false},
		{"prometheus, no mesh", "http://prom", plain, false},
		{"neither", "", plain, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &BirServiceReconciler{PromURL: c.promURL}
			if got := r.sloAnalysisPossible(c.bs); got != c.want {
				t.Errorf("sloAnalysisPossible = %v, want %v", got, c.want)
			}
		})
	}
}

// An aborted ramp must keep the instance on its known-good version. Regression test
// for a bug that undid the whole feature: aborting removed the temporary Deployment
// and dropped its weight to 0, then released the image pin — so the main Deployment
// rolled out to the very tag the ramp had just condemned at 5%, handing 100% of
// users the version that had already failed.
func TestAbortedRampKeepsInstanceOnHealthyVersion(t *testing.T) {
	srv := fakeProm(t, map[string]string{"increase": "1000", "response_code": "0.05"})
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t, mainDepAtTag("t", "v1"))
	r.PromURL = srv.URL

	bs := rampingBS(t, "t", defaultRolloutSteps[0], 5*time.Minute)
	bs.Spec.Traffic.AutoRollback = &deployv1alpha1.AutoRollbackSpec{Mode: "enforce"}
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive", Analysis: "enforce"}
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The breach aborts the ramp.
	weight, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if weight != 0 || bs.Status.Rollout.Phase != rolloutPhaseRolledBack {
		t.Fatalf("expected an abort; weight=%d phase=%s", weight, bs.Status.Rollout.Phase)
	}

	// The next reconcile must still hold the instance on v1.
	holdsImage, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v2")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !holdsImage {
		t.Fatal("an aborted ramp must keep the image pinned, or the instance deploys the condemned tag")
	}
	if bs.Status.StableTag != "v1" {
		t.Fatalf("StableTag = %q, want v1 to stay pinned after an abort", bs.Status.StableTag)
	}

	// The temporary pair is gone and the route is back to a single backend.
	svc, wt, _, err := r.reconcileNextRevision(context.Background(), progressiveReq(bs), bs,
		8080, 8080, corev1.ResourceRequirements{}, nil, nil, 4)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if svc != "" || wt != 0 {
		t.Errorf("expected the temporary pair torn down, got svc=%q weight=%d", svc, wt)
	}

	// And the crash-loop gate comes back: the ramp no longer owns the decision once
	// there is no parallel version left to judge.
	if progressiveOwnsRollbackDecision(bs) {
		t.Error("after an abort the ramp must hand the rollback decision back to autoRollback")
	}
}

// A fix arriving after an abort starts a fresh ramp, against the healthy version.
func TestNewTagAfterAbortStartsFreshRamp(t *testing.T) {
	r := newProgressiveClient(t, mainDepAtTag("t", "v1"))
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	bs.Status.StableTag = "v1"
	bs.Status.Rollout = &deployv1alpha1.RolloutStatus{Phase: rolloutPhaseRolledBack, Tag: "v2"}
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	holdsImage, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v3")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !holdsImage {
		t.Fatal("expected the fix to start a ramp")
	}
	if bs.Status.Rollout.Phase != rolloutPhaseProgressing || bs.Status.Rollout.Tag != "v3" {
		t.Fatalf("expected a fresh ramp for v3, got %+v", bs.Status.Rollout)
	}
	if bs.Status.Rollout.Step != 0 || bs.Status.Rollout.Weight != defaultRolloutSteps[0] {
		t.Errorf("a fresh ramp must start at step 0, got %+v", bs.Status.Rollout)
	}
	if bs.Status.StableTag != "v1" {
		t.Errorf("StableTag = %q, want the ramp to still run against the healthy v1", bs.Status.StableTag)
	}
}

// Only the pool's primary ramps. A non-primary member owns no HTTPRoute, so a ramp
// there would wait for traffic nothing can route to it, find no signal, hold — and
// holding pins the image, so that member's deploys would stop. Catalogs hand the same
// config to every member via a YAML anchor, so one `rollout:` block would otherwise do
// this to the whole pool at once.
func TestProgressiveIsPrimaryOnly(t *testing.T) {
	rollout := &deployv1alpha1.RolloutSpec{Strategy: "progressive"}

	primary := progressiveBS("t")
	primary.Spec.Rollout = rollout
	primary.Spec.Route = &deployv1alpha1.RouteSpec{Group: "hello", Primary: true, Weighted: true}
	if !resolveRollout(primary).progressive {
		t.Error("the pool's primary must ramp")
	}

	member := progressiveBS("t")
	member.Spec.Rollout = rollout
	member.Spec.Route = &deployv1alpha1.RouteSpec{Group: "hello", Primary: false, Weighted: true}
	if resolveRollout(member).progressive {
		t.Error("a non-primary pool member must not ramp — it owns no route to shift traffic with")
	}

	// A standalone service has no spec.route and is always its own primary.
	standalone := progressiveBS("t")
	standalone.Spec.Rollout = rollout
	if !resolveRollout(standalone).progressive {
		t.Error("a standalone service must still ramp")
	}
}

// A non-primary member must not even build a next-revision Deployment.
func TestNonPrimaryMemberBuildsNoNextRevision(t *testing.T) {
	r := newProgressiveClient(t, mainDepAtTag("t", "v1"))
	bs := progressiveBS("t")
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	bs.Spec.Route = &deployv1alpha1.RouteSpec{Group: "hello", Primary: false, Weighted: true}
	bs.Status.BuildTag = "v2"

	holdsImage, err := r.planProgressiveRollout(context.Background(), progressiveReq(bs), bs, "v2")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if holdsImage {
		t.Fatal("a non-primary member must deploy normally, not hold its image")
	}
	if bs.Status.Rollout != nil {
		t.Fatalf("a non-primary member must write no ramp state, got %+v", bs.Status.Rollout)
	}
}

// Steps above 50% are accepted but flagged: past that point the next revision cannot
// be given enough pods to match the instance's per-pod load, so it looks slow for
// reasons unrelated to its code and a latency objective aborts a healthy version.
func TestStepsAboveHalfAreFlagged(t *testing.T) {
	bs := meshBS(nil)
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{
		Strategy: "progressive",
		Steps:    []int32{5, 10, 20, 40, 50, 75, 90},
	}
	cfg := resolveRollout(bs)

	if len(cfg.steps) != 7 {
		t.Fatalf("the step list is valid and must be honoured, got %v", cfg.steps)
	}
	if got, want := cfg.underProvisionedSteps, []int32{75, 90}; len(got) != len(want) || got[0] != 75 || got[1] != 90 {
		t.Errorf("underProvisionedSteps = %v, want %v", got, want)
	}

	// The default stops exactly at the balanced limit.
	plain := meshBS(nil)
	plain.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	if len(resolveRollout(plain).underProvisionedSteps) != 0 {
		t.Error("the default steps must all be sizeable")
	}
}

// maxBalancedStep is arithmetic, not taste: matching per-pod load needs
// stable*w/(100-w) pods, which exceeds the instance's own count exactly above 50%,
// for every replica count.
func TestReplicaBalanceHoldsToFiftyPercentThenBreaks(t *testing.T) {
	for _, stable := range []int32{1, 3, 4, 10} {
		for _, w := range []int32{5, 10, 20, 40, 50} {
			n := nextRevisionReplicas(stable, w)
			ideal := float64(stable) * float64(w) / float64(100-w)
			// Sized at or above the ideal (rounded up), so the new revision is never
			// under-provisioned at or below the balanced limit.
			if float64(n) < ideal {
				t.Errorf("stable=%d w=%d: got %d pods, need at least %.2f to match per-pod load", stable, w, n, ideal)
			}
		}
		// Above the limit the cap binds and the balance is knowingly lost.
		if n := nextRevisionReplicas(stable, 90); n != stable {
			t.Errorf("stable=%d w=90: got %d, want it capped at %d", stable, n, stable)
		}
	}
}

// A version can ramp cleanly and still fail to start on the instance — different node
// pool, different limits, a PDB that will not release the old pods. The crash-loop
// gate must stay armed during promotion, or nothing is watching that rollout: the ramp
// waits forever on a promotion that never lands while the instance's share of traffic
// serves nothing.
func TestCrashGateStaysArmedDuringPromotion(t *testing.T) {
	bs := progressiveBS("t")

	for _, phase := range []string{rolloutPhaseProgressing, rolloutPhaseHeld} {
		bs.Status.Rollout = &deployv1alpha1.RolloutStatus{Phase: phase, Tag: "v2"}
		if !progressiveOwnsRollbackDecision(bs) {
			t.Errorf("phase %s: the ramp is judging this version, the crash gate should stand down", phase)
		}
	}
	// Promoting: the ramp is only waiting for the instance's rollout and cannot see
	// whether its pods come up at all.
	for _, phase := range []string{rolloutPhasePromoting, rolloutPhaseRolledBack} {
		bs.Status.Rollout = &deployv1alpha1.RolloutStatus{Phase: phase, Tag: "v2"}
		if progressiveOwnsRollbackDecision(bs) {
			t.Errorf("phase %s: the crash gate must stay armed", phase)
		}
	}
}

// When the crash gate condemns the tag mid-promotion, the ramp must notice and stop
// waiting: tear the temporary revision down and record the failure, rather than
// holding it at its last weight forever against a rollout that was abandoned.
func TestPromotionAbandonedWhenTagQuarantined(t *testing.T) {
	ns := "t"
	// The instance was reverted: the gate recorded the condemned tag and swapped the
	// image back to the healthy one.
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "app-deploy", Namespace: ns, Generation: 2,
		Annotations: map[string]string{annotRolledBackTag: "v2", annotHealthyTag: "v1"},
	}}
	dep.Spec.Template.Labels = map[string]string{labelBuildTag: "v1"}
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3}

	nextDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-next", Namespace: ns}}
	nextDep.Status = appsv1.DeploymentStatus{ReadyReplicas: 1}

	r := newProgressiveClient(t, dep, nextDep)
	bs := rampingBS(t, ns, 50, 5*time.Minute)
	bs.Status.Rollout.Phase = rolloutPhasePromoting
	bs.Status.StableTag = ""
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc, weight, _, err := r.reconcileNextRevision(context.Background(), progressiveReq(bs), bs,
		8080, 8080, corev1.ResourceRequirements{}, nil, nil, 3)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if svc != "" || weight != 0 {
		t.Fatalf("the temporary revision must stop taking traffic; got svc=%q weight=%d", svc, weight)
	}
	if bs.Status.Rollout.Phase != rolloutPhaseRolledBack {
		t.Fatalf("phase = %q, want RolledBack once the promotion was abandoned", bs.Status.Rollout.Phase)
	}
	if bs.Status.Rollout.Message == "" {
		t.Error("an abandoned promotion must explain itself")
	}
}

// A healthy promotion still completes normally — the quarantine check must not fire
// on a tag that was never condemned.
func TestPromotionCompletesWhenInstanceIsHealthy(t *testing.T) {
	ns := "t"
	dep := mainDepAtTag(ns, "v2") // instance now serves the ramped tag, all replicas up
	nextDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-next", Namespace: ns}}
	nextDep.Status = appsv1.DeploymentStatus{ReadyReplicas: 1}

	r := newProgressiveClient(t, dep, nextDep)
	bs := rampingBS(t, ns, 50, 5*time.Minute)
	bs.Status.Rollout.Phase = rolloutPhasePromoting
	bs.Status.StableTag = ""
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc, weight, _, err := r.reconcileNextRevision(context.Background(), progressiveReq(bs), bs,
		8080, 8080, corev1.ResourceRequirements{}, nil, nil, 3)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if svc != "" || weight != 0 {
		t.Errorf("a completed promotion tears the temporary revision down; got svc=%q weight=%d", svc, weight)
	}
	if bs.Status.Rollout != nil {
		t.Fatalf("a completed promotion clears the ramp state, got %+v", bs.Status.Rollout)
	}
}

// The end of the chain: once a ramped tag is actually running on the instance, the
// crash-loop gate adopts it as the rollback target.
//
// The ramp deliberately does not write healthy-tag itself. It validated the version on
// the TEMPORARY Deployment's pods; the instance's pods are new, created by the
// promotion rollout, so "the ramp passed" is not yet evidence that the version is
// healthy where it now runs. Until the normal observation period elapses the healthy
// tag stays on the previous version — which is what guarantees there is always a
// rollback target if the promotion turns out badly.
func TestPromotedTagBecomesHealthyTag(t *testing.T) {
	ns := "t"
	// The instance is fully rolled out on the ramped tag.
	dep := mainDepAtTag(ns, "v2")
	dep.Annotations = map[string]string{annotHealthyTag: "v1"} // still the pre-ramp version
	// Its pods are older than the observation period (grace + SLO window), ready and
	// never restarted.
	pod := tagPod(ns, "app", "v2", "app-v2-abc", true, false)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-30 * time.Minute))

	s := rollbackScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&deployv1alpha1.BirService{}).
		WithObjects(dep, pod).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}

	bs := meshBS(nil)
	bs.Name = "app"
	bs.Namespace = ns
	bs.Spec.Rollout = &deployv1alpha1.RolloutSpec{Strategy: "progressive"}
	bs.Status.Rollout = nil // promotion completed and cleared the ramp state

	// With the ramp finished, the crash-loop gate owns the decision again.
	if progressiveOwnsRollbackDecision(bs) {
		t.Fatal("after promotion the ramp must hand the decision back")
	}

	if _, err := r.evaluateAutoRollback(context.Background(), bs, "app-deploy", "v2", "v2", "v1", ""); err != nil {
		t.Fatalf("evaluateAutoRollback: %v", err)
	}

	var got appsv1.Deployment
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "app-deploy", Namespace: ns}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Annotations[annotHealthyTag] != "v2" {
		t.Fatalf("healthy-tag = %q, want v2 once the promoted version has proven itself on the instance",
			got.Annotations[annotHealthyTag])
	}
}

// The same version, still inside its observation window, must NOT yet be adopted —
// otherwise a version that crashes two minutes after promotion would have already
// overwritten the only tag worth rolling back to.
func TestPromotedTagIsNotHealthyTagBeforeObservation(t *testing.T) {
	ns := "t"
	dep := mainDepAtTag(ns, "v2")
	dep.Annotations = map[string]string{annotHealthyTag: "v1"}
	pod := tagPod(ns, "app", "v2", "app-v2-abc", true, false)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-5 * time.Second)) // just promoted

	s := rollbackScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&deployv1alpha1.BirService{}).
		WithObjects(dep, pod).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s, PromURL: "http://prom"}

	bs := meshBS(nil)
	bs.Name = "app"
	bs.Namespace = ns

	if _, err := r.evaluateAutoRollback(context.Background(), bs, "app-deploy", "v2", "v2", "v1", ""); err != nil {
		t.Fatalf("evaluateAutoRollback: %v", err)
	}

	var got appsv1.Deployment
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "app-deploy", Namespace: ns}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Annotations[annotHealthyTag] != "v1" {
		t.Fatalf("healthy-tag = %q, want it held at v1 until the new version clears its observation window",
			got.Annotations[annotHealthyTag])
	}
}

// routeScheme registers the Gateway API HTTPRoute as unstructured so a fake client can
// hold the real object the operator writes.
func routeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := rollbackScheme(t)
	s.AddKnownTypeWithName(httpRouteGVK, &unstructured.Unstructured{})
	listGVK := httpRouteGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

// routeBackendWeights reads back what the operator actually wrote to the HTTPRoute.
func routeBackendWeights(t *testing.T, cl client.Client, ns, name string) []string {
	t.Helper()
	var got unstructured.Unstructured
	got.SetGroupVersionKind(httpRouteGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &got); err != nil {
		t.Fatalf("get route: %v", err)
	}
	rules, found, err := unstructured.NestedSlice(got.Object, "spec", "rules")
	if err != nil || !found || len(rules) == 0 {
		t.Fatalf("no rules on the route: found=%v err=%v", found, err)
	}
	refs, _, _ := unstructured.NestedSlice(rules[0].(map[string]interface{}), "backendRefs")
	out := make([]string, 0, len(refs))
	for _, b := range refs {
		m := b.(map[string]interface{})
		if w, ok := m["weight"].(int64); ok {
			out = append(out, fmt.Sprintf("%s=%d", m["name"], w))
		} else {
			out = append(out, fmt.Sprintf("%s=unweighted", m["name"]))
		}
	}
	return out
}

// Each step must actually reach the HTTPRoute — the weight in status is only a plan,
// the backendRefs are what move traffic. This exercises the real object rather than
// the rule builder, so a break anywhere in the write path is caught.
//
// The return to a single unweighted backend at the end matters as much as the ramp:
// leaving a zero-weight backendRef behind would point the route at the Service that
// teardown just deleted.
func TestRouteWeightFollowsEachStep(t *testing.T) {
	s := routeScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s, BaseDomain: "example.com"}

	bs := progressiveBS("t")
	routeName := "app-default-route"

	cases := []struct {
		weight int32
		want   []string
	}{
		{0, []string{"app-svc=unweighted"}},
		{5, []string{"app-svc=95", "app-next-svc=5"}},
		{25, []string{"app-svc=75", "app-next-svc=25"}},
		{50, []string{"app-svc=50", "app-next-svc=50"}},
		// Abort or promotion: the temporary Service is gone, so it must leave the route.
		{0, []string{"app-svc=unweighted"}},
	}

	for _, c := range cases {
		split := routeSplit{}
		if c.weight > 0 {
			split = routeSplit{nextSvc: "app-next-svc", nextWeight: c.weight}
		}
		if err := r.reconcileHTTPRoute(context.Background(), bs, "app-svc", 8080, split, nil); err != nil {
			t.Fatalf("weight %d: %v", c.weight, err)
		}
		if got := routeBackendWeights(t, cl, "t", routeName); !reflect.DeepEqual(got, c.want) {
			t.Errorf("weight %d: backendRefs = %v, want %v", c.weight, got, c.want)
		}
	}
}

// The same, on a pool: the member's share subdivides while its sibling's is untouched.
func TestRouteWeightFollowsEachStepInPool(t *testing.T) {
	s := routeScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s, BaseDomain: "example.com"}

	bs := progressiveBS("t")
	bs.Spec.Route = &deployv1alpha1.RouteSpec{Group: "hello", Primary: true, Weighted: true}
	backends := []deployv1alpha1.RouteBackend{
		{Name: "app", Weight: 95},
		{Name: "testing", Weight: 5},
	}

	cases := []struct {
		weight int32
		want   []string
	}{
		{0, []string{"app-inst-svc=95", "testing-inst-svc=5"}},
		{5, []string{"app-inst-svc=9025", "app-next-svc=475", "testing-inst-svc=500"}},
		{50, []string{"app-inst-svc=4750", "app-next-svc=4750", "testing-inst-svc=500"}},
		{0, []string{"app-inst-svc=95", "testing-inst-svc=5"}},
	}

	for _, c := range cases {
		split := routeSplit{}
		if c.weight > 0 {
			split = routeSplit{nextSvc: "app-next-svc", nextWeight: c.weight}
		}
		if err := r.reconcileHTTPRoute(context.Background(), bs, "app-inst-svc", 8080, split, backends); err != nil {
			t.Fatalf("weight %d: %v", c.weight, err)
		}
		if got := routeBackendWeights(t, cl, "t", "app-default-route"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("weight %d: backendRefs = %v, want %v", c.weight, got, c.want)
		}
	}
}

// Below minRequests the gate returns no verdict, and no verdict is NOT a pass. The
// ramp must freeze rather than treat silence as approval — otherwise a low-traffic
// service walks an unwatched version all the way to promotion precisely because
// nobody was hitting it hard enough to catch the problem.
func TestBelowMinRequestsNeverCountsAsSuccess(t *testing.T) {
	// 20 requests against the default minimum of 50 — and 90% of them failing. Even a
	// catastrophic error rate must not register, because there is not enough evidence
	// to condemn OR to clear the version.
	srv := fakeProm(t, map[string]string{"increase": "20", "response_code": "0.90"})
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	bs := rampingBS(t, "t", defaultRolloutSteps[0], 5*time.Minute)
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	v := r.evaluateSLO(context.Background(), bs, resolveAutoRollback(bs), "v2", 5*time.Minute)
	if v.evaluated {
		t.Fatal("below minRequests the gate must not form an opinion at all")
	}

	weight, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if weight != defaultRolloutSteps[0] || bs.Status.Rollout.Step != 0 {
		t.Fatalf("the ramp must freeze on no evidence; weight=%d step=%d", weight, bs.Status.Rollout.Step)
	}
	if bs.Status.Rollout.Phase == rolloutPhasePromoting {
		t.Fatal("no evidence must never reach promotion")
	}
}

// A Held ramp is frozen, not finished: it must never promote itself, however long it
// waits. Only a human — or traffic finally arriving — moves it.
func TestHeldRampNeverPromotesItself(t *testing.T) {
	srv := fakeProm(t, map[string]string{}) // reachable, every query empty
	t.Cleanup(srv.Close)

	r := newProgressiveClient(t)
	r.PromURL = srv.URL
	// Already on the last step, and long past maxStepDuration — the most tempting
	// possible moment to "just finish".
	bs := rampingBS(t, "t", defaultRolloutSteps[len(defaultRolloutSteps)-1], 60*time.Minute)
	bs.Status.Rollout.Step = int32(len(defaultRolloutSteps) - 1)
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs); err != nil {
			t.Fatalf("advance #%d: %v", i, err)
		}
		if bs.Status.Rollout.Phase == rolloutPhasePromoting {
			t.Fatalf("reconcile #%d: a Held ramp promoted itself on no evidence", i+1)
		}
	}
	if bs.Status.Rollout.Phase != rolloutPhaseHeld {
		t.Fatalf("phase = %q, want Held", bs.Status.Rollout.Phase)
	}
}

// The freeze is not a dead end. When traffic finally arrives the verdict becomes
// possible again and the ramp resumes on its own — a service that was merely quiet
// during its soak should not need a human to restart its deploy.
func TestHeldRampResumesWhenTrafficArrives(t *testing.T) {
	r := newProgressiveClient(t)

	// First: quiet, so the ramp holds.
	quiet := fakeProm(t, map[string]string{})
	r.PromURL = quiet.URL
	bs := rampingBS(t, "t", defaultRolloutSteps[0], 30*time.Minute)
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if bs.Status.Rollout.Phase != rolloutPhaseHeld {
		t.Fatalf("expected the ramp to hold first, got %s", bs.Status.Rollout.Phase)
	}
	quiet.Close()

	// Then: traffic arrives and the version looks clean.
	busy := fakeProm(t, map[string]string{"increase": "1000", "response_code": "0.0001"})
	t.Cleanup(busy.Close)
	r.PromURL = busy.URL

	weight, _, err := r.advanceRolloutStep(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if bs.Status.Rollout.Phase != rolloutPhaseProgressing {
		t.Fatalf("phase = %q, want the ramp to resume once it can be judged", bs.Status.Rollout.Phase)
	}
	if weight != defaultRolloutSteps[1] {
		t.Errorf("weight = %d, want it to advance to %d", weight, defaultRolloutSteps[1])
	}
}

// A Held ramp is waiting on a judgement no timer can make, so a human needs a lever.
// Promote skips the remaining steps and moves the instance onto the ramped tag.
func TestManualPromoteReleasesAHeldRamp(t *testing.T) {
	r := newProgressiveClient(t)
	bs := rampingBS(t, "t", defaultRolloutSteps[0], 30*time.Minute)
	bs.Status.Rollout.Phase = rolloutPhaseHeld
	bs.Status.Rollout.Message = "no SLO signal"
	bs.Annotations = map[string]string{annotRolloutAction: rolloutActionPromote}
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	changed, err := r.consumeRolloutAction(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !changed {
		t.Fatal("promote must move the ramp out of Held")
	}
	if bs.Status.Rollout.Phase != rolloutPhasePromoting {
		t.Fatalf("phase = %q, want Promoting", bs.Status.Rollout.Phase)
	}
	if bs.Status.Rollout.Message == "" {
		t.Error("a hand-promoted rollout must record that it was not judged automatically")
	}
}

// Abort is the other half of the lever: give up on the version, put traffic back.
func TestManualAbortStopsARamp(t *testing.T) {
	r := newProgressiveClient(t)
	bs := rampingBS(t, "t", 25, 1*time.Minute) // mid-ramp, still soaking
	bs.Annotations = map[string]string{annotRolloutAction: rolloutActionAbort}
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := r.consumeRolloutAction(context.Background(), progressiveReq(bs), bs); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if bs.Status.Rollout.Phase != rolloutPhaseRolledBack {
		t.Fatalf("phase = %q, want RolledBack", bs.Status.Rollout.Phase)
	}
	if bs.Status.Rollout.Weight != 0 {
		t.Errorf("weight = %d, want traffic cut to 0", bs.Status.Rollout.Weight)
	}
}

// The annotation is a command, not desired state: acting on it twice would let one
// "promote" silently release the next ten deploys of that service.
func TestRolloutActionIsConsumedOnce(t *testing.T) {
	r := newProgressiveClient(t)
	bs := rampingBS(t, "t", defaultRolloutSteps[0], 30*time.Minute)
	bs.Status.Rollout.Phase = rolloutPhaseHeld
	bs.Annotations = map[string]string{annotRolloutAction: rolloutActionPromote}
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := r.consumeRolloutAction(context.Background(), progressiveReq(bs), bs); err != nil {
		t.Fatalf("consume: %v", err)
	}
	// Gone from the live object, so a GitOps sync has nothing to fight over.
	var latest deployv1alpha1.BirService
	if err := r.Get(context.Background(), progressiveReq(bs).NamespacedName, &latest); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, still := latest.Annotations[annotRolloutAction]; still {
		t.Fatal("the action annotation must be removed once applied")
	}

	// A second pass is a no-op.
	bs.Status.Rollout.Phase = rolloutPhaseProgressing
	changed, err := r.consumeRolloutAction(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("consume #2: %v", err)
	}
	if changed || bs.Status.Rollout.Phase != rolloutPhaseProgressing {
		t.Fatal("a consumed action must not apply again")
	}
}

// A typo must not be read as either decision.
func TestUnknownRolloutActionDoesNothing(t *testing.T) {
	r := newProgressiveClient(t)
	bs := rampingBS(t, "t", defaultRolloutSteps[0], 1*time.Minute)
	bs.Annotations = map[string]string{annotRolloutAction: "prmote"}
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	changed, err := r.consumeRolloutAction(context.Background(), progressiveReq(bs), bs)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if changed || bs.Status.Rollout.Phase != rolloutPhaseProgressing {
		t.Fatalf("an unrecognised action must leave the ramp alone, got %s", bs.Status.Rollout.Phase)
	}
}

// An abort must cut traffic on the SAME reconcile, not after one more soak — which is
// why the action is consumed before the phase dispatch in reconcileNextRevision.
func TestManualAbortCutsTrafficImmediately(t *testing.T) {
	ns := "t"
	nextDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-next", Namespace: ns}}
	nextDep.Status = appsv1.DeploymentStatus{ReadyReplicas: 1}

	r := newProgressiveClient(t, mainDepAtTag(ns, "v1"), nextDep)
	bs := rampingBS(t, ns, 25, 1*time.Minute)
	bs.Annotations = map[string]string{annotRolloutAction: rolloutActionAbort}
	if err := r.Create(context.Background(), bs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc, weight, _, err := r.reconcileNextRevision(context.Background(), progressiveReq(bs), bs,
		8080, 8080, corev1.ResourceRequirements{}, nil, nil, 3)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if svc != "" || weight != 0 {
		t.Fatalf("abort must take traffic off on the same pass; got svc=%q weight=%d", svc, weight)
	}
}
