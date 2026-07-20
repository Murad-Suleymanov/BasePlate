package controller

import (
	"reflect"
	"testing"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
)

func int32p(v int32) *int32 { return &v }

// asMap is a small helper to read a rule entry as a map.
func asMap(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	m, ok := v.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", v)
	}
	return m
}

func backendNames(t *testing.T, rule map[string]interface{}) []string {
	t.Helper()
	refs, ok := rule["backendRefs"].([]interface{})
	if !ok {
		t.Fatalf("rule has no backendRefs: %#v", rule)
	}
	var names []string
	for _, r := range refs {
		names = append(names, asMap(t, r)["name"].(string))
	}
	return names
}

func pathValue(t *testing.T, rule map[string]interface{}) string {
	t.Helper()
	matches, ok := rule["matches"].([]interface{})
	if !ok || len(matches) == 0 {
		return "" // no explicit match (single-rule shape)
	}
	return asMap(t, asMap(t, matches[0])["path"])["value"].(string)
}

// Standalone service: single rule, no explicit match, just its own backend.
func TestBuildHTTPRouteRules_Standalone(t *testing.T) {
	rules := buildHTTPRouteRules("app-svc", 8080, routeSplit{}, "", nil)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	rule := asMap(t, rules[0])
	if _, has := rule["matches"]; has {
		t.Errorf("standalone rule should have no matches, got %#v", rule["matches"])
	}
	if got := backendNames(t, rule); len(got) != 1 || got[0] != "app-svc" {
		t.Errorf("want [app-svc], got %v", got)
	}
}

// A weighted pool fans out to one backendRef per member, each pinned to its share. The
// pool Service is NOT among them: routing through it would put the members back behind a
// single unweighted door and hand the split back to the pod count.
func TestBuildHTTPRouteRules_WeightedPool(t *testing.T) {
	backends := []deployv1alpha1.RouteBackend{
		{Name: "app-main", Weight: 95},
		{Name: "app-testing", Weight: 5},
	}
	rules := buildHTTPRouteRules("app-main-svc", 8080, routeSplit{}, "", backends)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	rule := asMap(t, rules[0])
	if got, want := backendNames(t, rule), []string{"app-main-inst-svc", "app-testing-inst-svc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("backends = %v, want %v", got, want)
	}
	refs := rule["backendRefs"].([]interface{})
	if w := asMap(t, refs[0])["weight"].(int64); w != 95 {
		t.Errorf("main weight = %d, want 95", w)
	}
	if w := asMap(t, refs[1])["weight"].(int64); w != 5 {
		t.Errorf("testing weight = %d, want 5", w)
	}
}

// Weights are relative, so a member whose Service isn't up yet can simply be left out: the
// remaining members keep serving in their declared proportions instead of 5xx-ing its share.
func TestBuildHTTPRouteRules_WeightedPoolPartial(t *testing.T) {
	backends := []deployv1alpha1.RouteBackend{{Name: "app-main", Weight: 95}}
	rule := asMap(t, buildHTTPRouteRules("app-main-svc", 8080, routeSplit{}, "", backends)[0])
	if got := backendNames(t, rule); len(got) != 1 || got[0] != "app-main-inst-svc" {
		t.Errorf("want only [app-main-inst-svc], got %v", got)
	}
}

// Weights win over canary: both want to own the backendRef list, and the chart already
// rejects declaring them together, so the route must not silently emit the canary split.
func TestBuildHTTPRouteRules_WeightsBeatCanary(t *testing.T) {
	backends := []deployv1alpha1.RouteBackend{
		{Name: "app-main", Weight: 95},
		{Name: "app-testing", Weight: 5},
	}
	rule := asMap(t, buildHTTPRouteRules("app-main-svc", 8080, routeSplit{canarySvc: "app-main-canary-svc", canaryWeight: 10}, "", backends)[0])
	for _, name := range backendNames(t, rule) {
		if name == "app-main-canary-svc" {
			t.Errorf("canary backend leaked into a weighted pool: %v", backendNames(t, rule))
		}
	}
}

// Canary still produces a single weighted rule (no regression).
func TestBuildHTTPRouteRules_Canary(t *testing.T) {
	rules := buildHTTPRouteRules("app-svc", 8080, routeSplit{canarySvc: "app-canary-svc", canaryWeight: 10}, "", nil)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	refs := asMap(t, rules[0])["backendRefs"].([]interface{})
	if len(refs) != 2 {
		t.Fatalf("want 2 weighted backends, got %d", len(refs))
	}
	stable, canary := asMap(t, refs[0]), asMap(t, refs[1])
	if stable["weight"].(int64) != 90 || canary["weight"].(int64) != 10 {
		t.Errorf("want 90/10 split, got %v/%v", stable["weight"], canary["weight"])
	}
}

