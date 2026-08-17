package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/RuokeZhang/ember/internal/platform"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesStoreValidatesAndOwnsEndpoints(t *testing.T) {
	ctx := context.Background()
	store, c := newTestKubernetesStore(t)
	_, err := store.CreateEndpoint(ctx, "owner-a", "ep-valid", CreateEndpointRequest{
		ModelID:                  "qwen2.5-7b-instruct-awq",
		Revision:                 "b25037543e9394b818fdfca67ab2a00ecc7dd641",
		Profile:                  servingv1alpha1.ProfileStandard,
		MaxReplicas:              3,
		TargetQueueDepth:         4,
		IdleTimeoutSeconds:       900,
		CachePreference:          servingv1alpha1.CachePreferencePreferred,
		MaxColdStartFallbackSecs: 120,
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	current := &servingv1alpha1.InferenceEndpoint{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: servingv1alpha1.EmberSystemNamespace, Name: "ep-valid"}, current); err != nil {
		t.Fatalf("get created endpoint: %v", err)
	}
	if current.Spec.OwnerID != "owner-a" {
		t.Fatalf("expected injected owner, got %q", current.Spec.OwnerID)
	}
	if _, err := store.GetEndpoint(ctx, "owner-b", "ep-valid"); err != ErrEndpointNotFound {
		t.Fatalf("expected cross-owner lookup to be hidden, got %v", err)
	}
}

func TestKubernetesStoreCreateIsIdempotentForMatchingEndpoint(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestKubernetesStore(t)
	request := CreateEndpointRequest{
		ModelID:                  "qwen2.5-7b-instruct-awq",
		Revision:                 "b25037543e9394b818fdfca67ab2a00ecc7dd641",
		Profile:                  servingv1alpha1.ProfileStandard,
		MaxReplicas:              3,
		TargetQueueDepth:         4,
		IdleTimeoutSeconds:       900,
		CachePreference:          servingv1alpha1.CachePreferencePreferred,
		MaxColdStartFallbackSecs: 120,
	}
	first, err := store.CreateEndpoint(ctx, "owner-a", "ep-idempotent", request)
	if err != nil {
		t.Fatalf("create first endpoint: %v", err)
	}
	second, err := store.CreateEndpoint(ctx, "owner-a", "ep-idempotent", request)
	if err != nil {
		t.Fatalf("replay endpoint create: %v", err)
	}
	if first.Name != second.Name || second.Spec.OwnerID != "owner-a" {
		t.Fatalf("idempotent create returned wrong endpoint: %#v", second)
	}

	request.MaxReplicas = 4
	if _, err := store.CreateEndpoint(ctx, "owner-a", "ep-idempotent", request); err != ErrEndpointConflict {
		t.Fatalf("expected endpoint conflict, got %v", err)
	}
}

