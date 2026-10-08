package controllers

import (
	"context"
	"reflect"
	"sort"
	"time"

	"github.com/RuokeZhang/ember/internal/catalog"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	"github.com/RuokeZhang/ember/operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type ModelCacheReconciler struct {
	client.Client
	DirectClient     client.Client
	APIReader        client.Reader
	Scheme           *runtime.Scheme
	ManagedNamespace string
	Clock            Clock
	SimulationMode   bool
	PrefetchImage    string
}

type endpointCacheDemand struct {
	GPUCount int64
	Replicas int64
}

func (r *ModelCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	modelCache := &servingv1alpha1.ModelCache{}
	if err := r.Client.Get(ctx, req.NamespacedName, modelCache); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	job := &batchv1.Job{}
	jobErr := r.direct().Get(ctx, client.ObjectKey{Name: resources.PrefetchJobName(modelCache), Namespace: servingv1alpha1.EmberSystemNamespace}, job)
	if jobErr != nil && !apierrors.IsNotFound(jobErr) {
		return ctrl.Result{}, jobErr
	}
	if complete, _ := jobSucceeded(job); complete {
		if err := r.direct().Get(ctx, req.NamespacedName, modelCache); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	modelCache.Default()
	originalStatus := modelCache.Status.DeepCopy()
	modelCache.Status.ObservedGeneration = modelCache.Generation
	if err := createOrUpdate(ctx, r.direct(), resources.PrefetchServiceAccount()); err != nil {
		return ctrl.Result{}, err
	}
	references, demand, endpointDemand, err := r.cacheDemand(ctx, modelCache)
	if err != nil {
		return ctrl.Result{}, err
	}
	modelCache.Status.ReferencingEndpoints = references
	if err := r.refreshNodeStatus(ctx, modelCache); err != nil {
		return ctrl.Result{}, err
	}
	labelKey := catalog.CacheLabelKey(modelCache.Spec.ModelID, modelCache.Spec.Revision)
	now := r.clock().Now()
	if jobErr == nil {
		if !job.DeletionTimestamp.IsZero() {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		target := &corev1.Node{}
		targetErr := r.Client.Get(ctx, client.ObjectKey{Name: job.Labels["ember.dev/node-name"]}, target)
		if targetErr != nil && !apierrors.IsNotFound(targetErr) {
			return ctrl.Result{}, targetErr
		}
		if targetErr != nil || string(target.UID) != job.Labels["ember.dev/node-uid"] || !nodeSchedulable(*target) || !selectorMatches(target.Labels, modelCache.Spec.NodePoolSelector) || !metav1.IsControlledBy(job, modelCache) {
			if targetErr == nil && string(target.UID) == job.Labels["ember.dev/node-uid"] && target.Labels[labelKey] == "loading" {
				if err := updateNodeLabel(ctx, r.direct(), target.Name, string(target.UID), labelKey, ""); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{RequeueAfter: time.Second}, r.direct().Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground))
		}
		if complete, completionTime := jobSucceeded(job); complete {
			processed := false
			for _, status := range modelCache.Status.Nodes {
				if status.Name == target.Name && status.ProcessedJobUID == string(job.UID) && job.UID != "" {
					processed = true
				}
			}
			if !processed {
				if err := updateNodeLabel(ctx, r.direct(), target.Name, string(target.UID), labelKey, "ready"); err != nil {
					return ctrl.Result{}, err
				}
				modelCache.Status.UpsertNode(target.Name, servingv1alpha1.ModelCacheNodeStateReady, modelCache.Spec.SizeBytes, completionTime, nil, "Verified cache materialization completed.")
				for index := range modelCache.Status.Nodes {
					if modelCache.Status.Nodes[index].Name == target.Name {
						modelCache.Status.Nodes[index].NodeUID = string(target.UID)
						modelCache.Status.Nodes[index].ProcessedJobUID = string(job.UID)
					}
				}
			}
			r.setCacheConditions(modelCache, false, "", "")
			if err := r.updateStatusIfChanged(ctx, modelCache, originalStatus); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, r.direct().Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground))
		}
		if failed, message := jobFailed(job); failed {
			if target.Labels[labelKey] == "loading" {
				if err := updateNodeLabel(ctx, r.direct(), target.Name, string(target.UID), labelKey, ""); err != nil {
					return ctrl.Result{}, err
				}
			}
			modelCache.Status.UpsertNode(target.Name, servingv1alpha1.ModelCacheNodeStateFailed, 0, nil, nil, message)
			for index := range modelCache.Status.Nodes {
				if modelCache.Status.Nodes[index].Name == target.Name {
					modelCache.Status.Nodes[index].NodeUID = string(target.UID)
				}
			}
			r.setCacheConditions(modelCache, false, servingv1alpha1.ReasonWeightDownloadFailed, message)
			if err := r.updateStatusIfChanged(ctx, modelCache, originalStatus); err != nil {
				return ctrl.Result{}, err
			}
			for _, condition := range job.Status.Conditions {
				if condition.Type == batchv1.JobFailed && !now.Before(condition.LastTransitionTime.Add(30*time.Second)) {
					return ctrl.Result{RequeueAfter: time.Second}, r.direct().Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground))
				}
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if target.Labels[labelKey] != "ready" {
			if err := updateNodeLabel(ctx, r.direct(), target.Name, string(target.UID), labelKey, "loading"); err != nil {
				return ctrl.Result{}, err
			}
			modelCache.Status.UpsertNode(target.Name, servingv1alpha1.ModelCacheNodeStateLoading, 0, nil, nil, "Cache materialization in progress.")
		}
		r.setCacheConditions(modelCache, true, "", "")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.updateStatusIfChanged(ctx, modelCache, originalStatus)
	}

	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes, client.MatchingFields{nodeModelCacheIndex: labelKey}); err != nil {
		return ctrl.Result{}, err
	}
	var capacity []int64
	var loadingNodes []corev1.Node
	for _, node := range nodes.Items {
		if !nodeSchedulable(node) || !selectorMatches(node.Labels, modelCache.Spec.NodePoolSelector) {
			continue
		}
		if node.Labels[labelKey] == "ready" {
			available, pods, err := r.nodeGPUCapacity(ctx, node)
			if err != nil {
				return ctrl.Result{}, err
			}
			for _, pod := range pods {
				uid := types.UID(pod.Labels[resources.LabelEndpointUID])
				replicas := endpointDemand[uid]
				if reusableReplica(pod, modelCache, replicas) {
					replicas.Replicas--
					endpointDemand[uid] = replicas
					demand[replicas.GPUCount]--
					if demand[replicas.GPUCount] == 0 {
						delete(demand, replicas.GPUCount)
					}
				}
			}
			capacity = append(capacity, available)
		} else if node.Labels[labelKey] == "loading" {
			loadingNodes = append(loadingNodes, node)
		}
	}
	remaining := remainingCacheDemand(demand, capacity)
	if len(remaining) == 0 && len(loadingNodes) == 0 {
		r.setCacheConditions(modelCache, false, "", "")
		return ctrl.Result{}, r.updateStatusIfChanged(ctx, modelCache, originalStatus)
	}
	eligible := &corev1.NodeList{}
	if len(loadingNodes) > 0 {
		eligible.Items = loadingNodes
	} else if err := r.Client.List(ctx, eligible, client.MatchingFields{nodeGPUPoolIndex: gpuPoolIndexKey(modelCache.Spec.NodePoolSelector)}, client.MatchingLabels(modelCache.Spec.NodePoolSelector)); err != nil {
		return ctrl.Result{}, err
	}
	sort.Slice(eligible.Items, func(i, j int) bool { return eligible.Items[i].Name < eligible.Items[j].Name })
	var target, failedTarget *corev1.Node
	var targetGPU, failedTargetGPU int64
	for index := range eligible.Items {
		node := &eligible.Items[index]
		if node.Labels[labelKey] == "ready" {
			continue
		}
		if node.Labels[labelKey] == "loading" {
			// Recover the GPU-free download even if serving capacity has since filled.
			target = node
			break
		}
		available, pods, err := r.nodeGPUCapacity(ctx, *node)
		if err != nil {
			return ctrl.Result{}, err
		}
		credited := map[types.UID]int64{}
		for _, pod := range pods {
			uid := types.UID(pod.Labels[resources.LabelEndpointUID])
			replicas := endpointDemand[uid]
			replicas.Replicas -= credited[uid]
			if reusableReplica(pod, modelCache, replicas) {
				available += replicas.GPUCount
				credited[uid]++
			}
		}
		var feasibleGPU int64
		for gpuCount := range remaining {
			if gpuCount <= available && gpuCount > feasibleGPU {
				feasibleGPU = gpuCount
			}
		}
		if feasibleGPU == 0 {
			continue
		}
		failed := false
		for _, status := range modelCache.Status.Nodes {
			if status.Name == node.Name && status.State == servingv1alpha1.ModelCacheNodeStateFailed {
				failed = true
			}
		}
		if !failed {
			if feasibleGPU > targetGPU {
				target, targetGPU = node, feasibleGPU
			}
		} else if feasibleGPU > failedTargetGPU {
			failedTarget = node
			failedTargetGPU = feasibleGPU
		}
	}
	if target == nil {
		target = failedTarget
	}
	if target == nil {
		r.setCacheConditions(modelCache, false, servingv1alpha1.ReasonInsufficientGPU, "No schedulable GPU node has enough estimated capacity for additional cache placement.")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.updateStatusIfChanged(ctx, modelCache, originalStatus)
	}
	if err := updateNodeLabel(ctx, r.direct(), target.Name, string(target.UID), labelKey, "loading"); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.direct().Create(ctx, resources.PrefetchJob(modelCache, target, r.SimulationMode, r.PrefetchImage)); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	modelCache.Status.UpsertNode(target.Name, servingv1alpha1.ModelCacheNodeStateLoading, 0, nil, nil, "Cache materialization scheduled.")
	for index := range modelCache.Status.Nodes {
		if modelCache.Status.Nodes[index].Name == target.Name {
			modelCache.Status.Nodes[index].NodeUID = string(target.UID)
		}
	}
	r.setCacheConditions(modelCache, true, "", "")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, r.updateStatusIfChanged(ctx, modelCache, originalStatus)
}

