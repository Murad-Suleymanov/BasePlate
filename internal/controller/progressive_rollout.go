package controller

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

// Progressive rollout: shifting a new image onto an instance in weight steps.
//
// This is the NORMAL deploy path, not a feature a developer turns on per release.
// A service sets spec.rollout.strategy: progressive once, and from then on every
// image its pipeline pushes ramps instead of cutting over. With no spec.rollout at
// all — the default — none of this runs and nothing about the deploy changes.
//
// How it differs from spec.canary, which stays exactly as it was: canary is a
// manual, developer-driven experiment (you pick a weight, you promote by hand, the
// canary is a thing you see). This is an automatic mechanism the developer never
// interacts with. The temporary Deployment it uses is an implementation detail that
// exists for minutes and is deleted on the way out; what the developer deploys, and
// what they end up running, is the instance itself.
//
// Why a second Deployment at all. Traffic weight is attached to a backendRef, and a
// backendRef names a Service, so splitting traffic between two versions needs both
// versions to have pods at the same time. A single Deployment cannot hold that
// state: with maxUnavailable:0 its controller retires old pods the moment new ones
// go Ready, and there is no supported way to park a Deployment at a percentage
// (StatefulSet has .partition; Deployment has nothing). Pausing is the closest
// thing, and it freezes the rollout wherever it happened to land rather than where
// we want it — a race the operator loses more often than it wins, and losing it
// means a weighted backendRef pointing at zero endpoints, which is a 503, not a
// slower rollout.
//
// What that buys, and it is the whole point: the main Deployment is not touched for
// the entire ramp. Aborting is one route edit — weight to 0 — so a bad version stops
// receiving traffic in the time it takes Envoy to get the new config, with no pod
// churn, no capacity dip, and nothing to roll back. Compare the same abort on a
// single-Deployment rollout, where "stop the bleeding" means starting another
// rolling update and waiting for pods to schedule and warm.

const (
	rolloutStrategyImmediate   = "immediate"
	rolloutStrategyProgressive = "progressive"

	// Ramp phases. See RolloutStatus for what each means.
	rolloutPhaseProgressing = "Progressing"
	rolloutPhasePromoting   = "Promoting"
	rolloutPhaseHeld        = "Held"
	rolloutPhaseRolledBack  = "RolledBack"

	// minRolloutStepDuration floors the soak when the SLO window is very short.
	minRolloutStepDuration = 2 * time.Minute

	// defaultRolloutMaxStepDuration bounds a step that cannot be judged at all. See
	// the fail-closed note on evaluateRolloutStep for why this ends in a hold rather
	// than in a promotion.
	defaultRolloutMaxStepDuration = 10 * time.Minute

	// requeueRolloutStep is how often a ramp in flight is re-examined. A step is a
	// wall-clock soak, so this only needs to be fine enough that a step ends promptly
	// after its duration elapses, not fine enough to catch the instant it does.
	requeueRolloutStep = 15 * time.Second

	// annotRolloutAction is how a human overrides a ramp:
	//
	//   kubectl annotate birservice <name> deploy.easydeploy.io/rollout-action=promote
	//   kubectl annotate birservice <name> deploy.easydeploy.io/rollout-action=abort
	//
	// This is a COMMAND, not desired state, so the operator consumes it — it acts once
	// and removes the annotation. That distinction matters twice over. It makes the
	// action idempotent (a leftover "promote" cannot silently promote the next ten
	// deploys), and it survives GitOps: ArgoCD reverts drift it did not author, so an
	// annotation that lingered would be fought over, whereas one that deletes itself is
	// simply gone before the next sync.
	//
	// It exists because a ramp can legitimately reach a state no timer resolves. Held
	// means "no evidence, and waiting longer will not produce any" — a genuinely quiet
	// service, or a monitoring outage during the soak. Deciding that is a judgement
	// call about acceptable risk, which is a human's to make; without a lever the only
	// way out was editing the strategy in Git, which also disables progressive rollout
	// for that service entirely.
	annotRolloutAction = "deploy.easydeploy.io/rollout-action"

	rolloutActionPromote = "promote"
	rolloutActionAbort   = "abort"
)

// defaultRolloutSteps is the ramp when none is declared. It starts small enough that
// the first step answers "does this version serve at all" with very few users
// exposed, then roughly quadruples: the interesting failures show up immediately,
// and there is no value in spending a soak period on 6% once 5% was clean.
//
// It deliberately stops short of 100. The last step hands over to promotion, which
// moves the main Deployment — sized and autoscaled for the whole service — onto the
// new tag. Routing 100% at the temporary Deployment instead would put the entire
// service on replicas provisioned for a fraction of it.
var defaultRolloutSteps = []int32{5, 25, 50}

// nextRevisionDepName / nextRevisionSvcName name the temporary pair. Fixed names,
// not tag-derived: build tags are 40-char SHAs and would blow the 63-char limit, and
// a stable name means an interrupted ramp is adopted on restart instead of leaking a
// second copy.
func nextRevisionDepName(bs *deployv1alpha1.BirService) string { return bs.Name + "-next" }
func nextRevisionSvcName(bs *deployv1alpha1.BirService) string { return bs.Name + "-next-svc" }

// resolvedRollout is spec.rollout merged with platform defaults.
type resolvedRollout struct {
	progressive bool
	steps       []int32
	stepDur     time.Duration
	maxStepDur  time.Duration
	enforce     bool
	// sloWindow is the trailing window the SLI is computed over, from
	// spec.traffic.autoRollback.window. It is NOT stepDuration, and confusing the two
	// is the easy mistake: stepDuration is how long a step serves, sloWindow is how
	// far back the query looks when the step is judged.
	sloWindow time.Duration
	// dilutedWindow is set when stepDur does not exceed sloWindow, so the query that
	// judges a step still reaches back into the previous one. Surfaced when a ramp
	// starts rather than silently weakening every verdict.
	dilutedWindow bool
	// underProvisionedSteps are the declared steps above maxBalancedStep, where the
	// new revision cannot be given enough pods to carry its share at the same per-pod
	// load as the instance. See maxBalancedStep.
	underProvisionedSteps []int32
}

