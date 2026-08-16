package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/RuokeZhang/ember/internal/catalog"
	"github.com/RuokeZhang/ember/internal/platform"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	"github.com/RuokeZhang/ember/operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type staticClock struct{ now time.Time }

func (c staticClock) Now() time.Time { return c.now }

func TestEndpointReconcileWaitsForModelCacheBeforeDeployment(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	reconciler, c := newControllerClient(t, endpoint)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(endpoint)}

	mustReconcile(t, ctx, reconciler, req)
	mustReconcile(t, ctx, reconciler, req)

	current := &servingv1alpha1.InferenceEndpoint{}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if current.Status.Phase != servingv1alpha1.PhaseProgressing {
		t.Fatalf("expected progressing phase, got %q", current.Status.Phase)
	}
	ready := current.Status.Conditions[0]
	if ready.Reason != servingv1alpha1.ReasonLoadingWeights {
		t.Fatalf("expected LoadingWeights reason, got %q", ready.Reason)
	}
	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Name: resources.EngineName, Namespace: resources.WorkloadNamespaceName(endpoint.UID)}, deployment); err == nil {
		t.Fatal("expected no deployment before cache is ready")
	}
	cache := &servingv1alpha1.ModelCache{}
	if err := c.Get(ctx, client.ObjectKey{Name: catalog.ModelCacheName(endpoint.Spec.Model.ID, endpoint.Spec.Model.Revision)}, cache); err != nil {
		t.Fatalf("expected auto-created model cache: %v", err)
	}
}

func TestEndpointReconcileCreatesPinnedDeploymentWhenCacheReady(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	cacheHash := catalog.CacheHashForModel(model)
	cache := readyModelCache(model)
	node := readyNode("node-a", cacheHash, 2)
	reconciler, c := newControllerClient(t, endpoint, cache, node)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(endpoint)}

	mustReconcile(t, ctx, reconciler, req)
	mustReconcile(t, ctx, reconciler, req)

	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Name: resources.EngineName, Namespace: resources.WorkloadNamespaceName(endpoint.UID)}, deployment); err != nil {
		t.Fatalf("expected deployment once cache ready: %v", err)
	}
	if deployment.Spec.Template.Spec.Affinity == nil {
		t.Fatal("expected deployment affinity to ready cache node")
	}
	if deployment.Spec.Template.Spec.InitContainers[0].Args[0] != "--verify-only" {
		t.Fatalf("expected verify-only init container args, got %#v", deployment.Spec.Template.Spec.InitContainers[0].Args)
	}
	current := &servingv1alpha1.InferenceEndpoint{}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if current.Status.Placement.Node != "node-a" || current.Status.Placement.CacheState != "Hit" {
		t.Fatalf("expected cache hit placement, got %#v", current.Status.Placement)
	}

	deployment.Status.ReadyReplicas = 1
	deployment.Status.AvailableReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.ObservedGeneration = deployment.Generation
	if err := c.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}
	mustReconcile(t, ctx, reconciler, req)
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get endpoint after ready: %v", err)
	}
	if current.Status.Phase != servingv1alpha1.PhaseReady {
		t.Fatalf("expected ready phase, got %q", current.Status.Phase)
	}
}

func TestEndpointReconcileStableAfterCacheReady(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	cacheHash := catalog.CacheHashForModel(model)
	reconciler, c := newControllerClient(t, endpoint, readyModelCache(model), readyNode("node-a", cacheHash, 2))
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(endpoint)}

	mustReconcile(t, ctx, reconciler, req)
	mustReconcile(t, ctx, reconciler, req)
	mustReconcile(t, ctx, reconciler, req)

	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.InNamespace(resources.WorkloadNamespaceName(endpoint.UID))); err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(deployments.Items) != 1 {
		t.Fatalf("expected one deployment, got %d", len(deployments.Items))
	}
	var services corev1.ServiceList
	if err := c.List(ctx, &services, client.InNamespace(resources.WorkloadNamespaceName(endpoint.UID))); err != nil {
		t.Fatalf("list services: %v", err)
	}
	if len(services.Items) != 1 {
		t.Fatalf("expected one service, got %d", len(services.Items))
	}
	var caches servingv1alpha1.ModelCacheList
	if err := c.List(ctx, &caches); err != nil {
		t.Fatalf("list model caches: %v", err)
	}
	if len(caches.Items) != 1 {
		t.Fatalf("expected one model cache, got %d", len(caches.Items))
	}
}