func TestKubernetesStoreActivityPatchPreservesConditionsAndThrottles(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC)
	endpoint := readyEndpoint("ep-activity", "owner-a")
	endpoint.Status.Conditions = []metav1.Condition{{Type: servingv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: servingv1alpha1.ReasonEngineServing}}
	store, c := newTestKubernetesStore(t, endpoint)
	store.Now = func() time.Time { return now }
	store.ActivityWindow = 30 * time.Second

	if err := store.MarkActivity(ctx, "owner-a", endpoint.Name, true); err != nil {
		t.Fatalf("mark activity: %v", err)
	}
	current := &servingv1alpha1.InferenceEndpoint{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(endpoint), current); err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if current.Annotations[ActivationAnnotation] == "" {
		t.Fatal("expected activation annotation")
	}
	if current.Status.LastActivityTime == nil || !current.Status.LastActivityTime.Time.Equal(now) {
		t.Fatalf("expected activity timestamp %s, got %#v", now, current.Status.LastActivityTime)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != servingv1alpha1.ReasonEngineServing {
		t.Fatalf("activity patch overwrote controller conditions: %#v", current.Status.Conditions)
	}

	store.Now = func() time.Time { return now.Add(10 * time.Second) }
	if err := store.MarkActivity(ctx, "owner-a", endpoint.Name, false); err != nil {
		t.Fatalf("mark throttled activity: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(endpoint), current); err != nil {
		t.Fatalf("get endpoint after throttle: %v", err)
	}
	if !current.Status.LastActivityTime.Time.Equal(now) {
		t.Fatalf("expected timestamp to remain throttled at %s, got %s", now, current.Status.LastActivityTime)
	}
}

func TestKubernetesStoreInspectionSanitizesEndpointResources(t *testing.T) {
	ctx := context.Background()
	endpoint := readyEndpoint("ep-inspect", "owner-a")
	endpoint.Status.WorkloadNamespace = "ember-ep-inspect"
	endpointLabels := map[string]string{platform.LabelEndpointUID: string(endpoint.UID)}
	replicas := int32(2)
	automount := false
	runAsNonRoot := true
	readOnly := true
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "engine", Namespace: endpoint.Status.WorkloadNamespace, Labels: endpointLabels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				AutomountServiceAccountToken: &automount,
				SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot},
				Containers: []corev1.Container{{
					Name: "engine", Image: "ember-mock-engine:dev",
					SecurityContext: &corev1.SecurityContext{
						ReadOnlyRootFilesystem: &readOnly,
						Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "model-cache", ReadOnly: true}},
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
					}},
				}},
			}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint-quota", Namespace: endpoint.Status.WorkloadNamespace, Labels: endpointLabels},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceName("requests.nvidia.com/gpu"): resource.MustParse("2"),
				corev1.ResourcePods:                            resource.MustParse("3"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceName("requests.nvidia.com/gpu"): resource.MustParse("1"),
				corev1.ResourcePods:                            resource.MustParse("1"),
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine-abc", Namespace: endpoint.Status.WorkloadNamespace, Labels: endpointLabels},
		Spec: corev1.PodSpec{
			NodeName: "ember-worker2",
			Containers: []corev1.Container{{
				Name: "engine", Image: "ember-mock-engine:dev",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
				}},
			}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "engine", Namespace: endpoint.Status.WorkloadNamespace, Labels: endpointLabels},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8000}}},
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-deny", Namespace: endpoint.Status.WorkloadNamespace, Labels: endpointLabels},
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "keda-hpa-engine-autoscaler", Namespace: endpoint.Status.WorkloadNamespace, Labels: endpointLabels},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: ptrInt32(1), MaxReplicas: 3},
		Status:     autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 1, DesiredReplicas: 2},
	}
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "scheduled", Namespace: endpoint.Status.WorkloadNamespace},
		Type:           corev1.EventTypeNormal,
		Reason:         "Scheduled",
		Message:        "Assigned to ember-worker2",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name},
		LastTimestamp:  metav1.NewTime(time.Now().UTC()),
	}
	coreClient := kubernetesfake.NewSimpleClientset(deployment, quota, pod, service, policy, hpa, event)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := servingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Ember scheme: %v", err)
	}
	scaledObject := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1",
		"kind":       "ScaledObject",
		"metadata": map[string]any{
			"name":      "engine-autoscaler",
			"namespace": endpoint.Status.WorkloadNamespace,
			"labels":    map[string]any{platform.LabelEndpointUID: string(endpoint.UID)},
		},
		"spec": map[string]any{
			"minReplicaCount": int64(1),
			"maxReplicaCount": int64(3),
			"pollingInterval": int64(2),
		},
	}}
	scaledObject.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	controllerClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(endpoint, scaledObject).Build()
	store := NewKubernetesStore(controllerClient, coreClient, servingv1alpha1.EmberSystemNamespace)

	inspection, err := store.InspectEndpoint(ctx, endpoint)
	if err != nil {
		t.Fatalf("inspect endpoint: %v", err)
	}
	if inspection.Namespace != endpoint.Status.WorkloadNamespace || len(inspection.Pods) != 1 || inspection.Pods[0].RequestedGPUs != 1 {
		t.Fatalf("unexpected inspection: %#v", inspection)
	}
	if !inspectionHasResource(inspection, "Deployment") || !inspectionHasResource(inspection, "ScaledObject") || !inspectionHasResource(inspection, "HorizontalPodAutoscaler") {
		t.Fatalf("inspection omitted control-plane resources: %#v", inspection.Resources)
	}
	if !inspectionHasSecurityState(inspection, "Model cache mount", "pass") || !inspectionHasSecurityState(inspection, "CNI policy enforcement", "unknown") {
		t.Fatalf("inspection security evidence is incomplete: %#v", inspection.SecurityControls)
	}
	if len(inspection.Events) != 1 || inspection.Events[0].Reason != "Scheduled" {
		t.Fatalf("inspection events missing: %#v", inspection.Events)
	}
}

func newTestKubernetesStore(t *testing.T, objects ...client.Object) (*KubernetesStore, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := servingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Ember scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&servingv1alpha1.InferenceEndpoint{}).WithObjects(objects...).Build()
	return NewKubernetesStore(c, nil, servingv1alpha1.EmberSystemNamespace), c
}

func ptrInt32(value int32) *int32 {
	return &value
}

func inspectionHasResource(inspection *EndpointInspection, kind string) bool {
	for _, resource := range inspection.Resources {
		if resource.Kind == kind {
			return true
		}
	}
	return false
}

func inspectionHasSecurityState(inspection *EndpointInspection, name, state string) bool {
	for _, control := range inspection.SecurityControls {
		if control.Name == name && control.State == state {
			return true
		}
	}
	return false
}
