package v1alpha1

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type RetentionPolicy string

const (
	RetentionPolicyLRUWithFloor RetentionPolicy = "LRUWithFloor"
)

type ModelCacheNodeState string

const (
	ModelCacheNodeStatePending ModelCacheNodeState = "Pending"
	ModelCacheNodeStateLoading ModelCacheNodeState = "Loading"
	ModelCacheNodeStateReady   ModelCacheNodeState = "Ready"
	ModelCacheNodeStateFailed  ModelCacheNodeState = "Failed"
)

type ModelCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelCacheSpec   `json:"spec,omitempty"`
	Status ModelCacheStatus `json:"status,omitempty"`
}

type ModelCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelCache `json:"items"`
}

type ModelCacheSpec struct {
	ModelID          string            `json:"modelID"`
	Revision         string            `json:"revision"`
	Digest           string            `json:"digest"`
	SizeBytes        int64             `json:"sizeBytes"`
	NodePoolSelector map[string]string `json:"nodePoolSelector,omitempty"`
	RetentionPolicy  RetentionPolicy   `json:"retentionPolicy,omitempty"`
}

type ModelCacheStatus struct {
	ObservedGeneration   int64                  `json:"observedGeneration,omitempty"`
	Nodes                []ModelCacheNodeStatus `json:"nodes,omitempty"`
	ReferencingEndpoints int32                  `json:"referencingEndpoints,omitempty"`
	Conditions           []metav1.Condition     `json:"conditions,omitempty"`
}

type ModelCacheNodeStatus struct {
	Name           string              `json:"name,omitempty"`
	State          ModelCacheNodeState `json:"state,omitempty"`
	ProgressBytes  int64               `json:"progressBytes,omitempty"`
	MaterializedAt *metav1.Time        `json:"materializedAt,omitempty"`
	LastUsedAt     *metav1.Time        `json:"lastUsedAt,omitempty"`
	Message        string              `json:"message,omitempty"`
}

func (m *ModelCache) Default() {
	if m.Spec.RetentionPolicy == "" {
		m.Spec.RetentionPolicy = RetentionPolicyLRUWithFloor
	}
}

func (s *ModelCacheStatus) SetCondition(conditionType string, conditionStatus metav1.ConditionStatus, reason, message string, observedGeneration int64, now time.Time) bool {
	newCondition := metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: metav1.NewTime(now),
	}

	existing := meta.FindStatusCondition(s.Conditions, conditionType)
	if existing != nil {
		if existing.Status == newCondition.Status {
			newCondition.LastTransitionTime = existing.LastTransitionTime
		}
		if existing.Status == newCondition.Status && existing.Reason == newCondition.Reason && existing.Message == newCondition.Message && existing.ObservedGeneration == newCondition.ObservedGeneration {
			return false
		}
	}

	meta.SetStatusCondition(&s.Conditions, newCondition)
	return true
}

func (s *ModelCacheStatus) UpsertNode(name string, state ModelCacheNodeState, progressBytes int64, materializedAt, lastUsedAt *metav1.Time, message string) {
	for i := range s.Nodes {
		if s.Nodes[i].Name == name {
			s.Nodes[i].State = state
			s.Nodes[i].ProgressBytes = progressBytes
			s.Nodes[i].MaterializedAt = materializedAt
			s.Nodes[i].LastUsedAt = lastUsedAt
			s.Nodes[i].Message = message
			return
		}
	}
	s.Nodes = append(s.Nodes, ModelCacheNodeStatus{
		Name:           name,
		State:          state,
		ProgressBytes:  progressBytes,
		MaterializedAt: materializedAt,
		LastUsedAt:     lastUsedAt,
		Message:        message,
	})
}

func (s *ModelCacheStatus) RemoveNode(name string) {
	filtered := s.Nodes[:0]
	for _, node := range s.Nodes {
		if node.Name != name {
			filtered = append(filtered, node)
		}
	}
	s.Nodes = filtered
}

func ReasonFromModelCache(status ModelCacheStatus) string {
	if degraded := meta.FindStatusCondition(status.Conditions, ConditionDegraded); degraded != nil && degraded.Status == metav1.ConditionTrue {
		return degraded.Reason
	}
	if progressing := meta.FindStatusCondition(status.Conditions, ConditionProgressing); progressing != nil && progressing.Status == metav1.ConditionTrue {
		return progressing.Reason
	}
	return ReasonAsExpected
}

func MessageFromModelCache(status ModelCacheStatus) string {
	for _, conditionType := range []string{ConditionDegraded, ConditionProgressing, ConditionReady} {
		if condition := meta.FindStatusCondition(status.Conditions, conditionType); condition != nil && strings.TrimSpace(condition.Message) != "" {
			return condition.Message
		}
	}
	return ""
}