func (r *ModelCacheReconciler) cacheDemand(ctx context.Context, modelCache *servingv1alpha1.ModelCache) (int32, map[int64]int64, map[types.UID]endpointCacheDemand, error) {
	endpoints := &servingv1alpha1.InferenceEndpointList{}
	if err := r.Client.List(ctx, endpoints, client.InNamespace(r.managedNamespace()), client.MatchingFields{endpointModelIndex: modelCache.Spec.ModelID + "@" + modelCache.Spec.Revision}); err != nil {
		return 0, nil, nil, err
	}
	demand := map[int64]int64{}
	endpointDemand := map[types.UID]endpointCacheDemand{}
	var references int32
	var floorGPU int64 = 1
	for _, endpoint := range endpoints.Items {
		if !endpoint.DeletionTimestamp.IsZero() {
			continue
		}
		profile, ok := catalog.LookupProfile(string(endpoint.Spec.Profile))
		if !ok {
			continue
		}
		references++
		gpuCount := int64(profile.GPUCount)
		if gpuCount > floorGPU {
			floorGPU = gpuCount
		}
		replicas := desiredReplicasFor(&endpoint)
		deployment := &appsv1.Deployment{}
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: resources.WorkloadNamespaceName(endpoint.UID), Name: resources.EngineName}, deployment)
		if err != nil && !apierrors.IsNotFound(err) {
			return 0, nil, nil, err
		}
		if err == nil && deployment.Spec.Replicas != nil {
			replicas = *deployment.Spec.Replicas
		}
		endpointDemand[endpoint.UID] = endpointCacheDemand{GPUCount: gpuCount, Replicas: int64(replicas)}
		if replicas > 0 {
			demand[gpuCount] += int64(replicas)
		}
	}
	if len(demand) == 0 {
		demand[floorGPU] = 1
	}
	return references, demand, endpointDemand, nil
}