// maxBalancedStep is the highest weight at which the next revision can still be sized
// to match the instance's per-pod load.
//
// It is 50 and that is arithmetic, not a policy: matching per-pod load needs
// stable*w/(100-w) pods, which exceeds the instance's own replica count exactly when
// w > 50 — whatever that count is. Past it the new revision is under-provisioned for
// its share and looks slow for reasons that have nothing to do with its code, so a
// latency objective condemns it. The error is in the safe direction (a false abort,
// never a false promotion), but it makes high steps unusable rather than merely
// imprecise.
//
// The architecture cannot fix this by scaling the instance down to compensate: leaving
// the instance untouched is what makes an abort a route edit instead of a rollout, and
// scaling it back up on abort would reintroduce exactly the pod-startup delay the
// design exists to avoid. Past 50% the answer is to promote, not to keep ramping —
// which is why the default steps stop there.
const maxBalancedStep = 50

// stepDurationFor derives the default soak from the SLO window.
//
// It cannot be a constant. The metrics are a TRAILING window that does not reset
// when the weight changes, so if a step is judged after less time than the window
// covers, the query still contains the previous step's traffic — and the previous
// step was, by definition, the one that passed. A bad version at 25% gets its error
// ratio averaged down by the clean 5% period that preceded it, which is exactly
// backwards: the signal is weakest at the moment it matters most.
//
// Twice the window: the first half lets the window fill with only this step's
// traffic, the second half is the measurement. Anything derived from the window
// stays correct when an operator changes that window; a hardcoded 2m did not, and
// silently equalled the default window.
func stepDurationFor(sloWindow time.Duration) time.Duration {
	d := sloWindow * 2
	if d < minRolloutStepDuration {
		return minRolloutStepDuration
	}
	return d
}

// resolveRollout merges spec.rollout with the defaults.
//
// Invalid input never silently becomes something dangerous. A malformed duration or
// an unusable step list falls back to the default rather than to zero — a zero soak
// would promote a version through every step without ever measuring it, which is a
// worse outcome than the immediate strategy the developer was trying to improve on.
func resolveRollout(bs *deployv1alpha1.BirService) resolvedRollout {
	// The SLO window governs the soak, so it is resolved first.
	ar := resolveAutoRollback(bs)
	sloWindow, ok := parsePromDuration(ar.window)
	if !ok {
		sloWindow = minRolloutStepDuration / 2
	}

	out := resolvedRollout{
		steps:      defaultRolloutSteps,
		stepDur:    stepDurationFor(sloWindow),
		maxStepDur: defaultRolloutMaxStepDuration,
		sloWindow:  sloWindow,
	}

	rs := bs.Spec.Rollout
	if rs == nil || !strings.EqualFold(strings.TrimSpace(rs.Strategy), rolloutStrategyProgressive) {
		return out
	}

	// Only the pool's primary ramps. A standalone service is always its own primary,
	// so this is a no-op for the common case.
	//
	// It matters because the primary is the only member that owns an HTTPRoute — a
	// non-primary member creates none at all. A ramp there would build a
	// next-revision Deployment, wait for traffic that no route can send it, find no
	// SLO signal, and hold; and because holding pins the image, the member's deploys
	// would stop entirely. Worse, the config that reaches a pool usually reaches every
	// member at once (a YAML anchor in the catalog), so one `rollout:` block would do
	// that to all of them simultaneously.
	//
	// Restricting the ramp to the primary also makes the route composition local: the
	// only member that can be mid-ramp is the one writing the route, so composing the
	// split needs no lookup of any sibling's state.
	if !routeIsPrimary(bs) {
		return out
	}
	out.progressive = true

	if steps := sanitizeRolloutSteps(rs.Steps); len(steps) > 0 {
		out.steps = steps
	}
	if d, ok := parsePromDuration(rs.StepDuration); ok {
		out.stepDur = d
	}
	if d, ok := parsePromDuration(rs.MaxStepDuration); ok {
		out.maxStepDur = d
	}
	// An explicit soak shorter than the window is honoured — the developer may have
	// reasons — but it is recorded so the ramp can say once that its verdicts are
	// reaching back into the previous step.
	out.dilutedWindow = out.stepDur <= out.sloWindow

	for _, s := range out.steps {
		if s > maxBalancedStep {
			out.underProvisionedSteps = append(out.underProvisionedSteps, s)
		}
	}
	// The ramp must be allowed to finish. A maxStepDuration below the soak would end
	// every step in a hold before it could ever be judged.
	if out.maxStepDur < out.stepDur {
		out.maxStepDur = out.stepDur
	}

	// Analysis defaults to the service's autoRollback mode, so an objective is
	// described once. Enforcement stays opt-in: a ramp that only reports is still
	// useful (it paces the deploy and surfaces breaches) and cannot misfire.
	mode := strings.TrimSpace(rs.Analysis)
	if mode == "" {
		mode = resolveAutoRollback(bs).mode
	}
	out.enforce = strings.EqualFold(mode, autoRollbackModeEnforce)

	return out
}

// sanitizeRolloutSteps keeps only a strictly increasing run of 1-99 percentages.
// 100 is rejected rather than clamped — see defaultRolloutSteps. A list that does not
// survive this returns empty and the caller falls back to the default.
func sanitizeRolloutSteps(in []int32) []int32 {
	out := make([]int32, 0, len(in))
	prev := int32(0)
	for _, s := range in {
		if s < 1 || s > 99 || s <= prev {
			return nil
		}
		out = append(out, s)
		prev = s
	}
	return out
}

