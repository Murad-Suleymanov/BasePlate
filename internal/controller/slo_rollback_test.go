package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

func ptr32(v int32) *int32 { return &v }

// meshBS is a BirService with mesh enabled (the SLO gate needs spec.traffic) and the
// given autoRollback config.
func meshBS(ar *deployv1alpha1.AutoRollbackSpec) *deployv1alpha1.BirService {
	bs := &deployv1alpha1.BirService{}
	bs.Name = "app"
	bs.Namespace = "team"
	bs.Spec.Traffic = &deployv1alpha1.TrafficSpec{AutoRollback: ar}
	return bs
}

func TestResolveAutoRollbackDefaults(t *testing.T) {
	// No mesh: no request metrics exist, so the gate must be off rather than silently
	// evaluating against nothing.
	noMesh := &deployv1alpha1.BirService{}
	if got := resolveAutoRollback(noMesh); got.mode != autoRollbackModeOff {
		t.Fatalf("no traffic: mode = %q, want %q", got.mode, autoRollbackModeOff)
	}

	// Mesh but no autoRollback block: platform defaults, and monitor (not enforce) so
	// enabling the mesh can never start rolling deploys back on its own.
	cfg := resolveAutoRollback(meshBS(nil))
	if cfg.mode != autoRollbackModeMonitor {
		t.Errorf("mode = %q, want monitor", cfg.mode)
	}
	if cfg.sloPercent != defaultSLOPercent {
		t.Errorf("sloPercent = %v, want %v", cfg.sloPercent, defaultSLOPercent)
	}
	if cfg.window != defaultSLOWindow {
		t.Errorf("window = %q, want %q", cfg.window, defaultSLOWindow)
	}
	if cfg.minRequests != defaultSLOMinRequests {
		t.Errorf("minRequests = %v, want %v", cfg.minRequests, defaultSLOMinRequests)
	}
	if cfg.latencyP99Ms != 0 {
		t.Errorf("latencyP99Ms = %v, want 0 (error-rate only)", cfg.latencyP99Ms)
	}
}

func TestResolveAutoRollbackOverrides(t *testing.T) {
	cfg := resolveAutoRollback(meshBS(&deployv1alpha1.AutoRollbackSpec{
		Mode:         autoRollbackModeEnforce,
		SLO:          "99.5",
		Window:       "5m",
		MinRequests:  ptr32(200),
		LatencyP99Ms: ptr32(300),
	}))
	if cfg.mode != autoRollbackModeEnforce {
		t.Errorf("mode = %q, want enforce", cfg.mode)
	}
	if cfg.sloPercent != 99.5 {
		t.Errorf("sloPercent = %v, want 99.5", cfg.sloPercent)
	}
	if cfg.window != "5m" {
		t.Errorf("window = %q, want 5m", cfg.window)
	}
	if cfg.minRequests != 200 {
		t.Errorf("minRequests = %v, want 200", cfg.minRequests)
	}
	if cfg.latencyP99Ms != 300 {
		t.Errorf("latencyP99Ms = %v, want 300", cfg.latencyP99Ms)
	}
	// 99.5% success = a 0.5% error budget.
	if budget := cfg.errorBudget(); budget < 0.0049 || budget > 0.0051 {
		t.Errorf("errorBudget = %v, want ~0.005", budget)
	}
}

// A bad value must never become a *stricter* gate than the default. An SLO that parsed as
// 0 would give a 100% error budget (never fires); worse, a window below the floor would
// make rate() noisy enough to roll back healthy versions. Both fall back to the default.
func TestResolveAutoRollbackRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		ar   *deployv1alpha1.AutoRollbackSpec
	}{
		{"unparseable slo", &deployv1alpha1.AutoRollbackSpec{SLO: "ninety-nine"}},
		{"slo out of range", &deployv1alpha1.AutoRollbackSpec{SLO: "150"}},
		{"zero slo", &deployv1alpha1.AutoRollbackSpec{SLO: "0"}},
		{"window below floor", &deployv1alpha1.AutoRollbackSpec{Window: "30s"}},
		{"malformed window", &deployv1alpha1.AutoRollbackSpec{Window: "2 minutes"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := resolveAutoRollback(meshBS(c.ar))
			if cfg.sloPercent != defaultSLOPercent {
				t.Errorf("sloPercent = %v, want default %v", cfg.sloPercent, defaultSLOPercent)
			}
			if cfg.window != defaultSLOWindow {
				t.Errorf("window = %q, want default %q", cfg.window, defaultSLOWindow)
			}
		})
	}
}

