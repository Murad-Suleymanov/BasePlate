package controller

import (
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
	rules := buildHTTPRouteRules("app-svc", 8080, "", 0, nil)
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

// Canary still produces a single weighted rule (no regression).
func TestBuildHTTPRouteRules_Canary(t *testing.T) {
	rules := buildHTTPRouteRules("app-svc", 8080, "app-canary-svc", 10, nil)
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

// Path-based shared route: parent gets a /testing rule to the child plus a "/" catch-all
// to itself. This is the main/testing case from the tenant values.
func TestBuildHTTPRouteRules_PathMember(t *testing.T) {
	route := &deployv1alpha1.RouteSpec{
		Members: []deployv1alpha1.RouteMember{
			{Service: "hello-csharp-testing-svc", PathPrefix: "/testing"},
		},
	}
	rules := buildHTTPRouteRules("hello-csharp-main-svc", 8080, "", 0, route)
	if len(rules) != 2 {
		t.Fatalf("want 2 rules (member + catch-all), got %d", len(rules))
	}

	member := asMap(t, rules[0])
	if pathValue(t, member) != "/testing" {
		t.Errorf("first rule path = %q, want /testing", pathValue(t, member))
	}
	if got := backendNames(t, member); len(got) != 1 || got[0] != "hello-csharp-testing-svc" {
		t.Errorf("/testing should route to child svc, got %v", got)
	}
	// The member rule must strip its prefix so the child app receives paths from "/".
	filters, ok := member["filters"].([]interface{})
	if !ok || len(filters) != 1 {
		t.Fatalf("member rule should have 1 filter, got %#v", member["filters"])
	}
	rw := asMap(t, filters[0])
	if rw["type"] != "URLRewrite" {
		t.Errorf("filter type = %v, want URLRewrite", rw["type"])
	}
	path := asMap(t, asMap(t, rw["urlRewrite"])["path"])
	if path["type"] != "ReplacePrefixMatch" || path["replacePrefixMatch"] != "/" {
		t.Errorf("rewrite path = %#v, want ReplacePrefixMatch /", path)
	}

	catchAll := asMap(t, rules[1])
	if pathValue(t, catchAll) != "/" {
		t.Errorf("catch-all path = %q, want /", pathValue(t, catchAll))
	}
	if got := backendNames(t, catchAll); len(got) != 1 || got[0] != "hello-csharp-main-svc" {
		t.Errorf("/ should route to parent svc, got %v", got)
	}
}

// Weighted shared route (no path): child joins the catch-all as a weighted backend.
func TestBuildHTTPRouteRules_WeightedMember(t *testing.T) {
	route := &deployv1alpha1.RouteSpec{
		Members: []deployv1alpha1.RouteMember{
			{Service: "app-testing-svc", Weight: int32p(10)},
		},
	}
	rules := buildHTTPRouteRules("app-main-svc", 8080, "", 0, route)
	if len(rules) != 1 {
		t.Fatalf("weighted member should stay single-rule, got %d rules", len(rules))
	}
	got := backendNames(t, asMap(t, rules[0]))
	if len(got) != 2 || got[0] != "app-main-svc" || got[1] != "app-testing-svc" {
		t.Errorf("want [app-main-svc app-testing-svc], got %v", got)
	}
}