// parsePromDuration converts a Prometheus-style duration to a Duration, reusing the
// same validation the HPA window and SLO window already use so a service configures
// durations one way throughout.
func parsePromDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !validPromDuration(s) {
		return 0, false
	}
	secs, err := promDurationSeconds(s)
	if err != nil || secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// nextRevisionReplicas sizes the temporary Deployment so its pods carry the same
// per-pod load as the main Deployment's.
//
// This is not cosmetic. The weight decides the share of REQUESTS, not of capacity:
// one pod holding 50% next to four pods sharing the other 50% takes four times the
// per-pod traffic, and the latency objective it is being judged against is the one
// the main Deployment meets at a quarter of that. The version would be condemned for
// being under-provisioned by the deploy mechanism, and the signal would look exactly
// like a genuine regression.
//
// Equal per-pod load means weight/next == (100-weight)/stable, so
// next = stable * weight / (100 - weight); rounded up, and at least one pod.
func nextRevisionReplicas(stableReplicas, weight int32) int32 {
	if stableReplicas < 1 {
		stableReplicas = 1
	}
	if weight >= 100 {
		return stableReplicas
	}
	if weight < 1 {
		return 1
	}
	n := int32(math.Ceil(float64(stableReplicas) * float64(weight) / float64(100-weight)))
	if n < 1 {
		n = 1
	}
	if n > stableReplicas {
		n = stableReplicas
	}
	return n
}

// rampingTag is the tag a ramp is (or would be) shifting traffic onto, when it
// differs from what the instance is pinned to. Empty means there is nothing to ramp.
//
// It is derived from the RESOLVED image rather than from status.BuildTag, so it works
// for both ways a service gets a version: a pipeline build (where the two agree) and
// a plain spec.image bump (where BuildTag is never set at all). Reading BuildTag
// would have made progressive silently do nothing for image-based services — the
// worst kind of failure for a safety feature, since the developer sees the knob
// accepted and assumes they are protected.
func rampingTag(bs *deployv1alpha1.BirService, desiredTag string) string {
	desired := strings.TrimSpace(desiredTag)
	if desired == "" || desired == "latest" {
		// An untagged or mutable-tagged image cannot be told apart from the version
		// already running, so there is no way to judge one against the other.
		return ""
	}
	if desired == bs.Status.StableTag {
		return ""
	}
	return desired
}

// progressiveRampInFlight reports whether a ramp is actively stepping. Used to decide
// whether a newer build arriving mid-ramp should restart it.
func progressiveRampInFlight(bs *deployv1alpha1.BirService) bool {
	if bs.Status.Rollout == nil {
		return false
	}
	switch bs.Status.Rollout.Phase {
	case rolloutPhaseProgressing, rolloutPhaseHeld:
		return true
	default:
		return false
	}
}

// progressiveHoldsImage reports whether the instance's image must stay pinned to
// status.StableTag instead of following the newest build.
//
// RolledBack is in this set, and leaving it out was a bug that undid the entire
// point of the ramp: aborting removed the temporary Deployment and dropped its
// weight to 0, then the pin came off and the main Deployment rolled out to the very
// tag the ramp had just condemned — exposing 100% of users to a version that had
// been rejected at 5%. A condemned tag has to keep being condemned. This mirrors
// quarantineTag: the instance keeps serving the known-good version until a NEW tag
// (a fix) arrives, rather than until the operator forgets.
func progressiveHoldsImage(bs *deployv1alpha1.BirService) bool {
	if bs.Status.Rollout == nil {
		return false
	}
	switch bs.Status.Rollout.Phase {
	case rolloutPhaseProgressing, rolloutPhaseHeld, rolloutPhaseRolledBack:
		return true
	default:
		return false
	}
}

// progressiveOwnsRollbackDecision reports whether the ramp — not the crash-loop
// auto-rollback gate — is the thing judging this version right now. While it is, the
// crash-loop gate stands down, because two mechanisms quarantining the same tag from
// different evidence would fight over the instance's image.
//
// Only the stepping phases qualify. Two exclusions matter:
//
//   - RolledBack: the temporary Deployment is gone and the instance is simply running
//     its known-good version, so it should have exactly the protection any other
//     steady-state service has.
//
//   - Promoting: the ramp has stopped judging anything — it is waiting for the
//     instance's own rollout. The ramp watches request metrics on the temporary
//     Deployment and has no view of whether the instance's NEW pods come up at all,
//     so suppressing the crash-loop gate here left a version that ramped cleanly but
//     crashes on the instance with nothing watching it: the rollout never completes,
//     the ramp waits forever, and the share routed to the instance 503s the whole
//     time. The crash gate is precisely the mechanism for that, so it stays on.
func progressiveOwnsRollbackDecision(bs *deployv1alpha1.BirService) bool {
	if bs.Status.Rollout == nil {
		return false
	}
	switch bs.Status.Rollout.Phase {
	case rolloutPhaseProgressing, rolloutPhaseHeld:
		return true
	default:
		return false
	}
}

