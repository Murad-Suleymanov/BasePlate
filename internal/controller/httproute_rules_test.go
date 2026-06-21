package controller

import (
	"testing"
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
	rules := buildHTTPRouteRules("app-svc", 8080, "", 0, "")
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
	rules := buildHTTPRouteRules("app-svc", 8080, "app-canary-svc", 10, "")
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
	rules := buildHTTPRouteRules("app-svc", 8080, "", 0, "15s")
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
	plain := asMap(t, buildHTTPRouteRules("app-svc", 8080, "", 0, "")[0])
	if _, has := plain["timeouts"]; has {
		t.Errorf("no timeout should omit the timeouts block, got %#v", plain["timeouts"])
	}
}