func TestPromDurationSeconds(t *testing.T) {
	cases := map[string]int{
		"30s":   30,
		"1m":    60,
		"2m":    120,
		"1h":    3600,
		"1h30m": 5400,
		"2m30s": 150,
		"500ms": 0, // sub-second rounds to 0, i.e. below the window floor, which is the point
	}
	for in, want := range cases {
		got, err := promDurationSeconds(in)
		if err != nil {
			t.Errorf("promDurationSeconds(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("promDurationSeconds(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"m", "2x", "abc", "2m3"} {
		if _, err := promDurationSeconds(bad); err == nil {
			t.Errorf("promDurationSeconds(%q) should have errored", bad)
		}
	}
}

// The baseline may only be recorded after grace + one full window, otherwise a version
// blessed at ~30s would be the only fallback for a breach detected at ~2m.
func TestObservationPeriod(t *testing.T) {
	cfg := resolveAutoRollback(meshBS(nil)) // default 2m window
	want := sloGracePeriod + 120*time.Second
	if got := cfg.observationPeriod(); got != want {
		t.Errorf("observationPeriod = %v, want %v", got, want)
	}
}

// fakeProm serves the Prometheus instant-query API. Values are matched by substring of the
// PromQL so one server can answer the volume, ratio and latency queries differently.
//
// The needles must be MUTUALLY EXCLUSIVE across the three queries, because map iteration
// order is random: use "increase" (volume), "response_code" (error ratio) and
// "histogram_quantile" (p99). A needle like "rate" would match the ratio AND the latency
// query, and whichever the map happened to yield first would win.
func fakeProm(t *testing.T, byQuery map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		for needle, val := range byQuery {
			if strings.Contains(q, needle) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"%s"]}]}}`, val)
				return
			}
		}
		// No match: an empty vector, which is how Prometheus reports "no such series".
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
}

// sloFixture wires a reconciler at a fake Prometheus and evaluates one version that is old
// enough to be judged.
func sloFixture(t *testing.T, ar *deployv1alpha1.AutoRollbackSpec, byQuery map[string]string) sloVerdict {
	t.Helper()
	srv := fakeProm(t, byQuery)
	t.Cleanup(srv.Close)

	bs := meshBS(ar)
	r := &BirServiceReconciler{PromURL: srv.URL}
	return r.evaluateSLO(context.Background(), bs, resolveAutoRollback(bs), "abc123", 10*time.Minute)
}

func TestEvaluateSLOBreachesOnErrorBudget(t *testing.T) {
	// 5% errors against the default 1% budget (SLO 99), with plenty of traffic.
	v := sloFixture(t, nil, map[string]string{
		"increase":      "1000",
		"response_code": "0.05",
	})
	if !v.evaluated {
		t.Fatal("expected the version to be evaluated")
	}
	if !v.breached {
		t.Fatalf("5%% errors vs a 1%% budget should breach; verdict: %+v", v)
	}
	if !strings.Contains(v.reason, "error rate") {
		t.Errorf("reason = %q, want it to mention the error rate", v.reason)
	}
}

func TestEvaluateSLOPassesWithinBudget(t *testing.T) {
	// 0.5% errors against a 1% budget: spending budget, but not over it.
	v := sloFixture(t, nil, map[string]string{
		"increase":      "1000",
		"response_code": "0.005",
	})
	if !v.evaluated {
		t.Fatal("expected the version to be evaluated")
	}
	if v.breached {
		t.Fatalf("0.5%% errors vs a 1%% budget must not breach; verdict: %+v", v)
	}
}

// A version that has served zero 5xx has no `response_code=~"5.."` series at all, and in
// PromQL an empty vector divided by anything is empty rather than zero. The gate must still
// reach a verdict on it: reading "no errors" as "cannot judge" held every clean progressive
// rollout at its first step until it timed out into Held — the healthier the version, the
// more certainly it was refused.
//
// This models Prometheus instead of canning an answer, because the bug lives in the shape of
// the query rather than in the value it returns: the ratio query only produces a sample if it
// defaults its own numerator to zero.
func TestEvaluateSLOJudgesAVersionWithNoErrorSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "increase"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"1000"]}]}}`)
		case strings.Contains(q, "response_code") && strings.Contains(q, "or vector(0)"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"0"]}]}}`)
		default:
			// No such series — exactly what Prometheus returns for a version with a clean
			// error record, and what the unguarded division propagates.
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		}
	}))
	defer srv.Close()

	bs := meshBS(nil)
	r := &BirServiceReconciler{PromURL: srv.URL}
	v := r.evaluateSLO(context.Background(), bs, resolveAutoRollback(bs), "abc123", 10*time.Minute)

	if !v.evaluated {
		t.Fatal("a version that has served no 5xx must still be judged, not treated as unjudgeable")
	}
	if v.breached {
		t.Fatalf("a clean version must not breach; verdict: %+v", v)
	}
	if v.errorRatio != 0 {
		t.Errorf("errorRatio = %v, want 0", v.errorRatio)
	}
}

// The volume guard is what protects a low-traffic service: 3 requests, 1 of which failed,
// is a 33% error rate but nowhere near enough evidence to condemn a version.
func TestEvaluateSLOWithTooFewRequestsIsNotJudged(t *testing.T) {
	v := sloFixture(t, nil, map[string]string{
		"increase":      "3",
		"response_code": "0.33",
	})
	if v.evaluated || v.breached {
		t.Fatalf("below minRequests the version must not be judged; verdict: %+v", v)
	}
}

// A version still inside the grace period is warming up; early failures must not condemn it.
func TestEvaluateSLOTooYoungIsNotJudged(t *testing.T) {
	srv := fakeProm(t, map[string]string{"increase": "1000", "response_code": "0.9"})
	defer srv.Close()

	bs := meshBS(nil)
	r := &BirServiceReconciler{PromURL: srv.URL}
	v := r.evaluateSLO(context.Background(), bs, resolveAutoRollback(bs), "abc123", time.Second)
	if v.evaluated || v.breached {
		t.Fatalf("a version younger than the grace period must not be judged; verdict: %+v", v)
	}
}

// Fail-open: a monitoring outage must never be able to roll back the fleet.
func TestEvaluateSLOFailsOpenWhenPrometheusIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	bs := meshBS(nil)
	r := &BirServiceReconciler{PromURL: srv.URL}
	v := r.evaluateSLO(context.Background(), bs, resolveAutoRollback(bs), "abc123", 10*time.Minute)
	if v.evaluated || v.breached {
		t.Fatalf("a Prometheus 500 must fail open, not breach; verdict: %+v", v)
	}
}

// Same for a platform with no Prometheus wired at all.
func TestEvaluateSLONoPrometheusURLIsInert(t *testing.T) {
	bs := meshBS(nil)
	r := &BirServiceReconciler{}
	v := r.evaluateSLO(context.Background(), bs, resolveAutoRollback(bs), "abc123", 10*time.Minute)
	if v.evaluated || v.breached {
		t.Fatalf("with no PromURL the gate must be inert; verdict: %+v", v)
	}
}

// mode: off is the kill switch, so no query is issued at all.
func TestEvaluateSLOModeOffIssuesNoQuery(t *testing.T) {
	queried := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queried = true
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer srv.Close()

	bs := meshBS(&deployv1alpha1.AutoRollbackSpec{Mode: autoRollbackModeOff})
	r := &BirServiceReconciler{PromURL: srv.URL}
	v := r.evaluateSLO(context.Background(), bs, resolveAutoRollback(bs), "abc123", 10*time.Minute)
	if v.evaluated || v.breached {
		t.Fatalf("mode off must not evaluate; verdict: %+v", v)
	}
	if queried {
		t.Error("mode off must not query Prometheus at all")
	}
}

// A clean error rate can still breach on latency when a latency objective is set.
func TestEvaluateSLOBreachesOnLatency(t *testing.T) {
	v := sloFixture(t, &deployv1alpha1.AutoRollbackSpec{LatencyP99Ms: ptr32(200)}, map[string]string{
		"increase":           "1000",
		"histogram_quantile": "850",
		"response_code":      "0.0", // error rate is clean
	})
	if !v.evaluated {
		t.Fatal("expected the version to be evaluated")
	}
	if !v.breached {
		t.Fatalf("p99 850ms vs a 200ms objective should breach; verdict: %+v", v)
	}
	if !strings.Contains(v.reason, "p99") {
		t.Errorf("reason = %q, want it to mention p99 latency", v.reason)
	}
}

// Objectives are OR'd, and BOTH are measured even when one has already failed. A version
// rolled back for its error rate may also be far too slow; the event must say so, since
// "what was actually wrong with the build" is the first question after a rollback.
func TestEvaluateSLOReportsBothObjectivesWhenBothBreach(t *testing.T) {
	v := sloFixture(t, &deployv1alpha1.AutoRollbackSpec{LatencyP99Ms: ptr32(200)}, map[string]string{
		"increase":           "1000",
		"response_code":      "0.05", // 5% errors vs a 1% budget
		"histogram_quantile": "850",  // and p99 850ms vs a 200ms objective
	})
	if !v.evaluated || !v.breached {
		t.Fatalf("both objectives breached, expected a breach; verdict: %+v", v)
	}
	if !strings.Contains(v.reason, "error rate") {
		t.Errorf("reason = %q, want it to mention the error rate", v.reason)
	}
	if !strings.Contains(v.reason, "p99") {
		t.Errorf("reason = %q, want it to ALSO mention p99 latency", v.reason)
	}
	// Latency must be measured, not short-circuited away by the error-rate breach.
	if v.p99Ms != 850 {
		t.Errorf("p99Ms = %v, want 850 (latency must still be queried)", v.p99Ms)
	}
}

// Latency alone can condemn a version whose error rate is clean (the OR, other direction).
// Covered by TestEvaluateSLOBreachesOnLatency.

// Latency is only gated when an objective is set: the default is error-rate only.
func TestEvaluateSLOIgnoresLatencyWhenUnset(t *testing.T) {
	v := sloFixture(t, nil, map[string]string{
		"increase":           "1000",
		"histogram_quantile": "9000", // catastrophically slow...
		"response_code":      "0.0",  // ...but no errors
	})
	if v.breached {
		t.Fatalf("with no latencyP99Ms set, latency must not breach; verdict: %+v", v)
	}
}

// Regression: a pool member must be judged on the WHOLE pool's traffic for its build tag,
// not on its own Deployment's slice of it.
//
// hello-csharp/prod runs main (3 replicas) and testing (1 replica) in one route-group pool.
// Scoping the query per-Deployment (destination_workload) meant testing only ever saw ~1/4
// of pool traffic, never reached minRequests, and was never judged -- so when a build was
// bad, main rolled back and testing kept serving it. The pool then served a MIX of the good
// and the bad version, which is worse than not rolling back at all.
//
// The query must select on destination_canonical_service (the app, shared by every pool
// member) plus destination_canonical_revision (the build tag), and must NOT pin
// destination_workload.
func TestEvaluateSLOScopesQueryToThePoolNotTheInstance(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("query"))
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"1000"]}]}}`)
	}))
	defer srv.Close()

	// The small member of the pool. appName() resolves the GitHub repo, so both members
	// report under one canonical service ("hello-csharp") while their workloads differ.
	bs := meshBS(nil)
	bs.Name = "hello-csharp-testing"
	bs.Namespace = "hello-csharp"
	bs.Spec.Repo = "https://github.com/Murad-Suleymanov/hello-csharp"

	r := &BirServiceReconciler{PromURL: srv.URL}
	r.evaluateSLO(context.Background(), bs, resolveAutoRollback(bs), "abc123", 10*time.Minute)

	if len(queries) == 0 {
		t.Fatal("expected the gate to query Prometheus")
	}
	for _, q := range queries {
		if !strings.Contains(q, `destination_canonical_service="hello-csharp"`) {
			t.Errorf("query must aggregate the pool by canonical service, got: %s", q)
		}
		if !strings.Contains(q, `destination_canonical_revision="abc123"`) {
			t.Errorf("query must pin exactly one build tag, got: %s", q)
		}
		if strings.Contains(q, "destination_workload=") {
			t.Errorf("query must NOT scope to a single Deployment -- that is the bug that let "+
				"a small pool member escape the gate entirely. Got: %s", q)
		}
	}
}

// A 0/0 ratio comes back from Prometheus as NaN. That is "no data", not "all requests
// failed" -- reading it as a breach would roll back an idle service.
func TestPromScalarTreatsNaNAsNoData(t *testing.T) {
	srv := fakeProm(t, map[string]string{"response_code": "NaN"})
	defer srv.Close()

	r := &BirServiceReconciler{PromURL: srv.URL}
	if _, ok := r.promScalar(context.Background(), `rate(x{response_code="500"}[1m])`); ok {
		t.Error("NaN must be reported as no data")
	}
}

func TestPromScalarEmptyResultIsNoData(t *testing.T) {
	srv := fakeProm(t, nil) // always an empty vector
	defer srv.Close()

	r := &BirServiceReconciler{PromURL: srv.URL}
	if _, ok := r.promScalar(context.Background(), "anything"); ok {
		t.Error("an empty result vector must be reported as no data")
	}
}
