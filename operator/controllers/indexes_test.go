package controllers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/RuokeZhang/ember/internal/catalog"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestIndexedPlacementFiltersCandidatesAndTracksChanges(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	modelCache := readyModelCache(model)
	cacheHash := catalog.CacheHashForModel(model)
	unrelated := readyNode("node-a", "other-model", 4)
	insufficient := readyNode("node-b", cacheHash, 1)
	selected := readyNode("node-c", cacheHash, 2)
	wrongPool := readyNode("node-d", cacheHash, 4)
	wrongPool.Labels[catalog.DefaultGPUNodeLabelKey] = "other-pool"
	unready := readyNode("node-e", cacheHash, 4)
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	reconciler, c := newControllerClient(t, endpoint, modelCache, unrelated, insufficient, selected, wrongPool, unready)
	reconciler.Client = indexedQueryClient{Client: c}
	reconciler.DirectClient = indexedQueryClient{Client: c, rejectNodeLists: true}

	placement, _, _, err := reconciler.resolvePlacement(ctx, endpoint, modelCache, 2)
	if err != nil || placement == nil {
		t.Fatalf("expected cache placement for two GPUs, got %#v, %v", placement, err)
	}
	delete(selected.Labels, catalog.CacheLabelKeyForModel(model))
	if err := c.Update(ctx, selected); err != nil {
		t.Fatalf("remove cache label: %v", err)
	}
	placement, _, _, err = reconciler.resolvePlacement(ctx, endpoint, modelCache, 2)
	if err != nil || placement != nil {
		t.Fatalf("expected no placement after cache removal, got %#v, %v", placement, err)
	}
	selected.Labels[catalog.CacheLabelKeyForModel(model)] = "ready"
	if err := c.Update(ctx, selected); err != nil {
		t.Fatalf("restore cache label: %v", err)
	}
	placement, _, _, err = reconciler.resolvePlacement(ctx, endpoint, modelCache, 2)
	if err != nil || placement == nil {
		t.Fatalf("expected restored placement, got %#v, %v", placement, err)
	}
	if err := c.Delete(ctx, selected); err != nil {
		t.Fatalf("delete node: %v", err)
	}
	placement, _, _, err = reconciler.resolvePlacement(ctx, endpoint, modelCache, 2)
	if err != nil || placement != nil {
		t.Fatalf("expected deleted node excluded, got %#v, %v", placement, err)
	}
}

func TestNodeInformerIndexesUpdateAndRemoveEntries(t *testing.T) {
	indexer := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{
		nodeModelCacheIndex: func(obj any) ([]string, error) { return nodeModelCacheKeys(obj.(client.Object)), nil },
		nodeGPUPoolIndex:    func(obj any) ([]string, error) { return nodeGPUPoolKeys(obj.(client.Object)), nil },
	})
	node := readyNode("node-a", "model-x", 2)
	cacheKey := "cache.ember.dev/model-x"
	assertCount := func(indexName, key string, expected int) {
		t.Helper()
		objects, err := indexer.ByIndex(indexName, key)
		if err != nil || len(objects) != expected {
			t.Fatalf("index %s[%s]: got %d objects, %v; expected %d", indexName, key, len(objects), err, expected)
		}
	}
	if err := indexer.Add(node); err != nil {
		t.Fatal(err)
	}
	assertCount(nodeModelCacheIndex, cacheKey, 1)
	assertCount(nodeGPUPoolIndex, "ember.dev/gpu=l4", 1)
	updated := node.DeepCopy()
	updated.Labels[cacheKey] = "failed"
	if err := indexer.Update(updated); err != nil {
		t.Fatal(err)
	}
	assertCount(nodeModelCacheIndex, cacheKey, 0)
	updated = updated.DeepCopy()
	updated.Labels[cacheKey] = "loading"
	if err := indexer.Update(updated); err != nil {
		t.Fatal(err)
	}
	assertCount(nodeModelCacheIndex, cacheKey, 1)
	updated = updated.DeepCopy()
	updated.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := indexer.Update(updated); err != nil {
		t.Fatal(err)
	}
	assertCount(nodeModelCacheIndex, cacheKey, 0)
	assertCount(nodeGPUPoolIndex, "*", 0)
	updated = updated.DeepCopy()
	updated.Status.Conditions[0].Status = corev1.ConditionTrue
	updated.Labels[catalog.DefaultGPUNodeLabelKey] = "other-pool"
	if err := indexer.Update(updated); err != nil {
		t.Fatal(err)
	}
	assertCount(nodeGPUPoolIndex, "ember.dev/gpu=l4", 0)
	assertCount(nodeGPUPoolIndex, "ember.dev/gpu=other-pool", 1)
	if err := indexer.Delete(updated); err != nil {
		t.Fatal(err)
	}
	assertCount(nodeModelCacheIndex, cacheKey, 0)
	assertCount(nodeGPUPoolIndex, "*", 0)
}

