package controllers

import (
	"context"
	"reflect"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	cache.Spec.NodePoolSelector["zone"] = "zone-a"
	nodeA := gpuNode("node-a", 2)
	nodeA.Labels["zone"] = "zone-a"
	nodeB := gpuNode("node-b", 2)
	nodeB.Labels["zone"] = "zone-b"
	noGPU := gpuNode("node-0", 0)
	noGPU.Labels["zone"] = "zone-a"
	unready := gpuNode("node-1", 2)
	unready.Labels["zone"] = "zone-a"
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	reconciler, c := newModelCacheReconciler(t, cache, nodeB, nodeA, noGPU, unready)
	reconciler.Client = indexedQueryClient{Client: c}
	reconciler.DirectClient = indexedQueryClient{Client: c, rejectNodeLists: true}
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
	job := resources.PrefetchJob(cache, node, true, resources.PrefetchImage)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Date(2026, 8, 15, 19, 10, 0, 0, time.UTC))}}
	endpoint := testEndpoint()
	reconciler, c := newModelCacheReconciler(t, cache, node, job, endpoint)
	reconciler.Client = indexedQueryClient{Client: c}
	reconciler.DirectClient = indexedQueryClient{Client: c, rejectNodeLists: true}
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
	job := resources.PrefetchJob(cache, node, true, resources.PrefetchImage)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "digest mismatch", LastTransitionTime: metav1.NewTime(time.Date(2026, 8, 15, 22, 0, 0, 0, time.UTC))}}
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
	job := resources.PrefetchJob(cache, node, true, resources.PrefetchImage)
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
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&corev1.Pod{}, podNodeIndex, podNodeKeys).
		WithIndex(&corev1.Node{}, nodeModelCacheIndex, nodeModelCacheKeys).
		WithIndex(&corev1.Node{}, nodeGPUPoolIndex, nodeGPUPoolKeys).
		WithIndex(&servingv1alpha1.InferenceEndpoint{}, endpointModelIndex, endpointModelKeys).WithStatusSubresource(&servingv1alpha1.InferenceEndpoint{}, &servingv1alpha1.ModelCache{}, &batchv1.Job{}).WithObjects(objects...).Build()
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

func TestModelCacheKeepsBoundReplicaPlacementWhenWarmingTP2(t *testing.T) {
	ctx := context.Background()
	oneGPU := testEndpoint()
	twoGPU := testEndpoint()
	twoGPU.Name, twoGPU.UID, twoGPU.Spec.Profile = "ep-tp2", "tp2-uid", servingv1alpha1.ProfileTP2
	model, _ := catalog.LookupModel(oneGPU.Spec.Model.ID)
	cache := pendingModelCache(model)
	nodeA, nodeB, nodeC := gpuNode("node-a", 2), gpuNode("node-b", 1), gpuNode("node-c", 2)
	for _, node := range []*corev1.Node{nodeA, nodeB} {
		node.Labels[catalog.CacheLabelKeyForModel(model)] = "ready"
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "running-small", Namespace: resources.WorkloadNamespaceName(oneGPU.UID), Labels: resources.LabelsForObject(oneGPU)}, Spec: corev1.PodSpec{NodeName: nodeA.Name, Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	pod.Labels[resources.LabelCacheHash] = catalog.CacheHashForModel(model)
	reconciler, c := newModelCacheReconciler(t, cache, oneGPU, twoGPU, nodeA, nodeB, nodeC, pod)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(resources.PrefetchJob(cache, nodeC, true, "")), job); err != nil {
		t.Fatalf("pending tp2 did not get another cache: %v", err)
	}
	if job.Labels["ember.dev/node-name"] != nodeC.Name {
		t.Fatalf("expected a real two-GPU slot on node-c, got %v", job.Labels)
	}
}