// planProgressiveRollout decides, BEFORE the main Deployment is written, whether a
// ramp should start, continue, or end — because that decision is what pins the main
// Deployment's image for this reconcile.
//
// It returns whether a ramp is in flight. Everything it changes lives in
// bs.Status.Rollout and bs.Status.StableTag, both updated in place so the caller
// reads the post-decision state.
func (r *BirServiceReconciler) planProgressiveRollout(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService, desiredTag string) (bool, error) {
	l := log.FromContext(ctx)
	cfg := resolveRollout(bs)

	// An explicitly enabled canary owns the HTTPRoute weight. Two mechanisms bidding
	// for the same backendRef would flap the split, so the automatic one stands down
	// and the manual one — which a human is actively driving — wins.
	if !cfg.progressive || (bs.Spec.Canary != nil && bs.Spec.Canary.Enabled) {
		if bs.Status.Rollout != nil || bs.Status.StableTag != "" {
			return false, r.clearRolloutState(ctx, req, bs)
		}
		return false, nil
	}

	tag := rampingTag(bs, desiredTag)
	st := bs.Status.Rollout

	// A ramp already running against a tag that is no longer the newest build: the
	// pipeline pushed again mid-ramp. Restart against the new tag rather than
	// finishing the old one — promoting a superseded version would put something
	// nobody is waiting for into production, and carrying the old step index over
	// would credit the new code with soak time it never served.
	if st != nil && st.Tag != "" && tag != "" && st.Tag != tag && progressiveRampInFlight(bs) {
		l.Info("progressive rollout: newer build arrived mid-ramp, restarting", "was", st.Tag, "now", tag)
		st = nil
	}

	// Nothing to ramp: either no new build, or the instance already runs it.
	if tag == "" {
		if st != nil && st.Phase == rolloutPhaseProgressing {
			// The ramp's tag became the instance's tag — promotion completed.
			return false, r.setRolloutState(ctx, req, bs, nil)
		}
		if bs.Status.Rollout != nil || bs.Status.StableTag != "" {
			return false, r.clearRolloutState(ctx, req, bs)
		}
		return false, nil
	}

	// A finished ramp (promoted or aborted) for THIS tag must not restart itself.
	// RolledBack in particular is terminal until a new build arrives: re-ramping a
	// version already judged bad would put it back in front of users on a loop.
	if st != nil && st.Tag == tag {
		switch st.Phase {
		case rolloutPhaseRolledBack:
			// Stay pinned to the known-good version. Returning false here would
			// release the pin and let the instance roll out to the tag the ramp just
			// condemned — see progressiveHoldsImage.
			return true, nil
		case rolloutPhasePromoting:
			// Promotion in progress: unpin so the main Deployment moves to this tag.
			if bs.Status.StableTag != "" {
				if err := r.updateStableTag(ctx, req, bs, ""); err != nil {
					return false, err
				}
				bs.Status.StableTag = ""
			}
			return false, nil
		}
	}

	// Start a ramp. Locking StableTag to what the instance runs today is what keeps
	// the main Deployment still while the new version is on trial beside it.
	if st == nil || st.Tag != tag {
		if bs.Status.StableTag == "" {
			current, err := r.runningInstanceTag(ctx, bs)
			if err != nil {
				return false, err
			}
			if current == tag {
				// The instance already serves this tag. Reached on every reconcile
				// after a promotion completes, so it must be silent — logging here
				// would narrate a non-event for every progressive service forever.
				return false, nil
			}
			if current == "" {
				// Nothing to shift traffic AWAY from: the first ever deploy, or a
				// service whose running tag cannot be named. Ramping would mean
				// sending 5% to the new version and 95% to nothing.
				l.Info("progressive rollout: no previous version to ramp from, deploying immediately", "tag", tag)
				return false, nil
			}
			if err := r.updateStableTag(ctx, req, bs, current); err != nil {
				return false, err
			}
			bs.Status.StableTag = current
		}
		l.Info("progressive rollout: starting ramp",
			"tag", tag, "from", bs.Status.StableTag, "steps", cfg.steps,
			"stepDuration", cfg.stepDur, "sloWindow", cfg.sloWindow)
		if len(cfg.underProvisionedSteps) > 0 && r.Recorder != nil {
			r.Recorder.Eventf(bs, corev1.EventTypeWarning, "RolloutStepsUnderProvisioned",
				"rollout.steps %v exceed %d%%, where the new revision cannot be given enough pods to carry its share at the instance's per-pod load; it will look slower than it is and a latency objective may abort the rollout. Promote from %d%% instead of ramping past it.",
				cfg.underProvisionedSteps, maxBalancedStep, maxBalancedStep)
		}
		if cfg.dilutedWindow && r.Recorder != nil {
			// Not fatal, but it weakens every verdict in the ramp: the query that
			// judges a step reaches further back than the step has existed, so it
			// still contains the previous — passing — step's traffic.
			r.Recorder.Eventf(bs, corev1.EventTypeWarning, "RolloutWindowDiluted",
				"rollout.stepDuration (%s) does not exceed the SLO window (%s), so each step is judged partly on the previous step's traffic; raise stepDuration above traffic.autoRollback.window",
				cfg.stepDur, cfg.sloWindow)
		}
		return true, r.setRolloutState(ctx, req, bs, &deployv1alpha1.RolloutStatus{
			Phase:         rolloutPhaseProgressing,
			Tag:           tag,
			Step:          0,
			Weight:        cfg.steps[0],
			StepStartedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}

	return progressiveHoldsImage(bs), nil
}

// runningInstanceTag is the tag the instance is serving right now, read off the live
// Deployment's pod template.
//
// The Deployment is the only honest source here. status.BuildTag is already the NEW
// tag by the time a ramp is being planned, and status.BuildImage is only written by
// the build flow — an image-based service has neither. The pod template's build-tag
// label is written by every deploy through either path, so it answers "what is
// actually running" in both.
//
// Empty means no Deployment yet (the first deploy) or an untagged image, and the
// caller treats both as "nothing to ramp away from".
func (r *BirServiceReconciler) runningInstanceTag(ctx context.Context, bs *deployv1alpha1.BirService) (string, error) {
	if t := strings.TrimSpace(bs.Status.StableTag); t != "" {
		return t, nil
	}
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: fmt.Sprintf("%s-deploy", bs.Name), Namespace: bs.Namespace}
	if err := r.Get(ctx, key, &dep); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	return strings.TrimSpace(dep.Spec.Template.Labels[labelBuildTag]), nil
}

// reconcileNextRevision brings the temporary Deployment and Service to match the
// current ramp state, then advances the step machine. It returns the Service name and
// weight for the HTTPRoute split — ("", 0) whenever no ramp is in flight, which is the
// default path and leaves the route exactly as it was before this feature existed.
func (r *BirServiceReconciler) reconcileNextRevision(
	ctx context.Context,
	req ctrl.Request,
	bs *deployv1alpha1.BirService,
	port, containerPort int32,
	resources corev1.ResourceRequirements,
	nodeSelector map[string]string,
	tolerations []corev1.Toleration,
	stableReplicas int32,
) (string, int32, time.Duration, error) {
	depName := nextRevisionDepName(bs)
	svcName := nextRevisionSvcName(bs)

	// A human's override takes effect before anything else this pass, so an abort cuts
	// traffic on the same reconcile rather than after one more soak.
	if _, err := r.consumeRolloutAction(ctx, req, bs); err != nil {
		return "", 0, 0, err
	}

	st := bs.Status.Rollout
	if st == nil || (st.Phase != rolloutPhaseProgressing && st.Phase != rolloutPhaseHeld && st.Phase != rolloutPhasePromoting) {
		return "", 0, 0, r.deleteNextRevision(ctx, bs, depName, svcName)
	}

	// Promotion: the main Deployment is moving onto the ramped tag. The temporary
	// pair MUST outlive that rollout — it is still carrying its share of traffic, and
	// deleting it here would drop that share on the floor while main is still coming
	// up. Hold weight steady until main is fully rolled out, then let go.
	if st.Phase == rolloutPhasePromoting {
		// The crash-loop gate runs during promotion (see progressiveOwnsRollbackDecision)
		// and may have condemned the tag while the instance was rolling onto it: a
		// version can serve fine on the temporary Deployment and still fail to start on
		// the instance — different node pool, different resource limits, a PDB that
		// will not let the old pods go. When that happens the instance is already being
		// reverted, and the promotion this ramp is waiting for will never arrive.
		//
		// Without this the ramp waits forever on a rollout that was abandoned, holding
		// the temporary Deployment at its last weight while the instance's share serves
		// nothing.
		quarantined, err := r.tagWasQuarantined(ctx, bs, st.Tag)
		if err != nil {
			return "", 0, 0, err
		}
		if quarantined {
			msg := fmt.Sprintf("tag %s passed its ramp but failed to start on the instance; the crash-loop gate reverted it", st.Tag)
			log.FromContext(ctx).Info("progressive rollout: promotion abandoned", "tag", st.Tag)
			if r.Recorder != nil {
				r.Recorder.Eventf(bs, corev1.EventTypeWarning, "RolloutPromotionFailed",
					"%s; the temporary revision was removed and traffic is back on the running version", msg)
			}
			if err := r.deleteNextRevision(ctx, bs, depName, svcName); err != nil {
				return "", 0, 0, err
			}
			next := *st
			next.Phase = rolloutPhaseRolledBack
			next.Weight = 0
			next.Message = msg
			return "", 0, requeueRolloutStep, r.setRolloutState(ctx, req, bs, &next)
		}

		done, err := r.mainRolloutComplete(ctx, bs, st.Tag)
		if err != nil {
			return "", 0, 0, err
		}
		if !done {
			return svcName, st.Weight, requeueRolloutStep, nil
		}
		// Main now serves the new tag on its own replicas. Both sides are identical
		// code, so shifting the remaining share back is not a user-visible change.
		if r.Recorder != nil {
			r.Recorder.Eventf(bs, corev1.EventTypeNormal, "RolloutPromoted",
				"tag %s completed its progressive rollout and is now serving on the instance", st.Tag)
		}
		if err := r.deleteNextRevision(ctx, bs, depName, svcName); err != nil {
			return "", 0, 0, err
		}
		return "", 0, 0, r.setRolloutState(ctx, req, bs, nil)
	}

	image := fmt.Sprintf("%s/%s:%s", r.effectiveRegistryURL(), appName(bs), st.Tag)

	// Size against what the instance is ACTUALLY running, not what its spec asks for:
	// under an HPA those differ by design, and sizing the new revision off spec.replicas
	// while the instance sits at ten times that would judge it under a load its pod
	// count was never meant to carry.
	if live, err := r.mainDeploymentReplicas(ctx, bs); err != nil {
		return "", 0, 0, err
	} else if live > 0 {
		stableReplicas = live
	}
	replicas := nextRevisionReplicas(stableReplicas, st.Weight)

	if err := r.upsertNextRevision(ctx, bs, depName, image, st.Tag, replicas, port, containerPort, resources, nodeSelector, tolerations); err != nil {
		return "", 0, 0, err
	}
	if err := r.upsertNextRevisionService(ctx, bs, svcName, depName, port, containerPort); err != nil {
		return "", 0, 0, err
	}

	// Until the new revision has Ready pods it takes NO traffic. Weighting a
	// backendRef whose Service has no endpoints is a 503 for that share — the ramp
	// would cause the very outage it exists to prevent.
	ready, err := r.nextRevisionReady(ctx, bs, depName, replicas)
	if err != nil {
		return "", 0, 0, err
	}
	if !ready {
		return "", 0, requeueRolloutStep, nil
	}

	weight, requeue, err := r.advanceRolloutStep(ctx, req, bs)
	if err != nil {
		return "", 0, 0, err
	}
	return svcName, weight, requeue, nil
}

// consumeRolloutAction applies a human's override, if one is pending, and clears the
// annotation so it acts exactly once. Returns whether the ramp state changed.
func (r *BirServiceReconciler) consumeRolloutAction(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService) (bool, error) {
	action := strings.ToLower(strings.TrimSpace(bs.Annotations[annotRolloutAction]))
	if action == "" || bs.Status.Rollout == nil {
		return false, nil
	}
	l := log.FromContext(ctx)
	st := bs.Status.Rollout

	// Clear the annotation first. If the status write below fails the reconcile
	// retries, and a still-present annotation would apply the action a second time.
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latest deployv1alpha1.BirService
		if err := r.Get(ctx, req.NamespacedName, &latest); err != nil {
			return err
		}
		if latest.Annotations == nil {
			return nil
		}
		delete(latest.Annotations, annotRolloutAction)
		return r.Update(ctx, &latest)
	}); err != nil {
		return false, err
	}
	delete(bs.Annotations, annotRolloutAction)

	next := *st
	switch action {
	case rolloutActionPromote:
		l.Info("progressive rollout: promoted by operator", "tag", st.Tag, "atWeight", st.Weight)
		if r.Recorder != nil {
			r.Recorder.Eventf(bs, corev1.EventTypeNormal, "RolloutPromotedManually",
				"tag %s was promoted by hand at %d%% traffic, skipping the remaining steps", st.Tag, st.Weight)
		}
		next.Phase = rolloutPhasePromoting
		next.Message = fmt.Sprintf("promoted by hand at %d%% traffic", st.Weight)
	case rolloutActionAbort:
		l.Info("progressive rollout: aborted by operator", "tag", st.Tag, "atWeight", st.Weight)
		if r.Recorder != nil {
			r.Recorder.Eventf(bs, corev1.EventTypeWarning, "RolloutAbortedManually",
				"tag %s was aborted by hand at %d%% traffic; traffic is back on the running version", st.Tag, st.Weight)
		}
		next.Phase = rolloutPhaseRolledBack
		next.Weight = 0
		next.Message = fmt.Sprintf("aborted by hand at %d%% traffic", st.Weight)
	default:
		if r.Recorder != nil {
			r.Recorder.Eventf(bs, corev1.EventTypeWarning, "RolloutActionInvalid",
				"unknown %s=%q; expected %q or %q", annotRolloutAction, action, rolloutActionPromote, rolloutActionAbort)
		}
		return false, nil
	}
	return true, r.setRolloutState(ctx, req, bs, &next)
}

