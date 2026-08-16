package gateway

import (
	"bytes"
	"context"
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
	Core           kubernetes.Interface
	Namespace      string
	Now            func() time.Time
	ActivityWindow time.Duration

	mu           sync.Mutex
	lastActivity map[types.UID]time.Time
}

func NewKubernetesStore(c client.Client, core kubernetes.Interface, namespace string) *KubernetesStore {
	return &KubernetesStore{
		Client:         c,
		Core:           core,
		Namespace:      namespace,
		Now:            func() time.Time { return time.Now().UTC() },
		ActivityWindow: 30 * time.Second,
		lastActivity:   map[types.UID]time.Time{},
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
	endpoint := &servingv1alpha1.InferenceEndpoint{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, endpoint); err != nil {
		if client.IgnoreNotFound(err) == nil {
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
	endpoint, err := s.GetEndpoint(ctx, ownerID, name)
	if err != nil {
		return err
	}
	return s.Client.Delete(ctx, endpoint)
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
	now := s.now()
	if activate {
		base := endpoint.DeepCopy()
		if endpoint.Annotations == nil {
			endpoint.Annotations = map[string]string{}
		}
		endpoint.Annotations[ActivationAnnotation] = now.Format(time.RFC3339Nano)
		if err := s.Client.Patch(ctx, endpoint, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("set activation annotation: %w", err)
		}
	}

	if !s.shouldWriteActivity(endpoint.UID, now) {
		return nil
	}
	current, err := s.GetEndpoint(ctx, ownerID, name)
	if err != nil {
		return err
	}
	base := current.DeepCopy()
	stamp := metav1.NewTime(now)
	current.Status.LastActivityTime = &stamp
	if err := s.Client.Status().Patch(ctx, current, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("update last activity: %w", err)
	}
	s.recordActivity(endpoint.UID, now)
	return nil
}

func (s *KubernetesStore) shouldWriteActivity(uid types.UID, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.lastActivity[uid]
	return !ok || now.Sub(last) >= s.activityWindow()
}

func (s *KubernetesStore) recordActivity(uid types.UID, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActivity[uid] = now
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
