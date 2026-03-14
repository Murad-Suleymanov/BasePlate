// Code generated manually for MVP (normally via controller-gen). DO NOT EDIT lightly.

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *BirServiceSpec) DeepCopyInto(out *BirServiceSpec) {
	*out = *in
	if in.Replicas != nil {
		in, out := &in.Replicas, &out.Replicas
		*out = new(int32)
		**out = **in
	}
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
	if in.InjectPipeline != nil {
		in, out := &in.InjectPipeline, &out.InjectPipeline
		*out = new(bool)
		**out = **in
	}
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