// advanceRolloutStep runs the step machine for a ramp whose new revision is Ready.
func (r *BirServiceReconciler) advanceRolloutStep(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService) (int32, time.Duration, error) {
	l := log.FromContext(ctx)
	cfg := resolveRollout(bs)
	st := bs.Status.Rollout

	startedAt, err := time.Parse(time.RFC3339, st.StepStartedAt)
	if err != nil {
		// Unparseable timestamp: restart this step's clock rather than treating the
		// step as infinitely old and promoting on no evidence.
		next := *st
		next.StepStartedAt = time.Now().UTC().Format(time.RFC3339)
		return st.Weight, requeueRolloutStep, r.setRolloutState(ctx, req, bs, &next)
	}
	elapsed := time.Since(startedAt)

	// Still soaking. The step's traffic has to be in the metrics window before it can
	// be judged, so there is nothing to decide yet.
	if elapsed < cfg.stepDur {
		return st.Weight, requeueRolloutStep, nil
	}

	verdict := r.evaluateSLO(ctx, bs, resolveAutoRollback(bs), st.Tag, elapsed)

	// Breach → abort. Weight to 0 and the temporary Deployment goes; the instance
	// itself was never modified, so there is no rollback to perform and no capacity
	// to recover — the previous version has been serving the remaining share at full
	// strength throughout.
	if verdict.evaluated && verdict.breached && cfg.enforce {
		msg := fmt.Sprintf("tag %s breached its SLO at %d%% traffic (%s)", st.Tag, st.Weight, verdict.reason)
		l.Info("progressive rollout: aborting", "tag", st.Tag, "weight", st.Weight, "reason", verdict.reason)
		if r.Recorder != nil {
			r.Recorder.Eventf(bs, corev1.EventTypeWarning, "RolloutAborted",
				"%s; traffic returned to the running version and the new one was removed", msg)
		}
		next := *st
		next.Phase = rolloutPhaseRolledBack
		next.Weight = 0
		next.Message = msg
		return 0, requeueRolloutStep, r.setRolloutState(ctx, req, bs, &next)
	}
	if verdict.evaluated && verdict.breached && !cfg.enforce {
		// monitor mode: report and keep going, so the signal can be trusted before it
		// is given the trigger.
		if r.Recorder != nil {
			r.Recorder.Eventf(bs, corev1.EventTypeWarning, "RolloutSLOBreach",
				"tag %s is breaching its SLO at %d%% traffic (%s); analysis is monitor, so the rollout continues",
				st.Tag, st.Weight, verdict.reason)
		}
	}

	// No verdict. Two very different situations produce one, and conflating them was
	// a bug: a service that COULD be judged but wasn't, versus a service that can
	// never be judged at all.
	if !verdict.evaluated {
		// Analysis is impossible here by configuration — the service has no mesh (so
		// there are no Istio request metrics for it), or the operator was started
		// without a Prometheus. Holding would mean every deploy of such a service
		// stalls at the first step forever, which turns a safety feature into an
		// outage. Ramp on the clock instead: it still paces the rollout and still
		// limits blast radius, because readiness alone keeps traffic off a version
		// that cannot start. It just cannot catch a version that starts and misbehaves.
		if !r.sloAnalysisPossible(bs) {
			l.V(1).Info("progressive rollout: no SLO signal is possible for this service, ramping on time only",
				"tag", st.Tag, "weight", st.Weight)
			return r.commitRolloutAdvance(ctx, req, bs, cfg, st)
		}

		// Analysis IS possible but produced nothing: too little traffic in the window,
		// or Prometheus is unreachable right now.
		//
		// This is where promotion and rollback part company. The rollback gate fails
		// OPEN (a monitoring outage must never roll back the fleet), but promotion
		// must fail CLOSED: "we saw no problem" and "we saw nothing" are the same
		// observation when there is no data, and treating the second as a pass would
		// walk an unwatched version to 100% on a low-traffic service precisely
		// because nobody was hitting it. So the ramp HOLDS at its current weight —
		// safe, since the version is serving a small share — and after
		// maxStepDuration it stops and asks for a human instead of guessing.
		if elapsed < cfg.maxStepDur {
			return st.Weight, requeueRolloutStep, nil
		}
		if st.Phase != rolloutPhaseHeld {
			msg := fmt.Sprintf("no SLO signal for tag %s after %s at %d%% traffic (too little traffic, or Prometheus is unreachable)",
				st.Tag, cfg.maxStepDur, st.Weight)
			l.Info("progressive rollout: holding", "tag", st.Tag, "weight", st.Weight)
			if r.Recorder != nil {
				r.Recorder.Eventf(bs, corev1.EventTypeWarning, "RolloutHeld",
					"%s; the rollout is paused at this weight and will not promote on its own", msg)
			}
			next := *st
			next.Phase = rolloutPhaseHeld
			next.Message = msg
			return st.Weight, requeueRolloutStep, r.setRolloutState(ctx, req, bs, &next)
		}
		return st.Weight, requeueRolloutStep, nil
	}

	// Step passed.
	return r.commitRolloutAdvance(ctx, req, bs, cfg, st)
}

