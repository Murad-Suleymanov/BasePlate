package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BirServiceSpec defines the desired state of BirService
type BirServiceSpec struct {
	// Image is a fully qualified container image reference (e.g. ealen/echo-server:0.9.2).
	Image string `json:"image,omitempty"`

	// Repo is either a container image repository (ghcr.io/acme/hello) or a
	// Git URL (https://github.com/user/app) containing a Dockerfile to build.
	Repo string `json:"repo,omitempty"`

	// InjectPipeline, when true, instructs the platform to add a GitHub Actions
	// workflow to the repository (when repo is a GitHub URL). Requires
	// GITHUB_TOKEN with repo write access. Pipeline builds and pushes to the
	// platform registry on every push.
	InjectPipeline *bool `json:"injectPipeline,omitempty"`

	// Tag is an image tag or git ref (branch/tag/commit). Defaults to "latest" for
	// image repos and "main" for git repos.
	Tag string `json:"tag,omitempty"`

	// ImageTag overrides the deployed image tag (e.g. abc1234). For rollback: set to a previous SHA.
	// When empty, uses status.buildTag from the last pipeline build.
	ImageTag string `json:"imageTag,omitempty"`

	// Dockerfile path relative to repo root (default: "Dockerfile"). Only used for git repos.
	Dockerfile string `json:"dockerfile,omitempty"`

	// Replicas is desired pod replicas (default 1). Ignored when HPA is used (spec.hpa.minReplicas/maxReplicas).
	Replicas *int32 `json:"replicas,omitempty"`

	// HPA config. When both set, Replicas is ignored and HPA controls scaling.
	HPA *HPASpec `json:"hpa,omitempty"`

	// Port is the Service port (default 8080).
	Port *int32 `json:"port,omitempty"`

	// ContainerPort is the container port (default = Port).
	ContainerPort *int32 `json:"containerPort,omitempty"`

	// Hostname is the public DNS name for external access.
	// If empty, auto-generated as <name>-<namespace>.<baseDomain>.
	Hostname string `json:"hostname,omitempty"`

	// Hostnames lists additional public DNS names (aliases) served by the same workload.
	// The HTTPRoute serves the primary hostname (Hostname or the auto-generated name)
	// plus every entry here, and external-dns creates a record for each. Use to expose
	// one instance under several names; all share the same pods and rate limit.
	Hostnames []string `json:"hostnames,omitempty"`

	// Expose controls external access via HTTPRoute and DNS. Default true.
	// If false, service is internal-only (ClusterIP).
	Expose *bool `json:"expose,omitempty"`

	// Metrics configures Prometheus ServiceMonitor for custom app metrics.
	// Omit or false: no ServiceMonitor. true: enabled with path /metrics.
	// Object form: { enabled: true, path: /actuator/prometheus }
	Metrics *MetricsSpec `json:"metrics,omitempty"`

	// Resources configures container requests/limits.
	// Defaults when omitted: requests(cpu=75m,memory=200Mi), limits are 2x requests.
	Resources *ResourceConfigSpec `json:"resources,omitempty"`

	// NodePool pins this workload to a named node pool. The operator resolves it
	// to a Pool resource and injects the pool's nodeSelector + matching tolerations.
	// Empty schedules onto the default (untainted) nodes. An unknown name blocks the
	// deploy rather than silently falling back to default.
	NodePool string `json:"nodePool,omitempty"`

	// Traffic configures service-mesh traffic policies. If non-nil, the operator treats the
	// workload as mesh-enabled (default provider: Istio): namespace istio-injection label, and
	// optional Envoy local rate limit. Omit entirely if the app should not use the mesh.
	Traffic *TrafficSpec `json:"traffic,omitempty"`

	// Canary enables a parallel canary deployment with weighted HTTPRoute traffic splitting.
	// Set enabled: false or remove this field to tear down the canary infra.
	Canary *CanarySpec `json:"canary,omitempty"`

	// ReadinessProbe configures the HTTP readiness probe. Port defaults to the container port.
	// Operator applies default timings (initialDelay=5s, period=5s, failureThreshold=3).
	ReadinessProbe *ProbeSpec `json:"readinessProbe,omitempty"`

	// LivenessProbe configures the HTTP liveness probe. Port defaults to the container port.
	// Operator applies default timings (initialDelay=15s, period=10s, failureThreshold=3).
	LivenessProbe *ProbeSpec `json:"livenessProbe,omitempty"`

	// Singleton declares that the app cannot run two versions concurrently
	// (in-memory state, leader-elected job, exclusive resource lock).
	// When true: deploys stop all old pods before starting new ones (brief downtime),
	// PDB is skipped, and replicas/HPA are still honored but typically set to 1.
	// When false or omitted (default): zero-downtime rolling deploy with platform-managed
	// surge/unavailable budgets.
	Singleton *bool `json:"singleton,omitempty"`

	// MaxDown is the maximum number of pods that may be down simultaneously during
	// voluntary disruptions (node drains, cluster upgrades, autoscaler removing nodes).
	// Maps to the PodDisruptionBudget maxUnavailable count.
	// When omitted, defaults to floor(N/2) where N is replicas or hpa.minReplicas
	// (half the fleet may go, half stays). Set lower (e.g. 1) for latency-critical apps
	// where losing more than one pod degrades the SLA. Set 0 to forbid any voluntary
	// disruption (warning: blocks node drains until pods reschedule).
	// Ignored when singleton: true or effective replicas < 2.
	MaxDown *int32 `json:"maxDown,omitempty"`

	// Shutdown tunes graceful termination. preStopSleepSeconds drains endpoints
	// before SIGTERM; terminationGracePeriodSeconds is auto-derived as preStopSleep + 5.
	Shutdown *ShutdownSpec `json:"shutdown,omitempty"`

	// Route is the chart-resolved routing for this instance. The chart looks up the
	// tenant's route name(s) in the per-service routes.yaml catalog for the active
	// cluster and fills this in; tenants never author it directly.
	//
	// Instances that resolve to the same Group share ONE Service (a load-balanced
	// pod pool): every member's pods carry the route-group label and the pool's
	// Service selects that label, so the load balancer spreads requests across all
	// of them. Exactly one member per Group is Primary and owns the Service +
	// HTTPRoutes; the others only contribute pods.
	//
	// Omit entirely for a standalone service (Group defaults to the instance name).
	Route *RouteSpec `json:"route,omitempty"`
}

