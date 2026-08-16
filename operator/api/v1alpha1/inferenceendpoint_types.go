package v1alpha1

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/RuokeZhang/ember/internal/catalog"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const (
	ConditionReady       = "Ready"
	ConditionProgressing = "Progressing"
	ConditionDegraded    = "Degraded"

	ReasonAccepted             = "Accepted"
	ReasonReconciling          = "Reconciling"
	ReasonRollingOut           = "RollingOut"
	ReasonWarmingEngine        = "WarmingEngine"
	ReasonLoadingWeights       = "LoadingWeights"
	ReasonEngineServing        = "EngineServing"
	ReasonScaledToZero         = "ScaledToZero"
	ReasonScalingFromZero      = "ScalingFromZero"
	ReasonCacheReady           = "CacheReady"
	ReasonWeightDownloadFailed = "WeightDownloadFailed"
	ReasonInsufficientGPU      = "InsufficientGPU"
	ReasonRolloutComplete      = "RolloutComplete"
	ReasonAsExpected           = "AsExpected"
	ReasonValidationFailed     = "ValidationFailed"
	ReasonInvalidOwner         = "InvalidOwner"
	ReasonInvalidModel         = "InvalidModel"
	ReasonInvalidRevision      = "InvalidRevision"
	ReasonInvalidProfile       = "InvalidProfile"
	ReasonInvalidScaling       = "InvalidScaling"
	ReasonInvalidPlacement     = "InvalidPlacement"
	ReasonTerminating          = "Terminating"
)

const (
	FinalizerEndpointCleanup = "serving.ember.dev/endpoint-cleanup"
	EmberSystemNamespace     = "ember-system"
)

type InferenceEndpointProfile string

const (
	ProfileSmall    InferenceEndpointProfile = "small"
	ProfileStandard InferenceEndpointProfile = "standard"
	ProfileTP2      InferenceEndpointProfile = "tp2"
)

type CachePreference string

const (
	CachePreferencePreferred CachePreference = "Preferred"
	CachePreferenceRequired  CachePreference = "Required"
)

type InferenceEndpointPhase string

const (
	PhasePending     InferenceEndpointPhase = "Pending"
	PhaseProgressing InferenceEndpointPhase = "Progressing"
	PhaseReady       InferenceEndpointPhase = "Ready"
	PhaseDegraded    InferenceEndpointPhase = "Degraded"
	PhaseDeleting    InferenceEndpointPhase = "Deleting"
)

type InferenceEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InferenceEndpointSpec   `json:"spec,omitempty"`
	Status InferenceEndpointStatus `json:"status,omitempty"`
}

type InferenceEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferenceEndpoint `json:"items"`
}

type InferenceEndpointSpec struct {
	OwnerID   string                         `json:"ownerID"`
	Model     InferenceEndpointModelSpec     `json:"model"`
	Profile   InferenceEndpointProfile       `json:"profile"`
	Scaling   InferenceEndpointScalingSpec   `json:"scaling,omitempty"`
	Placement InferenceEndpointPlacementSpec `json:"placement,omitempty"`
}

type InferenceEndpointModelSpec struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

type InferenceEndpointScalingSpec struct {
	MinReplicas        int32 `json:"minReplicas,omitempty"`
	MaxReplicas        int32 `json:"maxReplicas,omitempty"`
	TargetQueueDepth   int32 `json:"targetQueueDepth,omitempty"`
	IdleTimeoutSeconds int32 `json:"idleTimeoutSeconds,omitempty"`
}

type InferenceEndpointPlacementSpec struct {
	CachePreference             CachePreference `json:"cachePreference,omitempty"`
	MaxColdStartFallbackSeconds int32           `json:"maxColdStartFallbackSeconds,omitempty"`
}

type InferenceEndpointStatus struct {
	ObservedGeneration int64                            `json:"observedGeneration,omitempty"`
	Phase              InferenceEndpointPhase           `json:"phase,omitempty"`
	WorkloadNamespace  string                           `json:"workloadNamespace,omitempty"`
	EndpointURL        string                           `json:"endpointURL,omitempty"`
	Replicas           InferenceEndpointReplicaStatus   `json:"replicas,omitempty"`
	Placement          InferenceEndpointPlacementStatus `json:"placement,omitempty"`
	Model              InferenceEndpointModelStatus     `json:"model,omitempty"`
	LastActivityTime   *metav1.Time                     `json:"lastActivityTime,omitempty"`
	Conditions         []metav1.Condition               `json:"conditions,omitempty"`
}