func remainingCacheDemand(demand map[int64]int64, capacity []int64) map[int64]int64 {
	remaining := map[int64]int64{}
	gpuCounts := make([]int64, 0, len(demand))
	for gpuCount := range demand {
		gpuCounts = append(gpuCounts, gpuCount)
	}
	sort.Slice(gpuCounts, func(i, j int) bool { return gpuCounts[i] > gpuCounts[j] })
	for _, gpuCount := range gpuCounts {
		replicas := demand[gpuCount]
		for index := range capacity {
			allocated := capacity[index] / gpuCount
			if allocated > replicas {
				allocated = replicas
			}
			capacity[index] -= allocated * gpuCount
			replicas -= allocated
		}
		if replicas > 0 {
			remaining[gpuCount] = replicas
		}
	}
	return remaining
}

func reusableReplica(pod corev1.Pod, modelCache *servingv1alpha1.ModelCache, demand endpointCacheDemand) bool {
	return demand.Replicas > 0 && pod.DeletionTimestamp.IsZero() && pod.Labels[resources.LabelManaged] == resources.ManagedValue && pod.Labels[resources.LabelCacheHash] == catalog.CacheHash(modelCache.Spec.ModelID, modelCache.Spec.Revision) && podGPURequests(&pod) == demand.GPUCount
}

func (r *ModelCacheReconciler) nodeGPUCapacity(ctx context.Context, node corev1.Node) (int64, []corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods, client.MatchingFields{podNodeIndex: node.Name}); err != nil {
		return 0, nil, err
	}
	available := gpuAllocatable(node)
	for _, pod := range pods.Items {
		available -= podGPURequests(&pod)
	}
	if available < 0 {
		available = 0
	}
	return available, pods.Items, nil
}

