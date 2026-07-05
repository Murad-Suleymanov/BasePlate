package controller

import (
	"context"
	"testing"
	"time"

	deployv1alpha1 "easy-deploy/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func depKeyFor(ns string) types.NamespacedName {
	return types.NamespacedName{Name: "app-deploy", Namespace: ns}
}

func TestTagFromImage(t *testing.T) {
	cases := map[string]string{
		"registry.registry.svc.cluster.local:5000/hello:abc123": "abc123",
		"ghcr.io/org/app:v1.2.3":                                "v1.2.3",
		"nginx:latest":                                          "latest",
		"nginx":                                                 "",
		"host:5000/app":                                         "",
	}
	for in, want := range cases {
		if got := tagFromImage(in); got != want {
			t.Errorf("tagFromImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSwapImageTag(t *testing.T) {
	cases := []struct{ in, tag, want string }{
		{"registry:5000/hello:bad", "good", "registry:5000/hello:good"},
		{"ghcr.io/org/app:v2", "v1", "ghcr.io/org/app:v1"},
		{"nginx:latest", "1.25", "nginx:1.25"},
	}
	for _, c := range cases {
		if got := swapImageTag(c.in, c.tag); got != c.want {
			t.Errorf("swapImageTag(%q,%q) = %q, want %q", c.in, c.tag, got, c.want)
		}
	}
}

func TestPodCrashingAndReady(t *testing.T) {
	crash := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
		},
	}}
	if !podCrashing(crash) {
		t.Fatal("expected CrashLoopBackOff pod to be crashing")
	}
	restarts := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{RestartCount: 3}},
	}}
	if !podCrashing(restarts) {
		t.Fatal("expected 3-restart pod to be crashing")
	}
	// Backed-off start failures (can't pull image, bad config) also count as failing.
	for _, reason := range []string{"ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError"} {
		p := &corev1.Pod{Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}},
			},
		}}
		if !podCrashing(p) {
			t.Fatalf("expected %s pod to be failing", reason)
		}
	}
	// A failing init container blocks the pod too.
	initFail := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
		},
	}}
	if !podCrashing(initFail) {
		t.Fatal("expected failing init container to mark pod as failing")
	}
	// First-attempt ErrImagePull is transient — must NOT trip a rollback.
	transient := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}},
		},
	}}
	if podCrashing(transient) {
		t.Fatal("ErrImagePull (first attempt) must not count as failing")
	}
	ready := &corev1.Pod{Status: corev1.PodStatus{
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}}
	if podCrashing(ready) || !podReady(ready) {
		t.Fatal("expected a ready, non-crashing pod")
	}
}

func rollbackScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	if err := deployv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("deploy scheme: %v", err)
	}
	return s
}

func tagPod(ns, app, tag, name string, ready, crashing bool) *corev1.Pod {
	p := &corev1.Pod{}
	p.Name = name
	p.Namespace = ns
	p.Labels = map[string]string{"app.kubernetes.io/name": app, labelBuildTag: tag}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	if crashing {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
		}
	}
	return p
}

func newDep(ns string) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app-deploy", Namespace: ns, Generation: 1}}
	d.Status = appsv1.DeploymentStatus{
		ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 0,
	}
	return d
}

// A crash-looping new tag with a known healthy tag quarantines the bad tag.
func TestEvaluateAutoRollbackQuarantines(t *testing.T) {
	ns := "tenant-a"
	s := rollbackScheme(t)
	dep := newDep(ns)
	dep.Status.AvailableReplicas = 0
	dep.Status.UnavailableReplicas = 1
	pod := tagPod(ns, "app", "v2", "app-xyz", false, true)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, pod).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}

	bs := &deployv1alpha1.BirService{}
	bs.Name = "app"
	bs.Namespace = ns

	requeue, err := r.evaluateAutoRollback(context.Background(), bs, "app-deploy", "v2", "v2", "v1", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if requeue == 0 {
		t.Fatal("expected a requeue after quarantine")
	}
	var got appsv1.Deployment
	_ = cl.Get(context.Background(), depKeyFor(ns), &got)
	if got.Annotations[annotRolledBackTag] != "v2" {
		t.Fatalf("expected rolled-back-tag=v2, got %v", got.Annotations)
	}
}

