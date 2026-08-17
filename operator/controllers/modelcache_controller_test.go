package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/RuokeZhang/ember/internal/catalog"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	"github.com/RuokeZhang/ember/operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestModelCacheReconcileCreatesJobAndLoadingLabel(t *testing.T) {
	ctx := context.Background()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	cache := pendingModelCache(model)
	nodeA := gpuNode("node-a", 2)
	nodeB := gpuNode("node-b", 2)
	reconciler, c := newModelCacheReconciler(t, cache, nodeB, nodeA)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected bounded loading requeue")
	}
	node := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: "node-a"}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if node.Labels[catalog.CacheLabelKeyForModel(model)] != "loading" {
		t.Fatalf("expected deterministic loading label on node-a, got %#v", node.Labels)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Name: resources.PrefetchJobName(cache), Namespace: servingv1alpha1.EmberSystemNamespace}, job); err != nil {
		t.Fatalf("expected prefetch job: %v", err)
	}
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "node-a" {
		t.Fatalf("expected deterministic node target node-a, got %#v", job.Spec.Template.Spec.NodeSelector)
	}
}

func TestModelCacheReconcileMarksReadyOnJobCompletionAndDerivesReferences(t *testing.T) {
	ctx := context.Background()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	cache := pendingModelCache(model)
	node := gpuNode("node-a", 2)
	node.Labels[catalog.CacheLabelKeyForModel(model)] = "loading"
	job := resources.PrefetchJob(cache, "node-a", true, resources.PrefetchImage)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Date(2026, 8, 15, 19, 10, 0, 0, time.UTC))}}
	endpoint := testEndpoint()
	reconciler, c := newModelCacheReconciler(t, cache, node, job, endpoint)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	current := &servingv1alpha1.ModelCache{}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get cache: %v", err)
	}
	if current.Status.ReferencingEndpoints != 1 {
		t.Fatalf("expected derived reference count 1, got %d", current.Status.ReferencingEndpoints)
	}
	nodeCurrent := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: "node-a"}, nodeCurrent); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if nodeCurrent.Labels[catalog.CacheLabelKeyForModel(model)] != "ready" {
		t.Fatalf("expected node label ready, got %#v", nodeCurrent.Labels)
	}
}

func TestModelCacheReconcileHandlesJobFailureWithoutDuplicateJobs(t *testing.T) {
	ctx := context.Background()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	cache := pendingModelCache(model)
	node := gpuNode("node-a", 2)
	node.Labels[catalog.CacheLabelKeyForModel(model)] = "loading"
	job := resources.PrefetchJob(cache, "node-a", true, resources.PrefetchImage)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "digest mismatch", LastTransitionTime: metav1.NewTime(time.Date(2026, 8, 15, 19, 10, 0, 0, time.UTC))}}
	reconciler, c := newModelCacheReconciler(t, cache, node, job)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	current := &servingv1alpha1.ModelCache{}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get cache: %v", err)
	}
	if current.Status.Conditions[0].Reason != servingv1alpha1.ReasonWeightDownloadFailed && current.Status.Conditions[2].Reason != servingv1alpha1.ReasonWeightDownloadFailed {
		t.Fatalf("expected WeightDownloadFailed condition, got %#v", current.Status.Conditions)
	}
	nodeCurrent := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: "node-a"}, nodeCurrent); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if _, ok := nodeCurrent.Labels[catalog.CacheLabelKeyForModel(model)]; ok {
		t.Fatalf("expected loading label cleared on failure, got %#v", nodeCurrent.Labels)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(servingv1alpha1.EmberSystemNamespace)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected no duplicate job creation, got %d jobs", len(jobs.Items))
	}
}

func TestModelCacheReconcileRestartSafeWithExistingActiveJob(t *testing.T) {
	ctx := context.Background()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	cache := pendingModelCache(model)
	node := gpuNode("node-a", 2)
	node.Labels[catalog.CacheLabelKeyForModel(model)] = "loading"
	job := resources.PrefetchJob(cache, "node-a", true, resources.PrefetchImage)
	job.Status.Active = 1
	reconciler, c := newModelCacheReconciler(t, cache, node, job)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(servingv1alpha1.EmberSystemNamespace)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected one existing job, got %d", len(jobs.Items))
	}
}

func TestModelCacheReconcileRecreatesMissingJobForLoadingNode(t *testing.T) {
	ctx := context.Background()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	cache := pendingModelCache(model)
	node := gpuNode("node-a", 2)
	node.Labels[catalog.CacheLabelKeyForModel(model)] = "loading"
	reconciler, c := newModelCacheReconciler(t, cache, node)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Name: resources.PrefetchJobName(cache), Namespace: servingv1alpha1.EmberSystemNamespace}, job); err != nil {
		t.Fatalf("expected missing prefetch job to be recreated: %v", err)
	}
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "node-a" {
		t.Fatalf("expected recreated job to target loading node-a, got %#v", job.Spec.Template.Spec.NodeSelector)
	}
}

func newModelCacheReconciler(t *testing.T, objects ...client.Object) (*ModelCacheReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme, rbacv1.AddToScheme, corev1.AddToScheme, servingv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add scheme: %v", err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&servingv1alpha1.InferenceEndpoint{}, &servingv1alpha1.ModelCache{}, &batchv1.Job{}).WithObjects(objects...).Build()
	reconciler := &ModelCacheReconciler{Client: c, DirectClient: c, ManagedNamespace: servingv1alpha1.EmberSystemNamespace, Clock: staticClock{now: time.Date(2026, 8, 15, 22, 0, 0, 0, time.UTC)}, SimulationMode: true}
	return reconciler, c
}

func pendingModelCache(model catalog.Model) *servingv1alpha1.ModelCache {
	cache := &servingv1alpha1.ModelCache{ObjectMeta: metav1.ObjectMeta{Name: catalog.ModelCacheNameForModel(model), UID: types.UID("cache-uid"), Generation: 1}, Spec: servingv1alpha1.ModelCacheSpec{ModelID: model.ID, Revision: model.Revision, Digest: model.SimulationArtifact.Digest, SizeBytes: model.SimulationArtifact.SizeBytes, NodePoolSelector: catalog.CopySelector(model.NodePoolSelector), RetentionPolicy: servingv1alpha1.RetentionPolicyLRUWithFloor}}
	cache.Default()
	return cache
}

func gpuNode(name string, gpu int64) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"ember.dev/gpu": "l4", "kubernetes.io/hostname": name}}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): *resource.NewQuantity(gpu, resource.DecimalSI)}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}