func TestEndpointReconcilePreservesAutoscalerReplicaCount(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	cacheHash := catalog.CacheHashForModel(model)
	reconciler, c := newControllerClient(t, endpoint, readyModelCache(model), readyNode("node-a", cacheHash, 2))
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(endpoint)}

	mustReconcile(t, ctx, reconciler, req)
	mustReconcile(t, ctx, reconciler, req)

	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Name: resources.EngineName, Namespace: resources.WorkloadNamespaceName(endpoint.UID)}
	if err := c.Get(ctx, key, deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	two := int32(2)
	deployment.Spec.Replicas = &two
	if err := c.Update(ctx, deployment); err != nil {
		t.Fatalf("simulate HPA scale-up: %v", err)
	}

	mustReconcile(t, ctx, reconciler, req)
	if err := c.Get(ctx, key, deployment); err != nil {
		t.Fatalf("get reconciled deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 2 {
		t.Fatalf("operator overwrote the autoscaler replica count: %#v", deployment.Spec.Replicas)
	}
}

func TestEndpointStatusPatchPreservesConcurrentActivity(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	reconciler, c := newControllerClient(t, endpoint)
	stale := &servingv1alpha1.InferenceEndpoint{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(endpoint), stale); err != nil {
		t.Fatalf("get stale endpoint: %v", err)
	}
	originalStatus := stale.Status.DeepCopy()

	current := stale.DeepCopy()
	activity := metav1.NewTime(time.Date(2026, 8, 15, 22, 5, 0, 0, time.UTC))
	current.Status.LastActivityTime = &activity
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatalf("write concurrent activity: %v", err)
	}

	stale.Status.Phase = servingv1alpha1.PhaseProgressing
	if err := reconciler.updateStatusIfChanged(ctx, stale, originalStatus); err != nil {
		t.Fatalf("patch stale status: %v", err)
	}
	updated := &servingv1alpha1.InferenceEndpoint{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(endpoint), updated); err != nil {
		t.Fatalf("get updated endpoint: %v", err)
	}
	if updated.Status.LastActivityTime == nil || !updated.Status.LastActivityTime.Equal(&activity) {
		t.Fatalf("expected concurrent activity to survive status patch, got %#v", updated.Status.LastActivityTime)
	}
	if updated.Status.Phase != servingv1alpha1.PhaseProgressing {
		t.Fatalf("expected controller phase update, got %q", updated.Status.Phase)
	}
}

func TestEndpointInvalidSpecAllocatesNothing(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	endpoint.Spec.Model.ID = "bad-model"
	reconciler, c := newControllerClient(t, endpoint)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(endpoint)}

	mustReconcile(t, ctx, reconciler, req)
	mustReconcile(t, ctx, reconciler, req)

	current := &servingv1alpha1.InferenceEndpoint{}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if current.Status.Phase != servingv1alpha1.PhaseDegraded {
		t.Fatalf("expected degraded phase, got %q", current.Status.Phase)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: resources.WorkloadNamespaceName(endpoint.UID)}, ns); err == nil {
		t.Fatal("expected no workload namespace for invalid spec")
	}
}

func TestIdleEndpointScalesToZeroAfterCacheReady(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	lastActivity := metav1.NewTime(time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC))
	endpoint.Status.LastActivityTime = &lastActivity
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	cacheHash := catalog.CacheHashForModel(model)
	reconciler, c := newControllerClient(t, endpoint, readyModelCache(model), readyNode("node-a", cacheHash, 2))
	reconciler.Clock = staticClock{now: lastActivity.Time.Add(20 * time.Minute)}
	reconciler.EnableKEDA = true
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(endpoint)}

	mustReconcile(t, ctx, reconciler, req)
	mustReconcile(t, ctx, reconciler, req)

	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Name: resources.EngineName, Namespace: resources.WorkloadNamespaceName(endpoint.UID)}, deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		t.Fatalf("expected deployment scaled to zero, got %#v", deployment.Spec.Replicas)
	}
	scaledObject := &unstructured.Unstructured{}
	scaledObject.SetGroupVersionKind(resources.ScaledObjectGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: resources.ScaledObjectName, Namespace: resources.WorkloadNamespaceName(endpoint.UID)}, scaledObject); err != nil {
		t.Fatalf("get ScaledObject: %v", err)
	}
	if scaledObject.GetAnnotations()[resources.KEDAPausedAnnotation] != "true" {
		t.Fatalf("expected KEDA paused at zero, got %#v", scaledObject.GetAnnotations())
	}
}

