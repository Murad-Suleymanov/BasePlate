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
}

type HPASpec struct {
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`
}

type ResourceConfigSpec struct {
	Requests *ResourceValues `json:"requests,omitempty"`
	Limits   *ResourceValues `json:"limits,omitempty"`
}

type ResourceValues struct {
	Memory string `json:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty"`
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