// RouteSpec is the chart-resolved routing for one BirService instance (pool model).
type RouteSpec struct {
	// Group identifies the pool. All instances with the same Group share one Service
	// whose selector matches the route-group pod label. Defaults to the BirService
	// name (standalone) when no route is referenced.
	Group string `json:"group,omitempty"`

	// Primary marks the single member that owns the pool's Service and HTTPRoutes.
	// Non-primary members only contribute pods (the route-group label); they create
	// no Service or HTTPRoute of their own.
	Primary bool `json:"primary,omitempty"`

	// Entries are the named HTTP front doors for the pool — one HTTPRoute each, with
	// its own hostname/timeout/retries. Set only on the Primary member.
	Entries []RouteEntry `json:"entries,omitempty"`
}

// RouteEntry is one named HTTPRoute (front door) for a pool, resolved from the
// per-service routes.yaml catalog for the active cluster.
type RouteEntry struct {
	// Name is the route name the developer referenced (e.g. "main", "main_slow").
	Name string `json:"name"`
	// Hostname this route is served on. Empty → operator auto-derives
	// <name>-<namespace>.<baseDomain>.
	Hostname string `json:"hostname,omitempty"`
	// Timeout is the per-request timeout (e.g. "15s"). Empty → Gateway default.
	Timeout string `json:"timeout,omitempty"`
	// Retries is the number of HTTP retries. Nil → no retry policy.
	Retries *int32 `json:"retries,omitempty"`
}