// sloAnalysisPossible reports whether an SLO verdict could ever be produced for this
// service, as opposed to merely being absent right now.
//
// Both conditions are structural, not transient: a service with no spec.traffic is
// not in the mesh and therefore has no Istio request metrics at all, and an operator
// with no PROMETHEUS_URL has nothing to ask. Neither resolves by waiting, which is
// what separates this from "the query came back empty".
func (r *BirServiceReconciler) sloAnalysisPossible(bs *deployv1alpha1.BirService) bool {
	return r.PromURL != "" && resolveAutoRollback(bs).mode != autoRollbackModeOff
}

// commitRolloutAdvance moves the ramp to the next step, or hands over to promotion
// after the last one. Shared by the "step passed" and "nothing to judge, ramp on
// time" paths so they cannot advance differently.
func (r *BirServiceReconciler) commitRolloutAdvance(
	ctx context.Context,
	req ctrl.Request,
	bs *deployv1alpha1.BirService,
	cfg resolvedRollout,
	st *deployv1alpha1.RolloutStatus,
) (int32, time.Duration, error) {
	l := log.FromContext(ctx)

	nextIdx := st.Step + 1
	if int(nextIdx) >= len(cfg.steps) {
		l.Info("progressive rollout: ramp complete, promoting", "tag", st.Tag)
		next := *st
		next.Phase = rolloutPhasePromoting
		next.Message = ""
		return st.Weight, requeueRolloutStep, r.setRolloutState(ctx, req, bs, &next)
	}

	next := *st
	next.Phase = rolloutPhaseProgressing
	next.Step = nextIdx
	next.Weight = cfg.steps[nextIdx]
	next.StepStartedAt = time.Now().UTC().Format(time.RFC3339)
	next.Message = ""
	l.Info("progressive rollout: step passed", "tag", st.Tag, "weight", next.Weight)
	return next.Weight, requeueRolloutStep, r.setRolloutState(ctx, req, bs, &next)
}

