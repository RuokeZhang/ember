package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInferenceEndpointDefaults(t *testing.T) {
	endpoint := &InferenceEndpoint{}
	endpoint.Default()

	if endpoint.Spec.Profile != ProfileStandard {
		t.Fatalf("expected default profile %q, got %q", ProfileStandard, endpoint.Spec.Profile)
	}
	if endpoint.Spec.Scaling.MaxReplicas != 1 {
		t.Fatalf("expected default maxReplicas 1, got %d", endpoint.Spec.Scaling.MaxReplicas)
	}
	if endpoint.Spec.Scaling.TargetQueueDepth != 4 {
		t.Fatalf("expected default targetQueueDepth 4, got %d", endpoint.Spec.Scaling.TargetQueueDepth)
	}
	if endpoint.Spec.Scaling.IdleTimeoutSeconds != 900 {
		t.Fatalf("expected default idleTimeoutSeconds 900, got %d", endpoint.Spec.Scaling.IdleTimeoutSeconds)
	}
	if endpoint.Spec.Placement.CachePreference != CachePreferencePreferred {
		t.Fatalf("expected default cachePreference %q, got %q", CachePreferencePreferred, endpoint.Spec.Placement.CachePreference)
	}
	if endpoint.Spec.Placement.MaxColdStartFallbackSeconds != 120 {
		t.Fatalf("expected default fallback 120, got %d", endpoint.Spec.Placement.MaxColdStartFallbackSeconds)
	}
}

func TestInferenceEndpointValidationAndImmutability(t *testing.T) {
	valid := validEndpoint()
	if errs := valid.ValidateCreate(); len(errs) != 0 {
		t.Fatalf("expected valid endpoint, got errors: %v", errs)
	}

	invalid := validEndpoint()
	invalid.Spec.Model.ID = "bad-model"
	if reason := ValidationReason(invalid.ValidateCreate()); reason != ReasonInvalidModel {
		t.Fatalf("expected invalid model reason, got %q", reason)
	}

	invalid = validEndpoint()
	invalid.Spec.Scaling.MinReplicas = 2
	invalid.Spec.Scaling.MaxReplicas = 1
	if reason := ValidationReason(invalid.ValidateCreate()); reason != ReasonInvalidScaling {
		t.Fatalf("expected invalid scaling reason, got %q", reason)
	}

	updated := validEndpoint()
	updated.Spec.OwnerID = "usr_other"
	if reason := ValidationReason(updated.ValidateUpdate(valid)); reason != ReasonInvalidOwner {
		t.Fatalf("expected invalid owner reason, got %q", reason)
	}
}

func TestSetConditionIsGenerationAware(t *testing.T) {
	status := &InferenceEndpointStatus{}
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	changed := status.SetCondition(ConditionReady, metav1.ConditionTrue, ReasonEngineServing, "ready", 1, now)
	if !changed {
		t.Fatal("expected first condition set to report changed")
	}
	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(status.Conditions))
	}
	firstTransition := status.Conditions[0].LastTransitionTime

	changed = status.SetCondition(ConditionReady, metav1.ConditionTrue, ReasonEngineServing, "ready", 2, now.Add(time.Minute))
	if !changed {
		t.Fatal("expected observedGeneration change to report changed")
	}
	if status.Conditions[0].ObservedGeneration != 2 {
		t.Fatalf("expected observedGeneration 2, got %d", status.Conditions[0].ObservedGeneration)
	}
	if !status.Conditions[0].LastTransitionTime.Equal(&firstTransition) {
		t.Fatal("expected lastTransitionTime to stay stable when status does not change")
	}

	changed = status.SetCondition(ConditionReady, metav1.ConditionFalse, ReasonRollingOut, "rolling", 2, now.Add(2*time.Minute))
	if !changed {
		t.Fatal("expected status change to report changed")
	}
	if !status.Conditions[0].LastTransitionTime.Time.After(firstTransition.Time) {
		t.Fatal("expected lastTransitionTime to advance when status changes")
	}
}

func TestRecomputePhase(t *testing.T) {
	status := &InferenceEndpointStatus{}
	now := time.Now().UTC()
	status.SetCondition(ConditionReady, metav1.ConditionTrue, ReasonScaledToZero, "scaled down", 1, now)
	if phase := status.RecomputePhase(false); phase != PhaseReady {
		t.Fatalf("expected ready phase, got %q", phase)
	}
	status.SetCondition(ConditionProgressing, metav1.ConditionTrue, ReasonRollingOut, "rolling", 1, now)
	if phase := status.RecomputePhase(false); phase != PhaseProgressing {
		t.Fatalf("expected progressing phase, got %q", phase)
	}
	status.SetCondition(ConditionDegraded, metav1.ConditionTrue, ReasonInvalidModel, "bad", 1, now)
	if phase := status.RecomputePhase(false); phase != PhaseDegraded {
		t.Fatalf("expected degraded phase, got %q", phase)
	}
	if phase := status.RecomputePhase(true); phase != PhaseDeleting {
		t.Fatalf("expected deleting phase, got %q", phase)
	}
}

func validEndpoint() *InferenceEndpoint {
	endpoint := &InferenceEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-1", Namespace: EmberSystemNamespace},
		Spec: InferenceEndpointSpec{
			OwnerID: "usr_31d2",
			Model: InferenceEndpointModelSpec{
				ID:       "qwen2.5-7b-instruct-awq",
				Revision: "b25037543e9394b818fdfca67ab2a00ecc7dd641",
			},
			Profile: ProfileStandard,
			Scaling: InferenceEndpointScalingSpec{
				MinReplicas:        0,
				MaxReplicas:        1,
				TargetQueueDepth:   4,
				IdleTimeoutSeconds: 900,
			},
			Placement: InferenceEndpointPlacementSpec{
				CachePreference:             CachePreferencePreferred,
				MaxColdStartFallbackSeconds: 120,
			},
		},
	}
	endpoint.Default()
	return endpoint
}
