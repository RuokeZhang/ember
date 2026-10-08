package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RuokeZhang/ember/internal/platform"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ActivationAnnotation = platform.ActivationAnnotation

var (
	ErrEndpointNotFound = errors.New("endpoint not found")
	ErrEndpointConflict = errors.New("endpoint already exists with different configuration")
)

type CreateEndpointRequest struct {
	ModelID                  string
	Revision                 string
	Profile                  servingv1alpha1.InferenceEndpointProfile
	MinReplicas              int32
	MaxReplicas              int32
	TargetQueueDepth         int32
	IdleTimeoutSeconds       int32
	CachePreference          servingv1alpha1.CachePreference
	MaxColdStartFallbackSecs int32
}

type Store interface {
	CreateEndpoint(context.Context, string, string, CreateEndpointRequest) (*servingv1alpha1.InferenceEndpoint, error)
	GetEndpoint(context.Context, string, string) (*servingv1alpha1.InferenceEndpoint, error)
	DeleteEndpoint(context.Context, string, string) error
	EngineLogs(context.Context, *servingv1alpha1.InferenceEndpoint, int64) (string, error)
	InspectEndpoint(context.Context, *servingv1alpha1.InferenceEndpoint) (*EndpointInspection, error)
	MarkActivity(context.Context, string, string, bool) error
}

type ValidationError struct {
	Errors field.ErrorList
}

func (e *ValidationError) Error() string {
	return e.Errors.ToAggregate().Error()
}

type KubernetesStore struct {
	Client         client.Client
	EndpointReader client.Reader
	Core           kubernetes.Interface
	Namespace      string
	Now            func() time.Time
	ActivityWindow time.Duration

	mu       sync.Mutex
	activity map[types.UID]*endpointActivity
}

type endpointActivity struct {
	mu             sync.Mutex
	lastActivity   time.Time
	lastActivation time.Time
}

func NewKubernetesStore(c client.Client, core kubernetes.Interface, namespace string) *KubernetesStore {
	return &KubernetesStore{
		Client:         c,
		EndpointReader: c,
		Core:           core,
		Namespace:      namespace,
		Now:            func() time.Time { return time.Now().UTC() },
		ActivityWindow: 30 * time.Second,
		activity:       map[types.UID]*endpointActivity{},
	}
}

func (s *KubernetesStore) CreateEndpoint(ctx context.Context, ownerID, name string, request CreateEndpointRequest) (*servingv1alpha1.InferenceEndpoint, error) {
	endpoint := &servingv1alpha1.InferenceEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace},
		Spec: servingv1alpha1.InferenceEndpointSpec{
			OwnerID: ownerID,
			Model: servingv1alpha1.InferenceEndpointModelSpec{
				ID:       request.ModelID,
				Revision: request.Revision,
			},
			Profile: request.Profile,
			Scaling: servingv1alpha1.InferenceEndpointScalingSpec{
				MinReplicas:        request.MinReplicas,
				MaxReplicas:        request.MaxReplicas,
				TargetQueueDepth:   request.TargetQueueDepth,
				IdleTimeoutSeconds: request.IdleTimeoutSeconds,
			},
			Placement: servingv1alpha1.InferenceEndpointPlacementSpec{
				CachePreference:             request.CachePreference,
				MaxColdStartFallbackSeconds: request.MaxColdStartFallbackSecs,
			},
		},
	}
	endpoint.Default()
	if validationErrs := endpoint.ValidateCreate(); len(validationErrs) > 0 {
		return nil, &ValidationError{Errors: validationErrs}
	}
	if err := s.Client.Create(ctx, endpoint); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		current := &servingv1alpha1.InferenceEndpoint{}
		if getErr := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, current); getErr != nil {
			return nil, getErr
		}
		if current.Spec.OwnerID != ownerID || !sameEndpointSpec(current.Spec, endpoint.Spec) {
			return nil, ErrEndpointConflict
		}
		return current, nil
	}
	return endpoint, nil
}

func (s *KubernetesStore) GetEndpoint(ctx context.Context, ownerID, name string) (*servingv1alpha1.InferenceEndpoint, error) {
	endpoint, err := s.getOwnedEndpoint(ctx, s.EndpointReader, ownerID, name)
	if errors.Is(err, ErrEndpointNotFound) {
		return s.getOwnedEndpoint(ctx, s.Client, ownerID, name)
	}
	return endpoint, err
}

func (s *KubernetesStore) getOwnedEndpoint(ctx context.Context, reader client.Reader, ownerID, name string) (*servingv1alpha1.InferenceEndpoint, error) {
	endpoint := &servingv1alpha1.InferenceEndpoint{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, endpoint); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrEndpointNotFound
		}
		return nil, err
	}
	if endpoint.Spec.OwnerID != ownerID {
		return nil, ErrEndpointNotFound
	}
	return endpoint, nil
}