// mainRolloutComplete reports whether the main Deployment is fully serving tag.
func (r *BirServiceReconciler) mainRolloutComplete(ctx context.Context, bs *deployv1alpha1.BirService, tag string) (bool, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: fmt.Sprintf("%s-deploy", bs.Name), Namespace: bs.Namespace}
	if err := r.Get(ctx, key, &dep); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if dep.Spec.Template.Labels[labelBuildTag] != tag {
		// Main has not picked up the new tag yet.
		return false, nil
	}
	return dep.Status.ObservedGeneration >= dep.Generation &&
		dep.Status.UpdatedReplicas == dep.Status.Replicas &&
		dep.Status.UnavailableReplicas == 0 &&
		dep.Status.AvailableReplicas > 0, nil
}

// tagWasQuarantined reports whether the crash-loop gate has condemned this tag on the
// instance. The gate records that decision as an annotation on the Deployment, which
// is also how it survives an operator restart, so reading it is how the ramp learns
// that the promotion it is waiting for has been abandoned.
func (r *BirServiceReconciler) tagWasQuarantined(ctx context.Context, bs *deployv1alpha1.BirService, tag string) (bool, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: fmt.Sprintf("%s-deploy", bs.Name), Namespace: bs.Namespace}
	if err := r.Get(ctx, key, &dep); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return tag != "" && dep.Annotations[annotRolledBackTag] == tag, nil
}

// mainDeploymentReplicas is the instance's live replica count. 0 when it cannot be
// read, which the caller treats as "keep the fallback".
func (r *BirServiceReconciler) mainDeploymentReplicas(ctx context.Context, bs *deployv1alpha1.BirService) (int32, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: fmt.Sprintf("%s-deploy", bs.Name), Namespace: bs.Namespace}
	if err := r.Get(ctx, key, &dep); err != nil {
		return 0, client.IgnoreNotFound(err)
	}
	if dep.Status.Replicas > 0 {
		return dep.Status.Replicas, nil
	}
	if dep.Spec.Replicas != nil {
		return *dep.Spec.Replicas, nil
	}
	return 0, nil
}

// nextRevisionReady reports whether the temporary Deployment can carry traffic.
func (r *BirServiceReconciler) nextRevisionReady(ctx context.Context, bs *deployv1alpha1.BirService, depName string, want int32) (bool, error) {
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: depName, Namespace: bs.Namespace}, &dep); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return dep.Status.ReadyReplicas >= want && dep.Status.ReadyReplicas > 0, nil
}

