package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *InferenceEndpoint) DeepCopyInto(out *InferenceEndpoint) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *InferenceEndpoint) DeepCopy() *InferenceEndpoint {
	if in == nil {
		return nil
	}
	out := new(InferenceEndpoint)
	in.DeepCopyInto(out)
	return out
}

func (in *InferenceEndpoint) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *InferenceEndpointList) DeepCopyInto(out *InferenceEndpointList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]InferenceEndpoint, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *InferenceEndpointList) DeepCopy() *InferenceEndpointList {
	if in == nil {
		return nil
	}
	out := new(InferenceEndpointList)
	in.DeepCopyInto(out)
	return out
}

func (in *InferenceEndpointList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *InferenceEndpointStatus) DeepCopyInto(out *InferenceEndpointStatus) {
	*out = *in
	if in.LastActivityTime != nil {
		out.LastActivityTime = in.LastActivityTime.DeepCopy()
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

func (in *InferenceEndpointStatus) DeepCopy() *InferenceEndpointStatus {
	if in == nil {
		return nil
	}
	out := new(InferenceEndpointStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *ModelCache) DeepCopyInto(out *ModelCache) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ModelCache) DeepCopy() *ModelCache {
	if in == nil {
		return nil
	}
	out := new(ModelCache)
	in.DeepCopyInto(out)
	return out
}

func (in *ModelCache) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *ModelCacheList) DeepCopyInto(out *ModelCacheList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ModelCache, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ModelCacheList) DeepCopy() *ModelCacheList {
	if in == nil {
		return nil
	}
	out := new(ModelCacheList)
	in.DeepCopyInto(out)
	return out
}

func (in *ModelCacheList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *ModelCacheSpec) DeepCopyInto(out *ModelCacheSpec) {
	*out = *in
	if in.NodePoolSelector != nil {
		out.NodePoolSelector = make(map[string]string, len(in.NodePoolSelector))
		for key, value := range in.NodePoolSelector {
			out.NodePoolSelector[key] = value
		}
	}
}

func (in *ModelCacheStatus) DeepCopyInto(out *ModelCacheStatus) {
	*out = *in
	if in.Nodes != nil {
		out.Nodes = make([]ModelCacheNodeStatus, len(in.Nodes))
		for i := range in.Nodes {
			in.Nodes[i].DeepCopyInto(&out.Nodes[i])
		}
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

func (in *ModelCacheStatus) DeepCopy() *ModelCacheStatus {
	if in == nil {
		return nil
	}
	out := new(ModelCacheStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *ModelCacheNodeStatus) DeepCopyInto(out *ModelCacheNodeStatus) {
	*out = *in
	if in.MaterializedAt != nil {
		out.MaterializedAt = in.MaterializedAt.DeepCopy()
	}
	if in.LastUsedAt != nil {
		out.LastUsedAt = in.LastUsedAt.DeepCopy()
	}
}