func (s *KubernetesStore) DeleteEndpoint(ctx context.Context, ownerID, name string) error {
	endpoint, err := s.getOwnedEndpoint(ctx, s.Client, ownerID, name)
	if err != nil {
		return err
	}
	if err := s.Client.Delete(ctx, endpoint, client.Preconditions{UID: &endpoint.UID, ResourceVersion: &endpoint.ResourceVersion}); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.activity, endpoint.UID)
	s.mu.Unlock()
	return nil
}

func (s *KubernetesStore) EngineLogs(ctx context.Context, endpoint *servingv1alpha1.InferenceEndpoint, tailLines int64) (string, error) {
	if s.Core == nil {
		return "", errors.New("Kubernetes Core client is unavailable")
	}
	namespace := endpoint.Status.WorkloadNamespace
	if namespace == "" {
		return "", errors.New("endpoint has no workload namespace")
	}
	selector := labels.Set{
		platform.LabelEndpointUID: string(endpoint.UID),
		platform.LabelComponent:   platform.EngineName,
	}.AsSelector().String()
	pods, err := s.Core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", errors.New("endpoint has no engine Pod")
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})
	limitBytes := int64(256 << 10)
	stream, err := s.Core.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{
		Container:  platform.EngineName,
		TailLines:  &tailLines,
		LimitBytes: &limitBytes,
		Timestamps: true,
	}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var output bytes.Buffer
	if _, err := io.CopyN(&output, stream, limitBytes+1); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if output.Len() > int(limitBytes) {
		return "", fmt.Errorf("engine logs exceeded %d bytes", limitBytes)
	}
	return output.String(), nil
}

func (s *KubernetesStore) MarkActivity(ctx context.Context, ownerID, name string, activate bool) error {
	endpoint, err := s.GetEndpoint(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if !endpoint.DeletionTimestamp.IsZero() {
		return ErrEndpointNotFound
	}
	s.mu.Lock()
	activity := s.activity[endpoint.UID]
	if activity == nil {
		activity = &endpointActivity{}
		s.activity[endpoint.UID] = activity
	}
	s.mu.Unlock()
	activity.mu.Lock()
	defer activity.mu.Unlock()
	now := s.now()
	if activate && (activity.lastActivation.IsZero() || now.Sub(activity.lastActivation) >= 5*time.Second) {
		if err := s.patchActivity(ctx, endpoint, ownerID, now, true); err != nil {
			return err
		}
		activity.lastActivation = now
	}
	if !activity.lastActivity.IsZero() && now.Sub(activity.lastActivity) < s.activityWindow() {
		return nil
	}
	if err := s.patchActivity(ctx, endpoint, ownerID, now, false); err != nil {
		return err
	}
	activity.lastActivity = now
	return nil
}

func (s *KubernetesStore) patchActivity(ctx context.Context, endpoint *servingv1alpha1.InferenceEndpoint, ownerID string, now time.Time, activate bool) error {
	originalUID := endpoint.UID
	refresh := false
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if refresh {
			current, err := s.getOwnedEndpoint(ctx, s.Client, ownerID, endpoint.Name)
			if err != nil {
				return err
			}
			if current.UID != originalUID || !current.DeletionTimestamp.IsZero() {
				return ErrEndpointNotFound
			}
			*endpoint = *current
		}
		refresh = true
		if !activate && endpoint.Status.LastActivityTime != nil && !endpoint.Status.LastActivityTime.Before(&metav1.Time{Time: now}) {
			return nil
		}
		current := endpoint.DeepCopy()
		payload := map[string]any{"metadata": map[string]any{"uid": current.UID, "resourceVersion": current.ResourceVersion}}
		if activate {
			payload["metadata"].(map[string]any)["annotations"] = map[string]string{ActivationAnnotation: now.Format(time.RFC3339Nano)}
		} else {
			payload["status"] = map[string]any{"lastActivityTime": metav1.NewTime(now)}
		}
		data, _ := json.Marshal(payload)
		patch := client.RawPatch(types.MergePatchType, data)
		var err error
		if activate {
			err = s.Client.Patch(ctx, current, patch)
		} else {
			err = s.Client.Status().Patch(ctx, current, patch)
		}
		if err == nil {
			*endpoint = *current
		}
		return err
	})
}

func (s *KubernetesStore) activityWindow() time.Duration {
	if s.ActivityWindow > 0 {
		return s.ActivityWindow
	}
	return 30 * time.Second
}

func (s *KubernetesStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func normalizeName(name string) string {
	return strings.TrimSpace(name)
}

func sameEndpointSpec(left, right servingv1alpha1.InferenceEndpointSpec) bool {
	return left.OwnerID == right.OwnerID &&
		left.Model == right.Model &&
		left.Profile == right.Profile &&
		left.Scaling == right.Scaling &&
		left.Placement == right.Placement
}