type InferenceEndpointReplicaStatus struct {
	Desired int32 `json:"desired,omitempty"`
	Ready   int32 `json:"ready,omitempty"`
}

type InferenceEndpointPlacementStatus struct {
	Node       string `json:"node,omitempty"`
	CacheState string `json:"cacheState,omitempty"`
}

type InferenceEndpointModelStatus struct {
	ResolvedDigest string `json:"resolvedDigest,omitempty"`
	SizeBytes      int64  `json:"sizeBytes,omitempty"`
}

func (e *InferenceEndpoint) Default() {
	e.Spec.Default()
}

func (s *InferenceEndpointSpec) Default() {
	if s.Profile == "" {
		s.Profile = ProfileStandard
	}
	if s.Scaling.MaxReplicas == 0 {
		s.Scaling.MaxReplicas = 1
	}
	if s.Scaling.TargetQueueDepth == 0 {
		s.Scaling.TargetQueueDepth = 4
	}
	if s.Scaling.IdleTimeoutSeconds == 0 {
		s.Scaling.IdleTimeoutSeconds = 900
	}
	if s.Placement.CachePreference == "" {
		s.Placement.CachePreference = CachePreferencePreferred
	}
	if s.Placement.MaxColdStartFallbackSeconds == 0 {
		s.Placement.MaxColdStartFallbackSeconds = 120
	}
}

func (e *InferenceEndpoint) ValidateCreate() field.ErrorList {
	copy := e.DeepCopy()
	copy.Default()
	return copy.validate(nil)
}

func (e *InferenceEndpoint) ValidateUpdate(old *InferenceEndpoint) field.ErrorList {
	copy := e.DeepCopy()
	copy.Default()
	return copy.validate(old)
}

