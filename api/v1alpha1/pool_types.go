package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PoolSpec describes a node pool. Workloads that select the pool (via
// BirService.spec.nodePool) get NodeSelector pinned to the pool's nodes plus a
// toleration for each of the pool's Taints, so only opted-in workloads land
// there. A pool with no taints (e.g. "default") simply pins via NodeSelector,
// or pins nothing at all when NodeSelector is also empty.
type PoolSpec struct {
	// NodeSelector pins pods to this pool's nodes, e.g. {nodePool: payments}.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Taints lists the taints the pool's nodes carry. The operator injects a
	// matching toleration onto every pod that selects this pool.
	Taints []corev1.Taint `json:"taints,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type Pool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PoolSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true
type PoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Pool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Pool{}, &PoolList{})
}