func (r *ModelCacheReconciler) refreshNodeStatus(ctx context.Context, modelCache *servingv1alpha1.ModelCache) error {
	labelKey := catalog.CacheLabelKey(modelCache.Spec.ModelID, modelCache.Spec.Revision)
	previous := map[string]servingv1alpha1.ModelCacheNodeStatus{}
	for _, status := range modelCache.Status.Nodes {
		node := &corev1.Node{}
		err := r.Client.Get(ctx, client.ObjectKey{Name: status.Name}, node)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if status.NodeUID != string(node.UID) || !selectorMatches(node.Labels, modelCache.Spec.NodePoolSelector) {
			continue
		}
		if !nodeReady(*node) || (node.Labels[labelKey] != "ready" && node.Labels[labelKey] != "loading") {
			if status.State != servingv1alpha1.ModelCacheNodeStateFailed {
				status.State = servingv1alpha1.ModelCacheNodeStatePending
				status.ProgressBytes = 0
			}
		}
		previous[status.Name] = status
	}
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes, client.MatchingFields{nodeModelCacheIndex: labelKey}); err != nil {
		return err
	}
	for _, node := range nodes.Items {
		if !selectorMatches(node.Labels, modelCache.Spec.NodePoolSelector) {
			continue
		}
		status := previous[node.Name]
		status.Name = node.Name
		status.NodeUID = string(node.UID)
		if node.Labels[labelKey] == "ready" {
			status.State = servingv1alpha1.ModelCacheNodeStateReady
			status.ProgressBytes = modelCache.Spec.SizeBytes
			status.Message = "Verified cache available."
		} else {
			status.State = servingv1alpha1.ModelCacheNodeStateLoading
			status.ProgressBytes = 0
			status.Message = "Cache materialization in progress."
		}
		previous[node.Name] = status
	}
	modelCache.Status.Nodes = nil
	for _, status := range previous {
		modelCache.Status.Nodes = append(modelCache.Status.Nodes, status)
	}
	sort.Slice(modelCache.Status.Nodes, func(i, j int) bool { return modelCache.Status.Nodes[i].Name < modelCache.Status.Nodes[j].Name })
	return nil
}

func (r *ModelCacheReconciler) setCacheConditions(modelCache *servingv1alpha1.ModelCache, progressing bool, failureReason, message string) {
	ready := false
	for _, node := range modelCache.Status.Nodes {
		ready = ready || node.State == servingv1alpha1.ModelCacheNodeStateReady
	}
	now := r.clock().Now()
	readyReason, readyMessage := servingv1alpha1.ReasonLoadingWeights, "Waiting for verified cache materialization."
	readyStatus := metav1.ConditionFalse
	if ready {
		readyStatus, readyReason, readyMessage = metav1.ConditionTrue, servingv1alpha1.ReasonCacheReady, "At least one node has a verified cache entry."
	} else if failureReason != "" {
		readyReason, readyMessage = failureReason, message
	}
	modelCache.Status.SetCondition(servingv1alpha1.ConditionReady, readyStatus, readyReason, readyMessage, modelCache.Generation, now)
	progressStatus, progressReason, progressMessage := metav1.ConditionFalse, servingv1alpha1.ReasonRolloutComplete, "Cache placement demand is satisfied."
	if progressing {
		progressStatus, progressReason, progressMessage = metav1.ConditionTrue, servingv1alpha1.ReasonLoadingWeights, "Cache replication is in progress."
	} else if failureReason != "" {
		progressReason, progressMessage = failureReason, message
	}
	modelCache.Status.SetCondition(servingv1alpha1.ConditionProgressing, progressStatus, progressReason, progressMessage, modelCache.Generation, now)
	degradedStatus, degradedReason, degradedMessage := metav1.ConditionFalse, servingv1alpha1.ReasonAsExpected, "ModelCache is healthy."
	if failureReason != "" {
		degradedStatus, degradedReason, degradedMessage = metav1.ConditionTrue, failureReason, message
	}
	modelCache.Status.SetCondition(servingv1alpha1.ConditionDegraded, degradedStatus, degradedReason, degradedMessage, modelCache.Generation, now)
}

func jobSucceeded(job *batchv1.Job) (bool, *metav1.Time) {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			stamp := condition.LastTransitionTime
			return true, &stamp
		}
	}
	return false, nil
}