func (e *InferenceEndpoint) validate(old *InferenceEndpoint) field.ErrorList {
	var errs field.ErrorList
	specPath := field.NewPath("spec")
	ownerPath := specPath.Child("ownerID")
	if strings.TrimSpace(e.Spec.OwnerID) == "" {
		errs = append(errs, field.Required(ownerPath, "ownerID is required"))
	} else if !ownerIDPattern.MatchString(e.Spec.OwnerID) {
		errs = append(errs, field.Invalid(ownerPath, e.Spec.OwnerID, "ownerID must be 3-63 characters of letters, numbers, '.', '_' or '-'"))
	}
	if old != nil && old.Spec.OwnerID != e.Spec.OwnerID {
		errs = append(errs, field.Invalid(ownerPath, e.Spec.OwnerID, "ownerID is immutable"))
	}

	modelPath := specPath.Child("model")
	model, modelOK := catalog.LookupModel(e.Spec.Model.ID)
	if strings.TrimSpace(e.Spec.Model.ID) == "" {
		errs = append(errs, field.Required(modelPath.Child("id"), "model.id is required"))
	} else if !modelOK {
		errs = append(errs, field.NotSupported(modelPath.Child("id"), e.Spec.Model.ID, catalog.ModelIDs()))
	}
	if strings.TrimSpace(e.Spec.Model.Revision) == "" {
		errs = append(errs, field.Required(modelPath.Child("revision"), "model.revision is required"))
	} else if modelOK && e.Spec.Model.Revision != model.Revision {
		errs = append(errs, field.Invalid(modelPath.Child("revision"), e.Spec.Model.Revision, fmt.Sprintf("revision must equal immutable catalog revision %q", model.Revision)))
	}

	profilePath := specPath.Child("profile")
	if _, ok := catalog.LookupProfile(string(e.Spec.Profile)); !ok {
		errs = append(errs, field.NotSupported(profilePath, e.Spec.Profile, catalog.ProfileNames()))
	} else if modelOK {
		allowed := false
		for _, profileName := range model.AllowedProfiles {
			if profileName == string(e.Spec.Profile) {
				allowed = true
				break
			}
		}
		if !allowed {
			errs = append(errs, field.Invalid(profilePath, e.Spec.Profile, "profile is not approved for this model"))
		}
	}

	scalingPath := specPath.Child("scaling")
	if e.Spec.Scaling.MinReplicas < 0 {
		errs = append(errs, field.Invalid(scalingPath.Child("minReplicas"), e.Spec.Scaling.MinReplicas, "minReplicas must be >= 0"))
	}
	if e.Spec.Scaling.MaxReplicas < 1 || e.Spec.Scaling.MaxReplicas > 10 {
		errs = append(errs, field.Invalid(scalingPath.Child("maxReplicas"), e.Spec.Scaling.MaxReplicas, "maxReplicas must be between 1 and 10"))
	}
	if e.Spec.Scaling.MinReplicas > e.Spec.Scaling.MaxReplicas {
		errs = append(errs, field.Invalid(scalingPath.Child("minReplicas"), e.Spec.Scaling.MinReplicas, "minReplicas must be <= maxReplicas"))
	}
	if e.Spec.Scaling.TargetQueueDepth < 1 || e.Spec.Scaling.TargetQueueDepth > 128 {
		errs = append(errs, field.Invalid(scalingPath.Child("targetQueueDepth"), e.Spec.Scaling.TargetQueueDepth, "targetQueueDepth must be between 1 and 128"))
	}
	if e.Spec.Scaling.IdleTimeoutSeconds < 60 || e.Spec.Scaling.IdleTimeoutSeconds > 86400 {
		errs = append(errs, field.Invalid(scalingPath.Child("idleTimeoutSeconds"), e.Spec.Scaling.IdleTimeoutSeconds, "idleTimeoutSeconds must be between 60 and 86400"))
	}

	placementPath := specPath.Child("placement")
	switch e.Spec.Placement.CachePreference {
	case CachePreferencePreferred, CachePreferenceRequired:
	default:
		errs = append(errs, field.NotSupported(placementPath.Child("cachePreference"), e.Spec.Placement.CachePreference, []string{string(CachePreferencePreferred), string(CachePreferenceRequired)}))
	}
	if e.Spec.Placement.MaxColdStartFallbackSeconds < 0 || e.Spec.Placement.MaxColdStartFallbackSeconds > 3600 {
		errs = append(errs, field.Invalid(placementPath.Child("maxColdStartFallbackSeconds"), e.Spec.Placement.MaxColdStartFallbackSeconds, "maxColdStartFallbackSeconds must be between 0 and 3600"))
	}

	return errs
}

func ValidationReason(errs field.ErrorList) string {
	if len(errs) == 0 {
		return ReasonAsExpected
	}
	switch errs[0].Field {
	case "spec.ownerID":
		return ReasonInvalidOwner
	case "spec.model.id":
		return ReasonInvalidModel
	case "spec.model.revision":
		return ReasonInvalidRevision
	case "spec.profile":
		return ReasonInvalidProfile
	}
	if strings.HasPrefix(errs[0].Field, "spec.scaling") {
		return ReasonInvalidScaling
	}
	if strings.HasPrefix(errs[0].Field, "spec.placement") {
		return ReasonInvalidPlacement
	}
	return ReasonValidationFailed
}

func (s *InferenceEndpointStatus) SetCondition(conditionType string, conditionStatus metav1.ConditionStatus, reason, message string, observedGeneration int64, now time.Time) bool {
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

func (s *InferenceEndpointStatus) RecomputePhase(deleting bool) InferenceEndpointPhase {
	if deleting {
		s.Phase = PhaseDeleting
		return s.Phase
	}
	if cond := meta.FindStatusCondition(s.Conditions, ConditionDegraded); cond != nil && cond.Status == metav1.ConditionTrue {
		s.Phase = PhaseDegraded
		return s.Phase
	}
	if cond := meta.FindStatusCondition(s.Conditions, ConditionProgressing); cond != nil && cond.Status == metav1.ConditionTrue {
		s.Phase = PhaseProgressing
		return s.Phase
	}
	if cond := meta.FindStatusCondition(s.Conditions, ConditionReady); cond != nil && cond.Status == metav1.ConditionTrue {
		s.Phase = PhaseReady
		return s.Phase
	}
	s.Phase = PhasePending
	return s.Phase
}

var ownerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,62}$`)