func TestIndexedModelReferencesExcludeOtherVersionsAndNamespaces(t *testing.T) {
	ctx := context.Background()
	endpoint := testEndpoint()
	otherVersion := endpoint.DeepCopy()
	otherVersion.Name = "other-version"
	otherVersion.Spec.Model.Revision = "other-revision"
	otherNamespace := endpoint.DeepCopy()
	otherNamespace.Namespace = "other-namespace"
	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	modelCache := readyModelCache(model)
	reconciler, c := newControllerClient(t, endpoint, otherVersion, otherNamespace)
	reconciler.Client = indexedQueryClient{Client: c}
	requests := reconciler.requestsForModelCache(ctx, modelCache)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(endpoint) {
		t.Fatalf("expected only matching endpoint, got %#v", requests)
	}
	cacheReconciler := &ModelCacheReconciler{Client: reconciler.Client, ManagedNamespace: endpoint.Namespace}
	if count, _, _, err := cacheReconciler.cacheDemand(ctx, modelCache); err != nil || count != 1 {
		t.Fatalf("expected one reference, got %d", count)
	}
}

type indexedQueryClient struct {
	client.Client
	rejectNodeLists bool
}

func (c indexedQueryClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	_, nodeList := list.(*corev1.NodeList)
	_, endpointList := list.(*servingv1alpha1.InferenceEndpointList)
	if nodeList && c.rejectNodeLists {
		return fmt.Errorf("node list must use the informer cache")
	}
	if nodeList || endpointList {
		listOptions := &client.ListOptions{}
		listOptions.ApplyOptions(options)
		if listOptions.FieldSelector == nil || listOptions.FieldSelector.Empty() {
			return fmt.Errorf("resource lookup must use an index")
		}
	}
	return c.Client.List(ctx, list, options...)
}

func BenchmarkNodeCacheLookup(b *testing.B) {
	for _, nodeCount := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			indexer := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{
				nodeModelCacheIndex: func(obj any) ([]string, error) { return nodeModelCacheKeys(obj.(client.Object)), nil },
			})
			for nodeNumber := 0; nodeNumber < nodeCount; nodeNumber++ {
				cacheHash := "other-model"
				if nodeNumber < 10 {
					cacheHash = "model-x"
				}
				if err := indexer.Add(readyNode(fmt.Sprintf("node-%05d", nodeNumber), cacheHash, 2)); err != nil {
					b.Fatal(err)
				}
			}
			for _, useIndex := range []bool{false, true} {
				name := "full-scan"
				if useIndex {
					name = "indexed"
				}
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					for iteration := 0; iteration < b.N; iteration++ {
						var candidates []any
						if useIndex {
							var err error
							candidates, err = indexer.ByIndex(nodeModelCacheIndex, "cache.ember.dev/model-x")
							if err != nil {
								b.Fatal(err)
							}
						} else {
							candidates = indexer.List()
						}
						var selectedName string
						matches := 0
						for _, candidate := range candidates {
							node := candidate.(*corev1.Node).DeepCopy()
							if node.Labels["cache.ember.dev/model-x"] == "ready" && nodeReady(*node) && gpuAllocatable(*node) >= 1 {
								matches++
								if selectedName == "" || node.Name < selectedName {
									selectedName = node.Name
								}
							}
						}
						if matches != 10 || selectedName != "node-00000" {
							b.Fatalf("unexpected lookup: %d matches, selected %s", matches, selectedName)
						}
					}
				})
			}
		})
	}
}

func TestPlacementWatchesIgnoreHeartbeatButTrackResourceChanges(t *testing.T) {
	node := gpuNode("node-a", 2)
	heartbeat := node.DeepCopy()
	heartbeat.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(time.Now())
	if nodePlacementChanges().Update(event.UpdateEvent{ObjectOld: node, ObjectNew: heartbeat}) {
		t.Fatal("heartbeat triggered cache reconciliation")
	}
	cordoned := node.DeepCopy()
	cordoned.Spec.Unschedulable = true
	if !nodePlacementChanges().Update(event.UpdateEvent{ObjectOld: node, ObjectNew: cordoned}) {
		t.Fatal("cordoning was ignored")
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}}}
	unbound := pod.DeepCopy()
	unbound.Spec.NodeName = ""
	if !podCapacityChanges().Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: unbound}) {
		t.Fatal("Pod unbinding was ignored")
	}
	terminal := pod.DeepCopy()
	terminal.Status.Phase = corev1.PodSucceeded
	if !podCapacityChanges().Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: terminal}) || len(podNodeKeys(terminal)) != 0 {
		t.Fatal("terminal GPU capacity was not released")
	}
}

func TestPodGPURequestsIncludeInitSidecarsAndOverhead(t *testing.T) {
	container := func(gpus string) corev1.Container {
		return corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse(gpus)}}}
	}
	sidecar := container("1")
	restartPolicy := corev1.ContainerRestartPolicyAlways
	sidecar.RestartPolicy = &restartPolicy
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{container("1")}, InitContainers: []corev1.Container{sidecar, container("3")}, Overhead: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}
	if actual := podGPURequests(pod); actual != 5 {
		t.Fatalf("effective GPU requests = %d, want 5", actual)
	}
}