func jobFailed(job *batchv1.Job) (bool, string) {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			if condition.Message != "" {
				return true, condition.Message
			}
			return true, "Prefetch job failed."
		}
	}
	return false, ""
}

func (r *ModelCacheReconciler) updateStatusIfChanged(ctx context.Context, modelCache *servingv1alpha1.ModelCache, original *servingv1alpha1.ModelCacheStatus) error {
	if reflect.DeepEqual(modelCache.Status, *original) {
		return nil
	}
	base := modelCache.DeepCopy()
	base.Status = *original
	return r.direct().Status().Patch(ctx, modelCache, client.MergeFrom(base))
}

func (r *ModelCacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&servingv1alpha1.ModelCache{}).Owns(&batchv1.Job{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.requestsForNode), builder.WithPredicates(nodePlacementChanges())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.requestsForPod), builder.WithPredicates(podCapacityChanges())).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.requestsForDeployment), builder.WithPredicates(predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
			previous, current := update.ObjectOld.(*appsv1.Deployment), update.ObjectNew.(*appsv1.Deployment)
			return !reflect.DeepEqual(previous.Spec.Replicas, current.Spec.Replicas) || !reflect.DeepEqual(previous.Labels, current.Labels) || !reflect.DeepEqual(previous.DeletionTimestamp, current.DeletionTimestamp)
		}})).
		Watches(&servingv1alpha1.InferenceEndpoint{}, handler.EnqueueRequestsFromMapFunc(r.requestsForEndpoint)).Complete(r)
}

func nodePlacementChanges() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
		previous, current := update.ObjectOld.(*corev1.Node), update.ObjectNew.(*corev1.Node)
		return previous.UID != current.UID || nodeReady(*previous) != nodeReady(*current) || gpuAllocatable(*previous) != gpuAllocatable(*current) || previous.Spec.Unschedulable != current.Spec.Unschedulable || !reflect.DeepEqual(previous.Labels, current.Labels) || !reflect.DeepEqual(previous.Spec.Taints, current.Spec.Taints) || !reflect.DeepEqual(previous.DeletionTimestamp, current.DeletionTimestamp)
	}}
}

func podCapacityChanges() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(create event.CreateEvent) bool { return podGPURequests(create.Object.(*corev1.Pod)) > 0 },
		DeleteFunc: func(deletion event.DeleteEvent) bool { return podGPURequests(deletion.Object.(*corev1.Pod)) > 0 },
		UpdateFunc: func(update event.UpdateEvent) bool {
			previous, current := update.ObjectOld.(*corev1.Pod), update.ObjectNew.(*corev1.Pod)
			return podGPURequests(previous) != podGPURequests(current) || previous.Spec.NodeName != current.Spec.NodeName || len(podNodeKeys(previous)) != len(podNodeKeys(current)) || !reflect.DeepEqual(previous.Labels, current.Labels) || !reflect.DeepEqual(previous.DeletionTimestamp, current.DeletionTimestamp)
		},
	}
}

func (r *ModelCacheReconciler) requestsForDeployment(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[resources.LabelManaged] != resources.ManagedValue || obj.GetLabels()[resources.LabelModelCache] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: obj.GetLabels()[resources.LabelModelCache]}}}
}

func (r *ModelCacheReconciler) requestsForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod := obj.(*corev1.Pod)
	requests := r.requestsForDeployment(ctx, pod)
	if pod.Spec.NodeName != "" {
		node := &corev1.Node{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err == nil {
			requests = append(requests, r.requestsForNode(ctx, node)...)
		}
	}
	return requests
}

func (r *ModelCacheReconciler) requestsForNode(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	caches := &servingv1alpha1.ModelCacheList{}
	if err := r.Client.List(ctx, caches); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for _, modelCache := range caches.Items {
		if selectorMatches(node.Labels, modelCache.Spec.NodePoolSelector) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&modelCache)})
		}
	}
	return requests
}

func (r *ModelCacheReconciler) requestsForEndpoint(ctx context.Context, obj client.Object) []reconcile.Request {
	endpoint, ok := obj.(*servingv1alpha1.InferenceEndpoint)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: catalog.ModelCacheName(endpoint.Spec.Model.ID, endpoint.Spec.Model.Revision)}}}
}

func (r *ModelCacheReconciler) direct() client.Client {
	if r.DirectClient != nil {
		return r.DirectClient
	}
	return r.Client
}

func (r *ModelCacheReconciler) managedNamespace() string {
	if r.ManagedNamespace != "" {
		return r.ManagedNamespace
	}
	return servingv1alpha1.EmberSystemNamespace
}

func (r *ModelCacheReconciler) clock() Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return realClock{}
}