func TestModelCacheRecoversMissingDownloadOnBusyLoadingNode(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	cache := pendingModelCache(model)
	loadingNode, alternative := gpuNode("node-a", 1), gpuNode("node-b", 1)
	loadingNode.Labels[catalog.CacheLabelKeyForModel(model)] = "loading"
	otherPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other-model", Namespace: "other-workload"}, Spec: corev1.PodSpec{NodeName: loadingNode.Name, Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	reconciler, c := newModelCacheReconciler(t, cache, endpoint, loadingNode, alternative, otherPod)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(resources.PrefetchJob(cache, loadingNode, true, "")), job); err != nil {
		t.Fatalf("missing download was not resumed: %v", err)
	}
	if job.Labels["ember.dev/node-name"] != loadingNode.Name {
		t.Fatalf("expected existing download to resume on node-a, got %v", job.Labels)
	}
}

func TestModelCacheWarmsLargestFeasibleProfile(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		nodeBGPU   int64
		targetName string
	}{
		{"unavailable-tp2-allows-small", 1, "node-a"},
		{"available-tp2-preferred", 2, "node-b"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			oneGPU, twoGPU := testEndpoint(), testEndpoint()
			twoGPU.Name, twoGPU.UID, twoGPU.Spec.Profile = "ep-tp2", "tp2-uid", servingv1alpha1.ProfileTP2
			model, _ := catalog.LookupModel(oneGPU.Spec.Model.ID)
			cache := pendingModelCache(model)
			nodeA, nodeB := gpuNode("node-a", 1), gpuNode("node-b", testCase.nodeBGPU)
			reconciler, c := newModelCacheReconciler(t, cache, oneGPU, twoGPU, nodeA, nodeB)
			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}); err != nil {
				t.Fatal(err)
			}
			job := &batchv1.Job{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(resources.PrefetchJob(cache, nodeA, true, "")), job); err != nil {
				t.Fatalf("feasible demand did not get a download: %v", err)
			}
			if job.Labels["ember.dev/node-name"] != testCase.targetName {
				t.Fatalf("expected %s, got %v", testCase.targetName, job.Labels)
			}
		})
	}
}

func TestModelCacheReplicatesForCurrentDeploymentDemand(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	cache := pendingModelCache(model)
	nodeA, nodeB, nodeC := gpuNode("node-a", 1), gpuNode("node-b", 1), gpuNode("node-c", 1)
	nodeA.Labels[catalog.CacheLabelKeyForModel(model)] = "ready"
	replicas := int32(2)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resources.EngineName, Namespace: resources.WorkloadNamespaceName(endpoint.UID)}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}}
	reconciler, c := newModelCacheReconciler(t, cache, endpoint, deployment, nodeA, nodeB, nodeC)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(resources.PrefetchJob(cache, nodeB, true, "")), job); err != nil {
		t.Fatal(err)
	}
	if job.Labels["ember.dev/node-name"] != nodeB.Name {
		t.Fatalf("wrong cache target: %v", job.Labels)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("downloads were not serial: %v", err)
	}
	job.UID = "completed-job"
	if err := c.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(reconciler.clock().Now())}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.List(ctx, &jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatal("unneeded cache copy was started")
	}
	current := &servingv1alpha1.ModelCache{}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	version, stamp := current.ResourceVersion, current.Status.Nodes[1].MaterializedAt.DeepCopy()
	reconciler.Clock = staticClock{now: reconciler.clock().Now().Add(time.Minute)}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if version != current.ResourceVersion || !current.Status.Nodes[1].MaterializedAt.Equal(stamp) {
		t.Fatal("no-op reconcile rewrote cache status")
	}
	replicas = 3
	deployment.Spec.Replicas = &replicas
	if err := c.Update(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
		t.Fatal(err)
	}
	if job.Labels["ember.dev/node-name"] != nodeC.Name {
		t.Fatal("expansion did not warm node-c")
	}
}

