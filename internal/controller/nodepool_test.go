package controller

import (
	"context"
	"testing"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func nodePoolScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := deployv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

// A missing pool must NOT error or block: it pins to an unsatisfiable
// nodeSelector (so the pod stays Pending) and returns a warning.
func TestResolveNodePoolMissing(t *testing.T) {
	s := nodePoolScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}

	bs := &deployv1alpha1.BirService{}
	bs.Spec.NodePool = "payments"

	sel, tol, warn, err := r.resolveNodePool(context.Background(), bs)
	if err != nil {
		t.Fatalf("missing pool must not error, got %v", err)
	}
	if warn == "" {
		t.Fatalf("expected a warning for a missing pool")
	}
	if sel["nodePool"] != "payments" {
		t.Fatalf("expected synthetic nodeSelector nodePool=payments, got %v", sel)
	}
	if tol != nil {
		t.Fatalf("expected no tolerations for a missing pool, got %v", tol)
	}
}

// A resolved pool applies its own selector + a toleration per taint, no warning.
func TestResolveNodePoolFound(t *testing.T) {
	s := nodePoolScheme(t)
	pool := &deployv1alpha1.Pool{}
	pool.Name = "payments"
	pool.Spec.NodeSelector = map[string]string{"nodePool": "payments"}
	pool.Spec.Taints = []corev1.Taint{{Key: "nodePool", Value: "payments", Effect: corev1.TaintEffectNoSchedule}}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(pool).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}

	bs := &deployv1alpha1.BirService{}
	bs.Spec.NodePool = "payments"

	sel, tol, warn, err := r.resolveNodePool(context.Background(), bs)
	if err != nil || warn != "" {
		t.Fatalf("resolved pool: unexpected warn=%q err=%v", warn, err)
	}
	if sel["nodePool"] != "payments" {
		t.Fatalf("expected pool's nodeSelector, got %v", sel)
	}
	if len(tol) != 1 || tol[0].Key != "nodePool" || tol[0].Operator != corev1.TolerationOpEqual {
		t.Fatalf("expected one Equal toleration for the taint, got %v", tol)
	}
}

// An empty nodePool pins nothing (default scheduling), no warning.
func TestResolveNodePoolEmpty(t *testing.T) {
	s := nodePoolScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}

	sel, tol, warn, err := r.resolveNodePool(context.Background(), &deployv1alpha1.BirService{})
	if err != nil || warn != "" || sel != nil || tol != nil {
		t.Fatalf("empty nodePool should yield nils, got sel=%v tol=%v warn=%q err=%v", sel, tol, warn, err)
	}
}
