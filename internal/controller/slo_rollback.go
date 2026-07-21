package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

// SLO-gated rollback. The crash-loop path (evaluateAutoRollback) catches a version whose
// pods never come up. It is blind to the version that comes up perfectly Ready and then
// serves 5xx or runs slow — from the kubelet's point of view nothing is wrong. This file
// closes that gap: during a rollout the operator asks Prometheus how the NEW build tag is
// actually behaving, and quarantines it if it burns its error budget.
//
// The signal is Istio request metrics sliced by destination_canonical_revision, which the
// Deployment stamps with the build tag (see labelCanonicalRevision in reconcileDeployment).
// That slicing is what makes this possible at all: with maxUnavailable:0 the old healthy
// version keeps serving during the rollout, so unsliced metrics blend both versions and a
// bad new version hides behind the old one's healthy traffic.

const (
	// Auto-rollback modes. Default is monitor: evaluate and report, never act. Enforcement
	// is a deliberate opt-in so the signal can be trusted before it is given the trigger.
	autoRollbackModeOff     = "off"
	autoRollbackModeMonitor = "monitor"
	autoRollbackModeEnforce = "enforce"

	// defaultSLOPercent is the success-rate objective when none is set: 99% success,
	// i.e. a 1% error budget.
	defaultSLOPercent = 99.0

	// defaultSLOWindow is the trailing window the error ratio is computed over. This is a
	// rollback GATE, not a smoothing window — every second added here is a second a bad
	// version keeps serving errors before it is caught. 2m is the balance point: long
	// enough for rate() to be stable over several scrapes, short enough that a bad deploy
	// is caught in ~3 minutes end to end. Low-traffic services are protected by
	// minRequests, NOT by widening this.
	defaultSLOWindow = "2m"

	// minSLOWindowSeconds is the floor. Below ~1m a rate() has too few scrapes to be
	// stable and the ratio turns into noise, which would produce false rollbacks.
	minSLOWindowSeconds = 60

	// defaultSLOMinRequests is the volume guard: below this many requests in the window
	// there is not enough evidence to condemn a version. Mirrors the outlier-detection
	// successRateRequestVolume threshold (see destinationrule_outlier.go).
	defaultSLOMinRequests = 50

	// sloGracePeriod is how long a version is left alone after its first pod appears.
	// Readiness probes already keep traffic off a warming pod, so this only guards the
	// brief post-Ready warmup (cold caches, JIT) where early requests can legitimately
	// fail. Kept short — it is dead time in which a bad version serves traffic.
	sloGracePeriod = 30 * time.Second

	// promQueryTimeout bounds a single Prometheus query. The reconcile loop must not
	// stall on a slow or wedged monitoring stack.
	promQueryTimeout = 5 * time.Second
)

// autoRollbackConfig is the resolved (spec + platform defaults) SLO gate configuration.
type autoRollbackConfig struct {
	mode         string
	sloPercent   float64 // success-rate objective, e.g. 99.5
	window       string  // Prometheus duration
	minRequests  float64
	latencyP99Ms float64 // 0 = no latency objective
}

// errorBudget is the fraction of requests allowed to fail, i.e. (100-SLO)/100.
func (c autoRollbackConfig) errorBudget() float64 { return (100 - c.sloPercent) / 100 }

// observationPeriod is how long a new version must survive before it is trusted enough to
// become the rollback baseline: the warmup grace plus one full evaluation window, so the
// window it is judged on contains only its own steady-state traffic. Until this elapses a
// Ready version is still on probation and the rollout keeps being polled.
func (c autoRollbackConfig) observationPeriod() time.Duration {
	secs, err := promDurationSeconds(c.window)
	if err != nil || secs <= 0 {
		secs = minSLOWindowSeconds
	}
	return sloGracePeriod + time.Duration(secs)*time.Second
}

// resolveAutoRollback merges spec.traffic.autoRollback with the platform defaults.
// A workload with no mesh has no request metrics, so the gate is off for it.
func resolveAutoRollback(bs *deployv1alpha1.BirService) autoRollbackConfig {
	cfg := autoRollbackConfig{
		mode:        autoRollbackModeMonitor,
		sloPercent:  defaultSLOPercent,
		window:      defaultSLOWindow,
		minRequests: defaultSLOMinRequests,
	}
	if bs.Spec.Traffic == nil {
		cfg.mode = autoRollbackModeOff
		return cfg
	}
	ar := bs.Spec.Traffic.AutoRollback
	if ar == nil {
		return cfg
	}
	if m := strings.TrimSpace(ar.Mode); m != "" {
		cfg.mode = m
	}
	if s := strings.TrimSpace(ar.SLO); s != "" {
		// A malformed SLO must not silently become 0% (which would condemn every
		// version on its first error). Keep the default instead.
		if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 && v <= 100 {
			cfg.sloPercent = v
		}
	}
	if w := strings.TrimSpace(ar.Window); w != "" && promDurationRe.MatchString(w) {
		if secs, err := promDurationSeconds(w); err == nil && secs >= minSLOWindowSeconds {
			cfg.window = w
		}
	}
	if ar.MinRequests != nil && *ar.MinRequests >= 0 {
		cfg.minRequests = float64(*ar.MinRequests)
	}
	if ar.LatencyP99Ms != nil && *ar.LatencyP99Ms > 0 {
		cfg.latencyP99Ms = float64(*ar.LatencyP99Ms)
	}
	return cfg
}