// ShutdownSpec configures graceful pod termination. The operator computes
// terminationGracePeriodSeconds as PreStopSleepSeconds + DrainBufferSeconds.
type ShutdownSpec struct {
	// PreStopSleepSeconds is how long the container sleeps in its preStop hook before
	// receiving SIGTERM. Used to drain endpoints from kube-proxy / Istio xDS / Gateways
	// while the app is still alive. Default 15.
	PreStopSleepSeconds *int32 `json:"preStopSleepSeconds,omitempty"`
	// DrainBufferSeconds is the post-SIGTERM budget for the app to finish in-flight
	// requests before SIGKILL. terminationGracePeriodSeconds = PreStopSleepSeconds + DrainBufferSeconds.
	// Default 5. Increase for apps with long-running requests (uploads, streaming, batch).
	DrainBufferSeconds *int32 `json:"drainBufferSeconds,omitempty"`
}

// TrafficSpec groups mesh traffic settings. Presence of spec.traffic means mesh intent;
// use provider to select the mesh (only "istio" today).
type TrafficSpec struct {
	// Provider is the mesh implementation. Empty or "istio" uses Istio (Envoy sidecar, EnvoyFilter).
	// Sidecar injection is always enabled for Istio (namespace label istio-injection=enabled).
	// Other values are reserved; the operator skips Istio resources if set to a non-istio value.
	Provider string `json:"provider,omitempty"`

	// RateLimit configures request rate limiting (Envoy local rate limit for mode=local).
	RateLimit *RateLimitSpec `json:"rateLimit,omitempty"`

	// EjectUnhealthy controls whether failing pods are temporarily removed from the
	// load-balancing pool by the waypoint Envoy (Istio outlier detection). Default true.
	// Two complementary mechanisms (both platform-managed, no per-service knobs):
	//   - Statistical: eject pods whose success rate is ≥1.9σ below the fleet mean
	//     over a 30s window. RPS-adaptive — fires only when ≥2 pods each have ≥100
	//     requests in the window (~3 req/s minimum per pod).
	//   - Hard-failure backstop: 3 consecutive 502/503/504 responses eject the pod
	//     immediately; works for any RPS and single-pod deployments.
	// Max 50% of pods ejected; minHealthPercent: 0 keeps panic mode off.
	// Set false for workloads that legitimately return 5xx (webhook endpoints,
	// batch processors with retries).
	EjectUnhealthy *bool `json:"ejectUnhealthy,omitempty"`

	// LatencyAware switches the load-balancer from round-robin (default) to least-request:
	// each new request goes to the pod with the fewest in-flight requests. Useful when
	// request latency is heterogeneous (some hit cache, some hit DB) — slow pods
	// automatically get less traffic. For uniform request latency, round-robin is
	// usually better. Verify with latency histograms (P50 vs P99) before enabling.
	LatencyAware *bool `json:"latencyAware,omitempty"`
}

// RateLimitSpec configures rate limiting for the BirService workload.
type RateLimitSpec struct {
	// Enabled turns on Istio Envoy local rate limiting for this workload.
	Enabled bool `json:"enabled"`
	// Mode is "local" (Envoy sidecar token bucket) or "global" (not implemented yet).
	Mode string `json:"mode,omitempty"`
	// Local configures per-pod rate limit (Envoy http local rate limit filter).
	Local *LocalRateLimitSpec `json:"local,omitempty"`
}

// LocalRateLimitSpec configures Envoy local rate limiting (inbound).
type LocalRateLimitSpec struct {
	// RequestsPerSecond is the steady-state requests allowed per second per pod.
	RequestsPerSecond int32 `json:"requestsPerSecond"`
	// Burst is extra tokens added to the bucket capacity (max_tokens = requestsPerSecond + burst).
	// +optional
	Burst *int32 `json:"burst,omitempty"`
}