// upsertNextRevision writes the temporary Deployment.
//
// Its selector must differ from the main Deployment's, or the two controllers would
// each try to own the other's pods. Every other property comes from the same builders
// the main Deployment uses (see pod_template.go), so the version on trial is judged
// as the real thing rather than as a stripped-down copy of it.
func (r *BirServiceReconciler) upsertNextRevision(
	ctx context.Context,
	bs *deployv1alpha1.BirService,
	depName, image, tag string,
	replicas, port, containerPort int32,
	resources corev1.ResourceRequirements,
	nodeSelector map[string]string,
	tolerations []corev1.Toleration,
) error {
	selector := map[string]string{
		"app.kubernetes.io/name":        depName,
		"app.kubernetes.io/managed-by":  "easy-deploy-operator",
		"deploy.easydeploy.io/tenant":   bs.Namespace,
		"deploy.easydeploy.io/revision": "next",
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		dep := appsv1.Deployment{}
		dep.Name = depName
		dep.Namespace = bs.Namespace

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, &dep, func() error {
			dep.ObjectMeta.Labels = mergeStringMap(dep.ObjectMeta.Labels, selector)
			dep.ObjectMeta.Labels[labelApp] = appName(bs)

			rep := replicas
			dep.Spec.Replicas = &rep
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
			// joinPool: false — the temporary pods must NOT answer on the pool
			// Service. See revisionPodLabels.
			dep.Spec.Template.ObjectMeta.Labels = revisionPodLabels(bs, selector, tag, false)
			dep.Spec.Template.ObjectMeta.Annotations = r.tracingAnnotations(ctx, bs, dep.Spec.Template.ObjectMeta.Annotations)

			templateSpec := &dep.Spec.Template.Spec
			if strings.HasPrefix(image, r.effectiveRegistryURL()+"/") {
				if r.ensureRegistryPushSecret(ctx, bs.Namespace) == nil {
					templateSpec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: registryPushSecretName}}
				}
			}

			dep.Spec.MinReadySeconds = 5
			dep.Spec.ProgressDeadlineSeconds = int32Ptr(600)
			dep.Spec.RevisionHistoryLimit = int32Ptr(2)

			preStopSleep, drainBuffer := resolveShutdown(bs)
			grace := int64(preStopSleep) + int64(drainBuffer)
			templateSpec.TerminationGracePeriodSeconds = &grace

			templateSpec.Containers = []corev1.Container{
				appContainer(bs, image, containerPort, resources, dep.Spec.Template.ObjectMeta.Annotations),
			}
			templateSpec.NodeSelector = nodeSelector
			templateSpec.Tolerations = tolerations

			return ctrl.SetControllerReference(bs, &dep, r.Scheme)
		})
		return err
	})
}

// upsertNextRevisionService fronts the temporary Deployment's pods, and only those.
func (r *BirServiceReconciler) upsertNextRevisionService(
	ctx context.Context,
	bs *deployv1alpha1.BirService,
	svcName, depName string,
	port, containerPort int32,
) error {
	selector := map[string]string{"app.kubernetes.io/name": depName}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		svc := corev1.Service{}
		svc.Name = svcName
		svc.Namespace = bs.Namespace

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, &svc, func() error {
			svc.ObjectMeta.Labels = mergeStringMap(svc.ObjectMeta.Labels, map[string]string{
				"app.kubernetes.io/managed-by":  "easy-deploy-operator",
				"deploy.easydeploy.io/tenant":   bs.Namespace,
				"deploy.easydeploy.io/revision": "next",
			})
			// Ambient mesh: the same waypoint binding the pool Service carries, so a
			// weighted split applied at the waypoint reaches this backend too. Both
			// labels, for the same reason the pool Service sets both — and here the
			// ingress one is load-bearing rather than merely nice: a ramp is judged
			// by the new revision's SLO, that judgement reads the waypoint's
			// per-revision request metrics, and ingress traffic that skipped the
			// waypoint produces none. The canary would take real traffic and be
			// scored on an empty metric stream.
			if bsNeedsWaypoint(bs) {
				if svc.ObjectMeta.Labels == nil {
					svc.ObjectMeta.Labels = map[string]string{}
				}
				svc.ObjectMeta.Labels[labelUseWaypoint] = waypointName
				svc.ObjectMeta.Labels[labelIngressUseWaypoint] = "true"
			} else {
				delete(svc.ObjectMeta.Labels, labelUseWaypoint)
				delete(svc.ObjectMeta.Labels, labelIngressUseWaypoint)
			}
			svc.Spec.Selector = selector
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
	})
}

// deleteNextRevision removes the temporary pair. Idempotent: it is called on every
// reconcile of every service that is not ramping, which is most of them.
func (r *BirServiceReconciler) deleteNextRevision(ctx context.Context, bs *deployv1alpha1.BirService, depName, svcName string) error {
	// Service first. It stops selecting pods immediately, so the endpoints are gone
	// before the pods are — the reverse order would leave the Service pointing at
	// terminating pods and 5xx anything still routed there.
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: bs.Namespace}, svc); err == nil {
		if err := r.Delete(ctx, svc); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: depName, Namespace: bs.Namespace}, dep); err == nil {
		if err := r.Delete(ctx, dep); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// setRolloutState persists ramp state. nil clears it.
func (r *BirServiceReconciler) setRolloutState(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService, next *deployv1alpha1.RolloutStatus) error {
	if rolloutStatusEqual(bs.Status.Rollout, next) {
		return nil
	}
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latest deployv1alpha1.BirService
		if err := r.Get(ctx, req.NamespacedName, &latest); err != nil {
			return err
		}
		latest.Status.Rollout = next.DeepCopy()
		return r.Status().Update(ctx, &latest)
	}); err != nil {
		return err
	}
	bs.Status.Rollout = next.DeepCopy()
	return nil
}

// clearRolloutState drops both the ramp state and the stable-tag pin, returning the
// instance to following its newest build directly.
func (r *BirServiceReconciler) clearRolloutState(ctx context.Context, req ctrl.Request, bs *deployv1alpha1.BirService) error {
	if bs.Status.StableTag != "" && (bs.Spec.Canary == nil || !bs.Spec.Canary.Enabled) {
		if err := r.updateStableTag(ctx, req, bs, ""); err != nil {
			return err
		}
		bs.Status.StableTag = ""
	}
	return r.setRolloutState(ctx, req, bs, nil)
}

func rolloutStatusEqual(a, b *deployv1alpha1.RolloutStatus) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