func TestModelCacheWarmsAlternativeToBusyCachedNode(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	cache := pendingModelCache(model)
	nodeA, nodeB := gpuNode("node-a", 1), gpuNode("node-b", 1)
	nodeA.Labels[catalog.CacheLabelKeyForModel(model)] = "ready"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other-model", Namespace: "other-workload"}, Spec: corev1.PodSpec{NodeName: nodeA.Name, Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	reconciler, c := newModelCacheReconciler(t, cache, endpoint, nodeA, nodeB, pod)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(resources.PrefetchJob(cache, nodeB, true, "")), job); err != nil || job.Labels["ember.dev/node-name"] != nodeB.Name {
		t.Fatalf("busy cache did not trigger an alternative: %v", err)
	}
	pod.Finalizers = []string{"hold"}
	pod.Labels = map[string]string{resources.LabelManaged: resources.ManagedValue, resources.LabelCacheHash: catalog.CacheHashForModel(model)}
	if err := c.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if available, _, err := reconciler.nodeGPUCapacity(ctx, *nodeA); err != nil || available != 0 {
		t.Fatalf("terminating Pod released capacity early: %d, %v", available, err)
	}
}

func TestCacheDemandRespectsSingleNodeGPUProfiles(t *testing.T) {
	cases := []struct {
		name      string
		demand    map[int64]int64
		capacity  []int64
		remaining map[int64]int64
	}{
		{"fragmented-tp2", map[int64]int64{2: 1}, []int64{1, 1}, map[int64]int64{2: 1}},
		{"mixed", map[int64]int64{1: 1, 2: 1}, []int64{2, 1}, map[int64]int64{}},
		{"mixed-shortfall", map[int64]int64{1: 1, 2: 1}, []int64{2}, map[int64]int64{1: 1}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if actual := remainingCacheDemand(testCase.demand, testCase.capacity); !reflect.DeepEqual(actual, testCase.remaining) {
				t.Fatalf("got %v, want %v", actual, testCase.remaining)
			}
		})
	}
}

func TestModelCacheSecondaryFailurePreservesWarmReadiness(t *testing.T) {
	ctx := context.Background()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	cache := pendingModelCache(model)
	nodeA, nodeB := gpuNode("node-a", 1), gpuNode("node-b", 1)
	nodeA.Labels[catalog.CacheLabelKeyForModel(model)] = "ready"
	nodeB.Labels[catalog.CacheLabelKeyForModel(model)] = "loading"
	job := resources.PrefetchJob(cache, nodeB, true, "")
	job.Status.Failed = 1
	reconciler, c := newModelCacheReconciler(t, cache, nodeA, nodeB, job)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	current := &servingv1alpha1.ModelCache{}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if servingv1alpha1.ReasonFromModelCache(current.Status) == servingv1alpha1.ReasonWeightDownloadFailed {
		t.Fatal("retrying Job was marked failed")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(reconciler.clock().Now()), Message: "digest mismatch"}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(current.Status.Conditions, servingv1alpha1.ConditionReady) || servingv1alpha1.ReasonFromModelCache(current.Status) != servingv1alpha1.ReasonWeightDownloadFailed {
		t.Fatal("secondary failure hid the warm cache or failure evidence")
	}
}

func TestModelCacheDoesNotRepublishProcessedJobAfterCacheLoss(t *testing.T) {
	ctx := context.Background()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	cache := pendingModelCache(model)
	node := gpuNode("node-a", 1)
	node.UID = "node-uid"
	cache.Status.Nodes = []servingv1alpha1.ModelCacheNodeStatus{{Name: node.Name, NodeUID: string(node.UID), State: servingv1alpha1.ModelCacheNodeStateReady, ProcessedJobUID: "old-job"}}
	job := resources.PrefetchJob(cache, node, true, "")
	job.UID = "old-job"
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	reconciler, c := newModelCacheReconciler(t, cache, node, job)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[catalog.CacheLabelKeyForModel(model)] == "ready" {
		t.Fatal("historical Job republished missing cache")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("processed Job survived: %v", err)
	}
}

func TestModelCacheRejectsReplacedNodeAndUnschedulableTargets(t *testing.T) {
	ctx := context.Background()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	cache := pendingModelCache(model)
	node := gpuNode("node-a", 1)
	node.UID = "old-node"
	job := resources.PrefetchJob(cache, node, true, "")
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	node.UID = "replacement"
	reconciler, c := newModelCacheReconciler(t, cache, node, job)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[catalog.CacheLabelKeyForModel(model)] == "ready" {
		t.Fatal("replacement received old-node ready state")
	}
	node.Spec.Unschedulable = true
	if err := c.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cordoned node received a Job: %v", err)
	}
}