type HPASpec struct {
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// ScaleType selects the single signal the HPA scales on. The developer only
	// names the signal; the operator resolves the underlying metric source
	// (resource / external / pods) and target shape:
	//   cpu    — average CPU utilization across pods (Target is a % of the CPU
	//            request, default 80). Resource metric, served by metrics-server.
	//   memory — average memory utilization across pods (Target is a % of the
	//            memory request, default 80). Resource metric, metrics-server.
	//   rps    — Istio requests-per-second per pod (Target is req/s). External
	//            metric istio_requests_per_second; requires spec.traffic (waypoint).
	//   worker — worker-pool saturation per pod (Target is a utilization %). Pods
	//            metric app_worker_utilization = 100*busy/max workers, normalized
	//            across runtimes; requires spec.metrics (ServiceMonitor).
	// Empty → CPU at 80% (the platform default).
	ScaleType string `json:"scaleType,omitempty"`

	// Target is the per-pod threshold for ScaleType. For cpu/memory/worker it is a
	// utilization percentage (defaults to 80 when omitted); for rps it is an
	// absolute requests/sec per pod and is required (no default).
	Target int32 `json:"target,omitempty"`

	// TargetRPS is the legacy RPS knob, kept for backward compatibility. It is
	// equivalent to scaleType: rps with target: N. Prefer scaleType for new
	// configs; if scaleType is already set, TargetRPS is ignored. Like the rps
	// signal it requires spec.traffic. Ignored when spec.replicas is set.
	TargetRPS *int32 `json:"targetRPS,omitempty"`

	// Window is the rate-averaging window for the rps signal (e.g. 1m, 2m, 5m).
	// A larger window smooths spikes (stabler, slower); a smaller one reacts
	// faster (noisier). Any valid Prometheus duration is accepted — the operator
	// creates the backing recording rule on demand and reuses it across services.
	// Empty defaults to 1m. Ignored for cpu/memory/worker (no rate window).
	Window string `json:"window,omitempty"`
}

type ResourceConfigSpec struct {
	Requests *ResourceValues `json:"requests,omitempty"`
	Limits   *ResourceValues `json:"limits,omitempty"`
}

type ResourceValues struct {
	Memory string `json:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty"`
}

// CanarySpec configures a parallel canary deployment with weighted HTTPRoute traffic splitting.
type CanarySpec struct {
	// Enabled activates the canary deployment. Set false or remove to tear down canary infra.
	Enabled bool `json:"enabled"`
	// Weight is the percentage of traffic routed to canary (0-100). Default 10.
	Weight *int32 `json:"weight,omitempty"`
	// Image is the fully-qualified canary container image. If empty, derived from spec.image/repo + Tag.
	Image string `json:"image,omitempty"`
	// Tag overrides the image tag for the canary deployment (e.g. "v2.0.0-rc1").
	Tag string `json:"tag,omitempty"`
}

// ProbeSpec configures an HTTP liveness or readiness probe.
// Port defaults to the container port when omitted.
type ProbeSpec struct {
	Path string `json:"path"`
	Port *int32 `json:"port,omitempty"`
}

// MetricsSpec configures Prometheus scraping for custom application metrics.
type MetricsSpec struct {
	// Enabled turns on ServiceMonitor creation. Default path /metrics.
	Enabled bool `json:"enabled"`
	// Path is the metrics HTTP path (default /metrics).
	Path string `json:"path,omitempty"`
}

// BirServiceStatus defines the observed state of BirService
type BirServiceStatus struct {
	AvailableReplicas int32  `json:"availableReplicas,omitempty"`
	BuildImage        string `json:"buildImage,omitempty"`
	BuildStatus       string `json:"buildStatus,omitempty"`
	BuildTag          string `json:"buildTag,omitempty"`
	LastRebuild       string `json:"lastRebuild,omitempty"`
	// StableTag is the image tag locked in as stable when canary was first enabled.
	// Cleared on promotion (canary.enabled → false).
	StableTag   string `json:"stableTag,omitempty"`
	// CanaryImage is the full image URL of the active canary deployment (display only).
	CanaryImage string `json:"canaryImage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type BirService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BirServiceSpec   `json:"spec,omitempty"`
	Status BirServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type BirServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BirService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BirService{}, &BirServiceList{})
}
