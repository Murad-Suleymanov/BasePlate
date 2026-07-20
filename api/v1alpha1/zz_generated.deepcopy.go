// Code generated manually for MVP (normally via controller-gen). DO NOT EDIT lightly.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *BirServiceSpec) DeepCopyInto(out *BirServiceSpec) {
	*out = *in
	if in.Hostnames != nil {
		in, out := &in.Hostnames, &out.Hostnames
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.Replicas != nil {
		in, out := &in.Replicas, &out.Replicas
		*out = new(int32)
		**out = **in
	}
	if in.HPA != nil {
		in, out := &in.HPA, &out.HPA
		*out = new(HPASpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Port != nil {
		in, out := &in.Port, &out.Port
		*out = new(int32)
		**out = **in
	}
	if in.ContainerPort != nil {
		in, out := &in.ContainerPort, &out.ContainerPort
		*out = new(int32)
		**out = **in
	}
	if in.Expose != nil {
		in, out := &in.Expose, &out.Expose
		*out = new(bool)
		**out = **in
	}
	if in.Metrics != nil {
		in, out := &in.Metrics, &out.Metrics
		*out = new(MetricsSpec)
		**out = **in
	}
	if in.Resources != nil {
		in, out := &in.Resources, &out.Resources
		*out = new(ResourceConfigSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.InjectPipeline != nil {
		in, out := &in.InjectPipeline, &out.InjectPipeline
		*out = new(bool)
		**out = **in
	}
	if in.Traffic != nil {
		in, out := &in.Traffic, &out.Traffic
		*out = new(TrafficSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.ReadinessProbe != nil {
		in, out := &in.ReadinessProbe, &out.ReadinessProbe
		*out = new(ProbeSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.LivenessProbe != nil {
		in, out := &in.LivenessProbe, &out.LivenessProbe
		*out = new(ProbeSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Singleton != nil {
		in, out := &in.Singleton, &out.Singleton
		*out = new(bool)
		**out = **in
	}
	if in.MaxDown != nil {
		in, out := &in.MaxDown, &out.MaxDown
		*out = new(int32)
		**out = **in
	}
	if in.Shutdown != nil {
		in, out := &in.Shutdown, &out.Shutdown
		*out = new(ShutdownSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Route != nil {
		in, out := &in.Route, &out.Route
		*out = new(RouteSpec)
		(*in).DeepCopyInto(*out)
	}
	// Canary was missing here: *out = *in only copies the POINTER, so a "deep" copy
	// shared one CanarySpec with the original. controller-runtime hands every
	// reconcile a DeepCopy precisely so the informer cache cannot be mutated from
	// under it, and a shared pointer defeats that.
	if in.Canary != nil {
		in, out := &in.Canary, &out.Canary
		*out = new(CanarySpec)
		(*in).DeepCopyInto(*out)
	}
	if in.Rollout != nil {
		in, out := &in.Rollout, &out.Rollout
		*out = new(RolloutSpec)
		(*in).DeepCopyInto(*out)
	}
}

func (in *CanarySpec) DeepCopyInto(out *CanarySpec) {
	*out = *in
	if in.Weight != nil {
		in, out := &in.Weight, &out.Weight
		*out = new(int32)
		**out = **in
	}
}

func (in *CanarySpec) DeepCopy() *CanarySpec {
	if in == nil {
		return nil
	}
	out := new(CanarySpec)
	in.DeepCopyInto(out)
	return out
}

func (in *RolloutSpec) DeepCopyInto(out *RolloutSpec) {
	*out = *in
	if in.Steps != nil {
		in, out := &in.Steps, &out.Steps
		*out = make([]int32, len(*in))
		copy(*out, *in)
	}
}

func (in *RolloutSpec) DeepCopy() *RolloutSpec {
	if in == nil {
		return nil
	}
	out := new(RolloutSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *RolloutStatus) DeepCopyInto(out *RolloutStatus) {
	*out = *in
}

func (in *RolloutStatus) DeepCopy() *RolloutStatus {
	if in == nil {
		return nil
	}
	out := new(RolloutStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *RouteSpec) DeepCopyInto(out *RouteSpec) {
	*out = *in
	if in.Entries != nil {
		in, out := &in.Entries, &out.Entries
		*out = make([]RouteEntry, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.Backends != nil {
		in, out := &in.Backends, &out.Backends
		*out = make([]RouteBackend, len(*in))
		copy(*out, *in)
	}
}

func (in *RouteSpec) DeepCopy() *RouteSpec {
	if in == nil {
		return nil
	}
	out := new(RouteSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *RouteBackend) DeepCopyInto(out *RouteBackend) {
	*out = *in
}

func (in *RouteBackend) DeepCopy() *RouteBackend {
	if in == nil {
		return nil
	}
	out := new(RouteBackend)
	in.DeepCopyInto(out)
	return out
}

func (in *RouteEntry) DeepCopyInto(out *RouteEntry) {
	*out = *in
	if in.Retries != nil {
		in, out := &in.Retries, &out.Retries
		*out = new(int32)
		**out = **in
	}
}

func (in *RouteEntry) DeepCopy() *RouteEntry {
	if in == nil {
		return nil
	}
	out := new(RouteEntry)
	in.DeepCopyInto(out)
	return out
}

func (in *ShutdownSpec) DeepCopyInto(out *ShutdownSpec) {
	*out = *in
	if in.PreStopSleepSeconds != nil {
		in, out := &in.PreStopSleepSeconds, &out.PreStopSleepSeconds
		*out = new(int32)
		**out = **in
	}
	if in.DrainBufferSeconds != nil {
		in, out := &in.DrainBufferSeconds, &out.DrainBufferSeconds
		*out = new(int32)
		**out = **in
	}
}

func (in *ShutdownSpec) DeepCopy() *ShutdownSpec {
	if in == nil {
		return nil
	}
	out := new(ShutdownSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *TrafficSpec) DeepCopyInto(out *TrafficSpec) {
	*out = *in
	if in.RateLimit != nil {
		in, out := &in.RateLimit, &out.RateLimit
		*out = new(RateLimitSpec)
		(*in).DeepCopyInto(*out)
	}
	if in.EjectUnhealthy != nil {
		in, out := &in.EjectUnhealthy, &out.EjectUnhealthy
		*out = new(bool)
		**out = **in
	}
	if in.LatencyAware != nil {
		in, out := &in.LatencyAware, &out.LatencyAware
		*out = new(bool)
		**out = **in
	}
	if in.AutoRollback != nil {
		in, out := &in.AutoRollback, &out.AutoRollback
		*out = new(AutoRollbackSpec)
		(*in).DeepCopyInto(*out)
	}
}

func (in *TrafficSpec) DeepCopy() *TrafficSpec {
	if in == nil {
		return nil
	}
	out := new(TrafficSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *AutoRollbackSpec) DeepCopyInto(out *AutoRollbackSpec) {
	*out = *in
	if in.MinRequests != nil {
		in, out := &in.MinRequests, &out.MinRequests
		*out = new(int32)
		**out = **in
	}
	if in.LatencyP99Ms != nil {
		in, out := &in.LatencyP99Ms, &out.LatencyP99Ms
		*out = new(int32)
		**out = **in
	}
}

func (in *AutoRollbackSpec) DeepCopy() *AutoRollbackSpec {
	if in == nil {
		return nil
	}
	out := new(AutoRollbackSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *RateLimitSpec) DeepCopyInto(out *RateLimitSpec) {
	*out = *in
	if in.Local != nil {
		in, out := &in.Local, &out.Local
		*out = new(LocalRateLimitSpec)
		(*in).DeepCopyInto(*out)
	}
}

func (in *RateLimitSpec) DeepCopy() *RateLimitSpec {
	if in == nil {
		return nil
	}
	out := new(RateLimitSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *LocalRateLimitSpec) DeepCopyInto(out *LocalRateLimitSpec) {
	*out = *in
	if in.Burst != nil {
		in, out := &in.Burst, &out.Burst
		*out = new(int32)
		**out = **in
	}
}

func (in *LocalRateLimitSpec) DeepCopy() *LocalRateLimitSpec {
	if in == nil {
		return nil
	}
	out := new(LocalRateLimitSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ProbeSpec) DeepCopyInto(out *ProbeSpec) {
	*out = *in
	if in.Port != nil {
		in, out := &in.Port, &out.Port
		*out = new(int32)
		**out = **in
	}
}

func (in *ProbeSpec) DeepCopy() *ProbeSpec {
	if in == nil {
		return nil
	}
	out := new(ProbeSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *MetricsSpec) DeepCopyInto(out *MetricsSpec) {
	*out = *in
}

func (in *MetricsSpec) DeepCopy() *MetricsSpec {
	if in == nil {
		return nil
	}
	out := new(MetricsSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *HPASpec) DeepCopyInto(out *HPASpec) {
	*out = *in
	if in.MinReplicas != nil {
		in, out := &in.MinReplicas, &out.MinReplicas
		*out = new(int32)
		**out = **in
	}
	if in.MaxReplicas != nil {
		in, out := &in.MaxReplicas, &out.MaxReplicas
		*out = new(int32)
		**out = **in
	}
	if in.TargetRPS != nil {
		in, out := &in.TargetRPS, &out.TargetRPS
		*out = new(int32)
		**out = **in
	}
}

func (in *HPASpec) DeepCopy() *HPASpec {
	if in == nil {
		return nil
	}
	out := new(HPASpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ResourceValues) DeepCopyInto(out *ResourceValues) {
	*out = *in
}

func (in *ResourceValues) DeepCopy() *ResourceValues {
	if in == nil {
		return nil
	}
	out := new(ResourceValues)
	in.DeepCopyInto(out)
	return out
}

func (in *ResourceConfigSpec) DeepCopyInto(out *ResourceConfigSpec) {
	*out = *in
	if in.Requests != nil {
		in, out := &in.Requests, &out.Requests
		*out = new(ResourceValues)
		**out = **in
	}
	if in.Limits != nil {
		in, out := &in.Limits, &out.Limits
		*out = new(ResourceValues)
		**out = **in
	}
}

func (in *ResourceConfigSpec) DeepCopy() *ResourceConfigSpec {
	if in == nil {
		return nil
	}
	out := new(ResourceConfigSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *BirServiceSpec) DeepCopy() *BirServiceSpec {
	if in == nil {
		return nil
	}
	out := new(BirServiceSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *BirServiceStatus) DeepCopyInto(out *BirServiceStatus) {
	*out = *in
	if in.Rollout != nil {
		in, out := &in.Rollout, &out.Rollout
		*out = new(RolloutStatus)
		(*in).DeepCopyInto(*out)
	}
}

func (in *BirServiceStatus) DeepCopy() *BirServiceStatus {
	if in == nil {
		return nil
	}
	out := new(BirServiceStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *BirService) DeepCopyInto(out *BirService) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

func (in *BirService) DeepCopy() *BirService {
	if in == nil {
		return nil
	}
	out := new(BirService)
	in.DeepCopyInto(out)
	return out
}

func (in *BirService) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *BirServiceList) DeepCopyInto(out *BirServiceList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]BirService, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *BirServiceList) DeepCopy() *BirServiceList {
	if in == nil {
		return nil
	}
	out := new(BirServiceList)
	in.DeepCopyInto(out)
	return out
}

func (in *BirServiceList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PoolSpec) DeepCopyInto(out *PoolSpec) {
	*out = *in
	if in.NodeSelector != nil {
		in, out := &in.NodeSelector, &out.NodeSelector
		*out = make(map[string]string, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
	if in.Taints != nil {
		in, out := &in.Taints, &out.Taints
		*out = make([]corev1.Taint, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *PoolSpec) DeepCopy() *PoolSpec {
	if in == nil {
		return nil
	}
	out := new(PoolSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *Pool) DeepCopyInto(out *Pool) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

func (in *Pool) DeepCopy() *Pool {
	if in == nil {
		return nil
	}
	out := new(Pool)
	in.DeepCopyInto(out)
	return out
}

func (in *Pool) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PoolList) DeepCopyInto(out *PoolList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Pool, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *PoolList) DeepCopy() *PoolList {
	if in == nil {
		return nil
	}
	out := new(PoolList)
	in.DeepCopyInto(out)
	return out
}

func (in *PoolList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