func TestExplicitActivationOverridesStaleIdleStatusUntilReady(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	lastActivity := metav1.NewTime(time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC))
	endpoint.Status.LastActivityTime = &lastActivity
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	cacheHash := catalog.CacheHashForModel(model)
	reconciler, c := newControllerClient(t, endpoint, readyModelCache(model), readyNode("node-a", cacheHash, 2))
	reconciler.Clock = staticClock{now: lastActivity.Time.Add(20 * time.Minute)}
	reconciler.EnableKEDA = true
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(endpoint)}

	mustReconcile(t, ctx, reconciler, req)
	mustReconcile(t, ctx, reconciler, req)

	current := &servingv1alpha1.InferenceEndpoint{}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get idle endpoint: %v", err)
	}
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[platform.ActivationAnnotation] = reconciler.Clock.Now().Format(time.RFC3339Nano)
	if err := c.Update(ctx, current); err != nil {
		t.Fatalf("set activation annotation: %v", err)
	}

	mustReconcile(t, ctx, reconciler, req)
	deployment := &appsv1.Deployment{}
	deploymentKey := client.ObjectKey{Name: resources.EngineName, Namespace: resources.WorkloadNamespaceName(endpoint.UID)}
	if err := c.Get(ctx, deploymentKey, deployment); err != nil {
		t.Fatalf("get activated deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("expected activation to scale from zero despite stale activity, got %#v", deployment.Spec.Replicas)
	}
	scaledObject := &unstructured.Unstructured{}
	scaledObject.SetGroupVersionKind(resources.ScaledObjectGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: resources.ScaledObjectName, Namespace: resources.WorkloadNamespaceName(endpoint.UID)}, scaledObject); err != nil {
		t.Fatalf("get unpaused ScaledObject: %v", err)
	}
	if scaledObject.GetAnnotations()[resources.KEDAPausedAnnotation] != "true" {
		t.Fatalf("expected KEDA to remain paused during activation, got %#v", scaledObject.GetAnnotations())
	}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get activating endpoint: %v", err)
	}
	if current.Annotations[platform.ActivationAnnotation] == "" {
		t.Fatal("activation annotation must remain until the engine is ready")
	}
	recentActivity := metav1.NewTime(reconciler.Clock.Now())
	current.Status.LastActivityTime = &recentActivity
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatalf("record delayed gateway activity: %v", err)
	}

	deployment.Status.Replicas = 1
	deployment.Status.ReadyReplicas = 1
	deployment.Status.AvailableReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.ObservedGeneration = deployment.Generation
	if err := c.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("mark activated deployment ready: %v", err)
	}
	mustReconcile(t, ctx, reconciler, req)
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get ready endpoint: %v", err)
	}
	if current.Annotations[platform.ActivationAnnotation] != "" {
		t.Fatalf("expected ready activation annotation cleared, got %#v", current.Annotations)
	}
	mustReconcile(t, ctx, reconciler, req)
	if err := c.Get(ctx, client.ObjectKey{Name: resources.ScaledObjectName, Namespace: resources.WorkloadNamespaceName(endpoint.UID)}, scaledObject); err != nil {
		t.Fatalf("get unpaused ScaledObject: %v", err)
	}
	if _, paused := scaledObject.GetAnnotations()[resources.KEDAPausedAnnotation]; paused {
		t.Fatalf("expected KEDA unpaused after activation completed, got %#v", scaledObject.GetAnnotations())
	}
}

func TestEndpointFinalizationLeavesCacheAndRemovesFinalizer(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	cacheHash := catalog.CacheHashForModel(model)
	nsName := resources.WorkloadNamespaceName(endpoint.UID)
	deletionTime := metav1.NewTime(time.Date(2026, 8, 15, 21, 0, 0, 0, time.UTC))
	endpoint.Finalizers = []string{servingv1alpha1.FinalizerEndpointCleanup}
	endpoint.DeletionTimestamp = &deletionTime
	endpoint.Status.WorkloadNamespace = nsName
	cache := readyModelCache(model)
	node := readyNode("node-a", cacheHash, 2)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	replicas := int32(1)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resources.EngineName, Namespace: nsName}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "engine-0", Namespace: nsName, Labels: resources.ManagedLabels(endpoint)}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	scaledObject := resources.ScaledObject(endpoint, true, false)
	reconciler, c := newControllerClient(t, endpoint, cache, node, ns, deployment, pod, scaledObject)
	reconciler.EnableKEDA = true
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(endpoint)}

	mustReconcile(t, ctx, reconciler, req)
	currentScaledObject := &unstructured.Unstructured{}
	currentScaledObject.SetGroupVersionKind(resources.ScaledObjectGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(scaledObject), currentScaledObject); err == nil {
		t.Fatal("expected ScaledObject deleted before workload teardown")
	}
	mustReconcile(t, ctx, reconciler, req)
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	mustReconcile(t, ctx, reconciler, req)
	mustReconcile(t, ctx, reconciler, req)

	remainingCache := &servingv1alpha1.ModelCache{}
	if err := c.Get(ctx, client.ObjectKey{Name: cache.Name}, remainingCache); err != nil {
		t.Fatalf("expected model cache to persist: %v", err)
	}
	current := &servingv1alpha1.InferenceEndpoint{}
	if err := c.Get(ctx, req.NamespacedName, current); err == nil && len(current.Finalizers) != 0 {
		t.Fatalf("expected finalizer removed, got %#v", current.Finalizers)
	}
}