// A crash with no prior healthy tag must NOT quarantine (nothing to fall back to).
func TestEvaluateAutoRollbackNoFallback(t *testing.T) {
	ns := "tenant-a"
	s := rollbackScheme(t)
	dep := newDep(ns)
	dep.Status.AvailableReplicas = 0
	dep.Status.UnavailableReplicas = 1
	pod := tagPod(ns, "app", "v1", "app-xyz", false, true)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, pod).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}

	bs := &deployv1alpha1.BirService{}
	bs.Name = "app"
	bs.Namespace = ns

	if _, err := r.evaluateAutoRollback(context.Background(), bs, "app-deploy", "v1", "v1", "", ""); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var got appsv1.Deployment
	_ = cl.Get(context.Background(), depKeyFor(ns), &got)
	if _, ok := got.Annotations[annotRolledBackTag]; ok {
		t.Fatalf("must not quarantine without a healthy fallback, got %v", got.Annotations)
	}
}

// With no recorded healthy-tag, a Ready pod on a different tag is used as the
// bootstrap rollback target (covers services healthy before auto-rollback existed).
func TestEvaluateAutoRollbackBootstraps(t *testing.T) {
	ns := "tenant-a"
	s := rollbackScheme(t)
	dep := newDep(ns)
	dep.Status.AvailableReplicas = 1
	dep.Status.UnavailableReplicas = 1
	bad := tagPod(ns, "app", "v2", "app-new", false, true) // new version crash-looping
	good := &corev1.Pod{}                                  // old version still Ready
	good.Name = "app-old"
	good.Namespace = ns
	good.Labels = map[string]string{"app.kubernetes.io/name": "app"}
	good.Spec.Containers = []corev1.Container{{Name: "app", Image: "registry.registry.svc.cluster.local:5000/app:v1"}}
	good.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, bad, good).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}

	bs := &deployv1alpha1.BirService{}
	bs.Name = "app"
	bs.Namespace = ns

	requeue, err := r.evaluateAutoRollback(context.Background(), bs, "app-deploy", "v2", "v2", "", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if requeue == 0 {
		t.Fatal("expected requeue after bootstrap quarantine")
	}
	var got appsv1.Deployment
	_ = cl.Get(context.Background(), depKeyFor(ns), &got)
	if got.Annotations[annotRolledBackTag] != "v2" || got.Annotations[annotHealthyTag] != "v1" {
		t.Fatalf("expected rolled-back=v2 healthy=v1 (bootstrapped), got %v", got.Annotations)
	}
}

// With several Ready pods on different tags, bootstrap picks the newest (most recent
// known-good), not an arbitrary older one.
func TestBootstrapHealthyTagPicksNewest(t *testing.T) {
	ns := "tenant-a"
	s := rollbackScheme(t)
	img := func(tag string) string { return "registry.registry.svc.cluster.local:5000/app:" + tag }
	readyPod := func(name, tag string, ageMin int) *corev1.Pod {
		p := &corev1.Pod{}
		p.Name = name
		p.Namespace = ns
		p.Labels = map[string]string{"app.kubernetes.io/name": "app"}
		p.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Duration(-ageMin) * time.Minute))
		p.Spec.Containers = []corev1.Container{{Name: "app", Image: img(tag)}}
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		return p
	}
	old := readyPod("app-old", "v8", 60)   // older healthy version
	newer := readyPod("app-new", "v9", 10) // most recent healthy version
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(old, newer).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}

	bs := &deployv1alpha1.BirService{}
	bs.Name = "app"
	bs.Namespace = ns

	if got := r.bootstrapHealthyTag(context.Background(), bs, "v10"); got != "v9" {
		t.Fatalf("expected newest healthy tag v9, got %q", got)
	}
}

// A fully-rolled-out healthy tag records itself as the healthy rollback target.
func TestEvaluateAutoRollbackMarksHealthy(t *testing.T) {
	ns := "tenant-a"
	s := rollbackScheme(t)
	dep := newDep(ns)
	pod := tagPod(ns, "app", "v2", "app-xyz", true, false)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, pod).Build()
	r := &BirServiceReconciler{Client: cl, Scheme: s}

	bs := &deployv1alpha1.BirService{}
	bs.Name = "app"
	bs.Namespace = ns

	requeue, err := r.evaluateAutoRollback(context.Background(), bs, "app-deploy", "v2", "v2", "v1", "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if requeue != 0 {
		t.Fatalf("healthy rollout should settle (requeue 0), got %v", requeue)
	}
	var got appsv1.Deployment
	_ = cl.Get(context.Background(), depKeyFor(ns), &got)
	if got.Annotations[annotHealthyTag] != "v2" {
		t.Fatalf("expected healthy-tag=v2, got %v", got.Annotations)
	}
}