// sloVerdict is the outcome of one SLO evaluation of one build tag.
type sloVerdict struct {
	// evaluated is false when the gate could not form an opinion (mode off, no
	// Prometheus, too few requests, version too young). Not evaluated is NOT a pass —
	// it means "no data", and it must never trigger a rollback.
	evaluated  bool
	breached   bool
	reason     string // human-readable, used for the event message
	errorRatio float64
	requests   float64
	p99Ms      float64
}

// evaluateSLO asks Prometheus how build tag `tag` of this service is behaving over the
// configured window and reports whether it is breaching any of its objectives.
//
// Objectives are independent and combine as OR: the error budget always applies, and a
// latency objective applies when one is configured. Breaching EITHER condemns the version.
// Both are measured on every pass, even after one has already failed, so the resulting
// event reports everything that is wrong with the build rather than just the first thing.
//
// Fail-open by design: any missing signal (Prometheus down, no metrics yet, too little
// traffic) returns evaluated=false and never a breach. A monitoring outage must not be
// able to roll back the fleet.
func (r *BirServiceReconciler) evaluateSLO(
	ctx context.Context,
	bs *deployv1alpha1.BirService,
	cfg autoRollbackConfig,
	tag string,
	versionAge time.Duration,
) sloVerdict {
	l := log.FromContext(ctx)

	if cfg.mode == autoRollbackModeOff || tag == "" {
		return sloVerdict{}
	}
	if r.PromURL == "" {
		// Gate configured but the platform has no Prometheus wired — say so once per
		// pass rather than silently behaving as if every version were healthy.
		l.V(1).Info("SLO gate skipped: no Prometheus URL configured", "service", bs.Name)
		return sloVerdict{}
	}
	// Too young to judge: a just-Ready pod may still be warming its caches.
	if versionAge < sloGracePeriod {
		return sloVerdict{}
	}

	// Scope: ONE build tag across the WHOLE pool — canonical_service (the app, shared by
	// every route-group member) plus canonical_revision (the build tag).
	//
	// Deliberately NOT destination_workload (the Deployment). A pool is one logical service
	// whose members all run the same build tag, so "is this Deployment healthy" is the wrong
	// question and scoping per-Deployment breaks it twice:
	//   - Volume: pool traffic is split across members, so a small member (1 replica beside
	//     a 3-replica sibling) never reaches minRequests on its own and is never judged.
	//   - Consistency: members would reach different verdicts, so the big one rolls back
	//     while the small one keeps serving the bad tag — the pool ends up serving a MIX of
	//     good and bad versions, which is worse than not rolling back at all.
	// Aggregating by canonical_service fixes both: every member sees the same evidence and
	// the same verdict, so the pool rolls back as a unit. The revision label still pins the
	// query to one version, so a member pinned to a different tag is judged on its own pods.
	sel := fmt.Sprintf(
		`destination_workload_namespace=%q,destination_canonical_service=%q,destination_canonical_revision=%q`,
		bs.Namespace, appName(bs), tag,
	)

	// Volume guard first: judging a version on a handful of requests is how a low-traffic
	// service gets rolled back for a single unlucky 500.
	requests, ok := r.promScalar(ctx, fmt.Sprintf(`sum(increase(istio_requests_total{%s}[%s]))`, sel, cfg.window))
	if !ok {
		return sloVerdict{}
	}
	if requests < cfg.minRequests {
		l.V(1).Info("SLO gate: not enough traffic to judge version",
			"service", bs.Name, "tag", tag, "requests", requests, "minRequests", cfg.minRequests)
		return sloVerdict{}
	}

	// `or vector(0)` on the numerator, and it is load-bearing. The 5xx series does not
	// exist at all until the version serves its first one, and in PromQL an empty vector
	// divided by anything is empty — not zero. Without it a version with a clean error
	// record produces no result, promScalar reports that as "no data", and the caller
	// reads no data as "cannot judge": the healthier the version, the more certainly the
	// gate refuses to pass it. A progressive rollout then holds at its first step and
	// ends in Held, which is precisely backwards.
	//
	// Note this is NOT the 0/0 case the NaN check in promScalar covers — that one needs
	// the numerator series to exist and be zero. Here it was never created.
	//
	// The denominator is safe to leave bare: the volume guard above has already matched
	// the same selector and found traffic, so it cannot be empty on this path.
	ratio, ok := r.promScalar(ctx, fmt.Sprintf(
		`(sum(rate(istio_requests_total{%s,response_code=~"5.."}[%s])) or vector(0)) / sum(rate(istio_requests_total{%s}[%s]))`,
		sel, cfg.window, sel, cfg.window,
	))
	if !ok {
		return sloVerdict{}
	}

	v := sloVerdict{evaluated: true, errorRatio: ratio, requests: requests}

	// Every configured objective is evaluated, and breaching ANY of them condemns the
	// version. Both are checked even once one has already failed: a version rolled back
	// for its error rate may ALSO be far too slow, and the event should say so — the
	// first thing anyone asks after a rollback is what was actually wrong with the build.
	var reasons []string

	if budget := cfg.errorBudget(); ratio > budget {
		v.breached = true
		reasons = append(reasons, fmt.Sprintf(
			"error rate %.2f%% exceeds the %.2f%% error budget (SLO %.2f%%)",
			ratio*100, budget*100, cfg.sloPercent))
	}

	// Latency is only an objective when one is set; omitted means error-rate only.
	if cfg.latencyP99Ms > 0 {
		p99, ok := r.promScalar(ctx, fmt.Sprintf(
			`histogram_quantile(0.99, sum by (le) (rate(istio_request_duration_milliseconds_bucket{%s}[%s])))`,
			sel, cfg.window,
		))
		if ok {
			v.p99Ms = p99
			if p99 > cfg.latencyP99Ms {
				v.breached = true
				reasons = append(reasons, fmt.Sprintf(
					"p99 latency %.0fms exceeds the %.0fms objective", p99, cfg.latencyP99Ms))
			}
		}
	}

	if v.breached {
		v.reason = fmt.Sprintf("%s — over %s, %.0f requests",
			strings.Join(reasons, "; and "), cfg.window, requests)
	}
	return v
}