// A route timeout is emitted as the Gateway API per-request timeout on the rule.
func TestBuildHTTPRouteRules_Timeout(t *testing.T) {
	rules := buildHTTPRouteRules("app-svc", 8080, routeSplit{}, "15s", nil)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	rule := asMap(t, rules[0])
	timeouts, ok := rule["timeouts"].(map[string]interface{})
	if !ok {
		t.Fatalf("rule should carry timeouts, got %#v", rule["timeouts"])
	}
	if timeouts["request"] != "15s" {
		t.Errorf("request timeout = %v, want 15s", timeouts["request"])
	}

	// No timeout → no timeouts block.
	plain := asMap(t, buildHTTPRouteRules("app-svc", 8080, routeSplit{}, "", nil)[0])
	if _, has := plain["timeouts"]; has {
		t.Errorf("no timeout should omit the timeouts block, got %#v", plain["timeouts"])
	}
}

// A ramp on a pool member NESTS inside the pool split instead of replacing it. This
// is the case the hello-csharp catalog produces: main at 95, testing at 5, main
// ramping a new revision. Before this, the pool branch won outright and the ramp's
// backend never appeared in the route at all — the next-revision pods were created,
// received no traffic, produced no SLO signal, and the ramp stalled forever.
func TestBuildHTTPRouteRules_RampNestsInPoolWeights(t *testing.T) {
	backends := []deployv1alpha1.RouteBackend{
		{Name: "app-main", Weight: 95},
		{Name: "app-testing", Weight: 5},
	}
	split := routeSplit{nextSvc: "app-main-next-svc", nextWeight: 5}
	rule := asMap(t, buildHTTPRouteRules("app-main-inst-svc", 8080, split, "", backends)[0])

	got := backendNames(t, rule)
	want := []string{"app-main-inst-svc", "app-main-next-svc", "app-testing-inst-svc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backends = %v, want %v", got, want)
	}

	refs := rule["backendRefs"].([]interface{})
	// main's own 95 subdivides 95/5; testing's 5 is untouched. Scaled by 100 so the
	// subdivision stays integral.
	for i, want := range []int64{9025, 475, 500} {
		if w := asMap(t, refs[i])["weight"].(int64); w != want {
			t.Errorf("backend %d weight = %d, want %d", i, w, want)
		}
	}

	// The proportions are what actually matter: main keeps 95% of the pool, of which
	// 5% is on the new revision, and testing still holds exactly 5%.
	var total int64
	for _, r := range refs {
		total += asMap(t, r)["weight"].(int64)
	}
	if total != 10000 {
		t.Fatalf("weights total %d, want 10000", total)
	}
	if pct := float64(500) / float64(total) * 100; pct != 5 {
		t.Errorf("testing share = %.2f%%, want it unchanged at 5%%", pct)
	}
}

// A non-ramping pool must emit exactly the weights it always did. Scaling them
// unconditionally would rewrite every HTTPRoute in the cluster for no gain.
func TestBuildHTTPRouteRules_IdlePoolWeightsAreUnscaled(t *testing.T) {
	backends := []deployv1alpha1.RouteBackend{
		{Name: "app-main", Weight: 95},
		{Name: "app-testing", Weight: 5},
	}
	rule := asMap(t, buildHTTPRouteRules("app-main-inst-svc", 8080, routeSplit{}, "", backends)[0])
	refs := rule["backendRefs"].([]interface{})
	if w := asMap(t, refs[0])["weight"].(int64); w != 95 {
		t.Errorf("main weight = %d, want an unscaled 95", w)
	}
	if w := asMap(t, refs[1])["weight"].(int64); w != 5 {
		t.Errorf("testing weight = %d, want an unscaled 5", w)
	}
}

// A ramp on a standalone service splits the instance against its next revision.
func TestBuildHTTPRouteRules_RampStandalone(t *testing.T) {
	split := routeSplit{nextSvc: "app-next-svc", nextWeight: 25}
	rule := asMap(t, buildHTTPRouteRules("app-svc", 8080, split, "", nil)[0])
	if got, want := backendNames(t, rule), []string{"app-svc", "app-next-svc"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backends = %v, want %v", got, want)
	}
	refs := rule["backendRefs"].([]interface{})
	if w := asMap(t, refs[0])["weight"].(int64); w != 75 {
		t.Errorf("instance weight = %d, want 75", w)
	}
	if w := asMap(t, refs[1])["weight"].(int64); w != 25 {
		t.Errorf("next weight = %d, want 25", w)
	}
}
