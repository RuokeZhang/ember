package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/RuokeZhang/ember/internal/catalog"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	"github.com/RuokeZhang/ember/operator/internal/resources"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
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

func (r *ModelCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	modelCache := &servingv1alpha1.ModelCache{}
	if err := r.Client.Get(ctx, req.NamespacedName, modelCache); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	modelCache.Default()
	originalStatus := modelCache.Status.DeepCopy()
	modelCache.Status.ObservedGeneration = modelCache.Generation
	if err := createOrUpdate(ctx, r.direct(), resources.PrefetchServiceAccount()); err != nil {
		return ctrl.Result{}, err
	}
	modelCache.Status.ReferencingEndpoints = r.deriveReferencingEndpoints(ctx, modelCache)

	labelKey := catalog.CacheLabelKey(modelCache.Spec.ModelID, modelCache.Spec.Revision)
	nodes := &corev1.NodeList{}
	if err := r.direct().List(ctx, nodes); err != nil {
		return ctrl.Result{}, err
	}
	job := &batchv1.Job{}
	jobErr := r.direct().Get(ctx, client.ObjectKey{Name: resources.PrefetchJobName(modelCache), Namespace: servingv1alpha1.EmberSystemNamespace}, job)
	if jobErr != nil && !apierrors.IsNotFound(jobErr) {
		return ctrl.Result{}, jobErr
	}
	var readyNodes []corev1.Node
	var eligibleNodes []corev1.Node
	var loadingNodeNames []string
	for _, node := range nodes.Items {
		if !selectorMatches(node.Labels, modelCache.Spec.NodePoolSelector) || !nodeReady(node) {
			continue
		}
		if gpuAllocatable(node) > 0 {
			eligibleNodes = append(eligibleNodes, node)
		}
		switch node.Labels[labelKey] {
		case "ready":
			readyNodes = append(readyNodes, node)
		case "loading":
			loadingNodeNames = append(loadingNodeNames, node.Name)
		}
	}
	sort.Slice(readyNodes, func(i, j int) bool { return readyNodes[i].Name < readyNodes[j].Name })
	sort.Slice(eligibleNodes, func(i, j int) bool { return eligibleNodes[i].Name < eligibleNodes[j].Name })
	sort.Strings(loadingNodeNames)

	now := r.clock().Now()
	if jobErr == nil {
		if failed, message := jobFailed(job); failed {
			if target := job.Labels["ember.dev/node-name"]; target != "" {
				_ = updateNodeLabel(ctx, r.direct(), target, labelKey, "")
				modelCache.Status.UpsertNode(target, servingv1alpha1.ModelCacheNodeStateFailed, 0, nil, nil, message)
			}
			modelCache.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionFalse, servingv1alpha1.ReasonWeightDownloadFailed, message, modelCache.Generation, now)
			modelCache.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionFalse, servingv1alpha1.ReasonWeightDownloadFailed, message, modelCache.Generation, now)
			modelCache.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionTrue, servingv1alpha1.ReasonWeightDownloadFailed, message, modelCache.Generation, now)
			if err := r.updateStatusIfChanged(ctx, modelCache, originalStatus); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if succeeded, completionTime := jobSucceeded(job); succeeded {
			target := job.Labels["ember.dev/node-name"]
			if target != "" {
				if err := updateNodeLabel(ctx, r.direct(), target, labelKey, "ready"); err != nil {
					return ctrl.Result{}, err
				}
				stamp := metav1.NewTime(now)
				if completionTime != nil {
					stamp = *completionTime
				}
				modelCache.Status.UpsertNode(target, servingv1alpha1.ModelCacheNodeStateReady, modelCache.Spec.SizeBytes, &stamp, nil, "Verified cache materialization completed.")
			}
			modelCache.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionTrue, servingv1alpha1.ReasonCacheReady, "At least one node has a verified cache entry.", modelCache.Generation, now)
			modelCache.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionFalse, servingv1alpha1.ReasonRolloutComplete, "ModelCache materialization is complete.", modelCache.Generation, now)
			modelCache.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionFalse, servingv1alpha1.ReasonAsExpected, "ModelCache is healthy.", modelCache.Generation, now)
			if shouldDeleteFinishedJob(job, now) {
				_ = r.direct().Delete(ctx, job)
			}
			if err := r.updateStatusIfChanged(ctx, modelCache, originalStatus); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	if len(readyNodes) > 0 {
		for _, node := range readyNodes {
			stamp := metav1.NewTime(now)
			modelCache.Status.UpsertNode(node.Name, servingv1alpha1.ModelCacheNodeStateReady, modelCache.Spec.SizeBytes, &stamp, nil, "Warm cache available.")
		}
		modelCache.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionTrue, servingv1alpha1.ReasonCacheReady, "At least one node has a verified cache entry.", modelCache.Generation, now)
		modelCache.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionFalse, servingv1alpha1.ReasonRolloutComplete, "ModelCache materialization is complete.", modelCache.Generation, now)
		modelCache.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionFalse, servingv1alpha1.ReasonAsExpected, "ModelCache is healthy.", modelCache.Generation, now)
		if err := r.updateStatusIfChanged(ctx, modelCache, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if apierrors.IsNotFound(jobErr) && len(loadingNodeNames) > 0 {
		target := loadingNodeNames[0]
		if err := r.direct().Create(ctx, resources.PrefetchJob(modelCache, target, r.SimulationMode, r.PrefetchImage)); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	}

	if len(loadingNodeNames) > 0 || (jobErr == nil && job.Status.Active > 0) {
		for _, name := range loadingNodeNames {
			modelCache.Status.UpsertNode(name, servingv1alpha1.ModelCacheNodeStateLoading, 0, nil, nil, "Cache materialization in progress.")
		}
		modelCache.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionFalse, servingv1alpha1.ReasonLoadingWeights, "Waiting for cache materialization to finish.", modelCache.Generation, now)
		modelCache.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionTrue, servingv1alpha1.ReasonLoadingWeights, "Waiting for cache materialization to finish.", modelCache.Generation, now)
		modelCache.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionFalse, servingv1alpha1.ReasonAsExpected, "ModelCache is progressing.", modelCache.Generation, now)
		if err := r.updateStatusIfChanged(ctx, modelCache, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if len(eligibleNodes) == 0 {
		message := "No Ready GPU nodes match the model cache selector with allocatable nvidia.com/gpu."
		modelCache.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionFalse, servingv1alpha1.ReasonInsufficientGPU, message, modelCache.Generation, now)
		modelCache.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionFalse, servingv1alpha1.ReasonInsufficientGPU, message, modelCache.Generation, now)
		modelCache.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionTrue, servingv1alpha1.ReasonInsufficientGPU, message, modelCache.Generation, now)
		if err := r.updateStatusIfChanged(ctx, modelCache, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	target := eligibleNodes[0]
	if err := updateNodeLabel(ctx, r.direct(), target.Name, labelKey, "loading"); err != nil {
		return ctrl.Result{}, err
	}
	modelCache.Status.UpsertNode(target.Name, servingv1alpha1.ModelCacheNodeStateLoading, 0, nil, nil, "Cache materialization scheduled.")
	if apierrors.IsNotFound(jobErr) {
		if err := r.direct().Create(ctx, resources.PrefetchJob(modelCache, target.Name, r.SimulationMode, r.PrefetchImage)); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	}
	modelCache.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionFalse, servingv1alpha1.ReasonLoadingWeights, fmt.Sprintf("Materializing cache on node %s.", target.Name), modelCache.Generation, now)
	modelCache.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionTrue, servingv1alpha1.ReasonLoadingWeights, fmt.Sprintf("Materializing cache on node %s.", target.Name), modelCache.Generation, now)
	modelCache.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionFalse, servingv1alpha1.ReasonAsExpected, "ModelCache is progressing.", modelCache.Generation, now)
	if err := r.updateStatusIfChanged(ctx, modelCache, originalStatus); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *ModelCacheReconciler) deriveReferencingEndpoints(ctx context.Context, modelCache *servingv1alpha1.ModelCache) int32 {
	endpoints := &servingv1alpha1.InferenceEndpointList{}
	if err := r.Client.List(ctx, endpoints, client.InNamespace(r.managedNamespace())); err != nil {
		return 0
	}
	var count int32
	for _, endpoint := range endpoints.Items {
		if endpoint.DeletionTimestamp.IsZero() && endpoint.Spec.Model.ID == modelCache.Spec.ModelID && endpoint.Spec.Model.Revision == modelCache.Spec.Revision {
			count++
		}
	}
	return count
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
	if job.Status.Failed > 0 {
		return true, "Prefetch job failed."
	}
	return false, ""
}

func shouldDeleteFinishedJob(job *batchv1.Job, now time.Time) bool {
	if job.Spec.TTLSecondsAfterFinished == nil {
		return false
	}
	for _, condition := range job.Status.Conditions {
		if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) && !condition.LastTransitionTime.IsZero() {
			return now.After(condition.LastTransitionTime.Time.Add(time.Duration(*job.Spec.TTLSecondsAfterFinished) * time.Second))
		}
	}
	return false
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
	return ctrl.NewControllerManagedBy(mgr).For(&servingv1alpha1.ModelCache{}).Owns(&batchv1.Job{}).Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.requestsForNode)).Watches(&servingv1alpha1.InferenceEndpoint{}, handler.EnqueueRequestsFromMapFunc(r.requestsForEndpoint)).Complete(r)
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