// promScalar runs an instant query and returns the single scalar it produced. ok is false
// when the query failed, timed out, or returned no/NaN data — every one of those means
// "no opinion", never "unhealthy".
//
// Hand-rolled over net/http rather than pulling in the Prometheus API client: this is one
// instant query with no new dependency, matching how the operator already talks to Istio
// (unstructured) instead of vendoring a client library.
func (r *BirServiceReconciler) promScalar(ctx context.Context, query string) (float64, bool) {
	l := log.FromContext(ctx)

	ctx, cancel := context.WithTimeout(ctx, promQueryTimeout)
	defer cancel()

	endpoint := strings.TrimSuffix(r.PromURL, "/") + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		l.V(1).Info("SLO gate: bad Prometheus request", "err", err)
		return 0, false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.V(1).Info("SLO gate: Prometheus unreachable, failing open", "err", err)
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		l.V(1).Info("SLO gate: Prometheus returned non-200, failing open", "status", resp.StatusCode)
		return 0, false
	}

	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				// [ <unix_time>, "<sample_value>" ] — value is a string in the Prom API.
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		l.V(1).Info("SLO gate: undecodable Prometheus response, failing open", "err", err)
		return 0, false
	}
	if body.Status != "success" || len(body.Data.Result) == 0 || len(body.Data.Result[0].Value) < 2 {
		// An empty result is the normal case for a version that has served no traffic yet.
		return 0, false
	}

	var raw string
	if err := json.Unmarshal(body.Data.Result[0].Value[1], &raw); err != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	// A 0/0 ratio comes back as NaN; treat it as no data, not as a breach.
	if f != f {
		return 0, false
	}
	return f, true
}

// promDurationSeconds converts a Prometheus duration (30s, 2m, 1h30m, …) to seconds.
// Used to enforce the SLO window floor; promDurationRe has already validated the shape.
// Sub-second units round down to 0, which is below the floor — exactly the intent.
func promDurationSeconds(d string) (int, error) {
	units := map[byte]int{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 604800, 'y': 31536000}
	total, num := 0, ""
	for i := 0; i < len(d); i++ {
		c := d[i]
		if c >= '0' && c <= '9' {
			num += string(c)
			continue
		}
		if num == "" {
			return 0, fmt.Errorf("invalid prometheus duration %q", d)
		}
		// "ms" is the one two-character unit; consume both so the 's' is not counted
		// again as seconds (which would read 500ms as 500m + 0s).
		if c == 'm' && i+1 < len(d) && d[i+1] == 's' {
			num, i = "", i+1
			continue
		}
		mult, ok := units[c]
		if !ok {
			return 0, fmt.Errorf("invalid prometheus duration %q", d)
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			return 0, err
		}
		total += n * mult
		num = ""
	}
	if num != "" {
		return 0, fmt.Errorf("invalid prometheus duration %q: trailing number", d)
	}
	return total, nil
}