func newControllerClient(t *testing.T, objects ...client.Object) (*EndpointReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	mustAddSchemes(t, scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&servingv1alpha1.InferenceEndpoint{}, &servingv1alpha1.ModelCache{}, &appsv1.Deployment{}, &batchv1.Job{}).WithObjects(objects...).Build()
	reconciler := &EndpointReconciler{Client: c, DirectClient: c, ManagedNamespace: servingv1alpha1.EmberSystemNamespace, Clock: staticClock{now: time.Date(2026, 8, 15, 22, 0, 0, 0, time.UTC)}, SimulationMode: true}
	return reconciler, c
}

func mustAddSchemes(t *testing.T, scheme *runtime.Scheme) {
	t.Helper()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme, rbacv1.AddToScheme, corev1.AddToScheme, servingv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add scheme: %v", err)
		}
	}
}

func mustReconcile(t *testing.T, ctx context.Context, reconciler *EndpointReconciler, req ctrl.Request) {
	t.Helper()
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
}

func testEndpoint() *servingv1alpha1.InferenceEndpoint {
	endpoint := &servingv1alpha1.InferenceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "ep-7f92c8", Namespace: servingv1alpha1.EmberSystemNamespace, UID: types.UID("7f92c8aa-1111-2222-3333-444444444444"), Generation: 1, CreationTimestamp: metav1.NewTime(time.Date(2026, 8, 15, 19, 0, 0, 0, time.UTC))}, Spec: servingv1alpha1.InferenceEndpointSpec{OwnerID: "usr_31d2", Model: servingv1alpha1.InferenceEndpointModelSpec{ID: "qwen2.5-7b-instruct-awq", Revision: "9c1f4ae"}, Profile: servingv1alpha1.ProfileStandard, Scaling: servingv1alpha1.InferenceEndpointScalingSpec{MinReplicas: 0, MaxReplicas: 3, TargetQueueDepth: 4, IdleTimeoutSeconds: 900}, Placement: servingv1alpha1.InferenceEndpointPlacementSpec{CachePreference: servingv1alpha1.CachePreferencePreferred, MaxColdStartFallbackSeconds: 120}}}
	endpoint.Default()
	return endpoint
}

func readyModelCache(model catalog.Model) *servingv1alpha1.ModelCache {
	now := metav1.NewTime(time.Date(2026, 8, 15, 19, 5, 0, 0, time.UTC))
	cache := &servingv1alpha1.ModelCache{ObjectMeta: metav1.ObjectMeta{Name: catalog.ModelCacheNameForModel(model), UID: types.UID("cache-uid"), Generation: 1}, Spec: servingv1alpha1.ModelCacheSpec{ModelID: model.ID, Revision: model.Revision, Digest: model.SimulationArtifact.Digest, SizeBytes: model.SimulationArtifact.SizeBytes, NodePoolSelector: catalog.CopySelector(model.NodePoolSelector), RetentionPolicy: servingv1alpha1.RetentionPolicyLRUWithFloor}, Status: servingv1alpha1.ModelCacheStatus{ObservedGeneration: 1, Nodes: []servingv1alpha1.ModelCacheNodeStatus{{Name: "node-a", State: servingv1alpha1.ModelCacheNodeStateReady, ProgressBytes: model.SimulationArtifact.SizeBytes, MaterializedAt: &now, Message: "warm cache"}}, Conditions: []metav1.Condition{{Type: servingv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: servingv1alpha1.ReasonCacheReady, Message: "warm cache", ObservedGeneration: 1, LastTransitionTime: now}}}}
	return cache
}

func readyNode(name, cacheHash string, gpu int64) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"ember.dev/gpu": "l4", "kubernetes.io/hostname": name, "cache.ember.dev/" + cacheHash: "ready"}}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): *resource.NewQuantity(gpu, resource.DecimalSI)}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}
