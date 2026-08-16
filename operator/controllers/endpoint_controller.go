package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/RuokeZhang/ember/internal/catalog"
	"github.com/RuokeZhang/ember/internal/platform"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	"github.com/RuokeZhang/ember/operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type EndpointReconciler struct {
	client.Client
	DirectClient     client.Client
	APIReader        client.Reader
	Scheme           *runtime.Scheme
	ManagedNamespace string
	Finalizer        string
	Clock            Clock
	SimulationMode   bool
	EnableKEDA       bool
}

func (r *EndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	endpoint := &servingv1alpha1.InferenceEndpoint{}
	if err := r.Client.Get(ctx, req.NamespacedName, endpoint); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.managedNamespace() != "" && endpoint.Namespace != r.managedNamespace() {
		return ctrl.Result{}, nil
	}
	if !endpoint.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, endpoint)
	}
	if !controllerutil.ContainsFinalizer(endpoint, r.finalizer()) {
		updated := endpoint.DeepCopy()
		controllerutil.AddFinalizer(updated, r.finalizer())
		if err := r.Client.Update(ctx, updated); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	originalStatus := endpoint.Status.DeepCopy()
	endpoint.Status.ObservedGeneration = endpoint.Generation
	validationErrs := endpoint.ValidateCreate()
	if len(validationErrs) > 0 {
		r.setDegraded(endpoint, servingv1alpha1.ValidationReason(validationErrs), validationErrs.ToAggregate().Error())
		return ctrl.Result{}, r.updateStatusIfChanged(ctx, endpoint, originalStatus)
	}

	model, _ := catalog.LookupModel(endpoint.Spec.Model.ID)
	profile, _ := catalog.LookupProfile(string(endpoint.Spec.Profile))
	if r.SimulationMode {
		profile = catalog.SimulationProfile(profile)
	}
	endpoint.Status.Model.ResolvedDigest = model.Digest
	endpoint.Status.Model.SizeBytes = model.SizeBytes
	endpoint.Status.WorkloadNamespace = resources.WorkloadNamespaceName(endpoint.UID)
	endpoint.Status.EndpointURL = resources.EndpointURL(endpoint)

	if err := r.ensureBaseResources(ctx, endpoint, profile); err != nil {
		r.setDegraded(endpoint, servingv1alpha1.ReasonReconciling, err.Error())
		if statusErr := r.updateStatusIfChanged(ctx, endpoint, originalStatus); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}
	modelCache, err := r.ensureModelCache(ctx, model)
	if err != nil {
		r.setDegraded(endpoint, servingv1alpha1.ReasonReconciling, err.Error())
		if statusErr := r.updateStatusIfChanged(ctx, endpoint, originalStatus); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	placement, waitReason, waitMessage, err := r.resolvePlacement(ctx, endpoint, modelCache, profile.GPUCount)
	if err != nil {
		r.setDegraded(endpoint, servingv1alpha1.ReasonReconciling, err.Error())
		if statusErr := r.updateStatusIfChanged(ctx, endpoint, originalStatus); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}
	if placement == nil {
		endpoint.Status.Placement.Node = ""
		endpoint.Status.Placement.CacheState = "Pending"
		if waitReason == servingv1alpha1.ReasonWeightDownloadFailed || waitReason == servingv1alpha1.ReasonInsufficientGPU {
			r.setDegraded(endpoint, waitReason, waitMessage)
		} else {
			r.setProgressing(endpoint, waitReason, waitMessage)
		}
		if err := r.updateStatusIfChanged(ctx, endpoint, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	endpoint.Status.Placement.Node = placement.NodeName
	endpoint.Status.Placement.CacheState = placement.CacheState
	initialReplicas := desiredReplicasFor(endpoint)
	explicitActivation := endpoint.Annotations[platform.ActivationAnnotation] != ""
	idle := r.shouldScaleToZero(endpoint) && !explicitActivation
	pauseAutoscaling := idle || explicitActivation
	if err := r.ensureServingResources(ctx, endpoint, model, profile, *placement, initialReplicas, pauseAutoscaling); err != nil {
		r.setDegraded(endpoint, servingv1alpha1.ReasonReconciling, err.Error())
		if statusErr := r.updateStatusIfChanged(ctx, endpoint, originalStatus); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	deployment := &appsv1.Deployment{}
	if err := r.direct().Get(ctx, client.ObjectKey{Name: resources.EngineName, Namespace: endpoint.Status.WorkloadNamespace}, deployment); err != nil {
		return ctrl.Result{}, err
	}
	activationRequested := explicitActivation || (!idle && endpoint.Status.LastActivityTime != nil && deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0)
	if idle {
		if err := r.scaleDeployment(ctx, deployment, 0); err != nil {
			return ctrl.Result{}, err
		}
	} else if activationRequested && deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		if err := r.scaleDeployment(ctx, deployment, initialReplicas); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.direct().Get(ctx, client.ObjectKey{Name: resources.EngineName, Namespace: endpoint.Status.WorkloadNamespace}, deployment); err != nil {
		return ctrl.Result{}, err
	}
	desiredReplicas := int32(0)
	if deployment.Spec.Replicas != nil {
		desiredReplicas = *deployment.Spec.Replicas
	}
	endpoint.Status.Replicas.Desired = desiredReplicas
	endpoint.Status.Replicas.Ready = deployment.Status.ReadyReplicas
	rolloutReady := deploymentRolloutReady(deployment, desiredReplicas)
	if explicitActivation && rolloutReady {
		if err := r.clearActivation(ctx, endpoint); err != nil {
			return ctrl.Result{}, err
		}
	}
	if desiredReplicas == 0 {
		r.setReady(endpoint, servingv1alpha1.ReasonScaledToZero, "Endpoint is healthy and scaled to zero after idle timeout.")
	} else if rolloutReady {
		r.setReady(endpoint, servingv1alpha1.ReasonEngineServing, "Mock engine reports ready on /healthz after cache verification.")
	} else if activationRequested {
		r.setProgressing(endpoint, servingv1alpha1.ReasonScalingFromZero, "Gateway activity triggered scale-from-zero; waiting for the engine to become ready.")
	} else {
		r.setProgressing(endpoint, servingv1alpha1.ReasonWarmingEngine, "Waiting for cache verification and mock engine warmup to complete.")
	}
	if err := r.updateStatusIfChanged(ctx, endpoint, originalStatus); err != nil {
		return ctrl.Result{}, err
	}
	if desiredReplicas > 0 && !rolloutReady {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if requeueAfter := r.requeueAfter(endpoint); requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *EndpointReconciler) reconcileDelete(ctx context.Context, endpoint *servingv1alpha1.InferenceEndpoint) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(endpoint, r.finalizer()) {
		return ctrl.Result{}, nil
	}
	originalStatus := endpoint.Status.DeepCopy()
	endpoint.Status.ObservedGeneration = endpoint.Generation
	endpoint.Status.WorkloadNamespace = resources.WorkloadNamespaceName(endpoint.UID)
	r.setDeleting(endpoint, "Cleaning up workload namespace and child resources.")
	if err := r.updateStatusIfChanged(ctx, endpoint, originalStatus); err != nil {
		return ctrl.Result{}, err
	}
	if r.EnableKEDA {
		scaledObject := resources.ScaledObject(endpoint, r.SimulationMode, true)
		err := r.direct().Get(ctx, client.ObjectKeyFromObject(scaledObject), scaledObject)
		if err == nil {
			if deleteErr := r.direct().Delete(ctx, scaledObject); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				return ctrl.Result{}, deleteErr
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	deployment := &appsv1.Deployment{}
	err := r.direct().Get(ctx, client.ObjectKey{Name: resources.EngineName, Namespace: endpoint.Status.WorkloadNamespace}, deployment)
	if err == nil {
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
			deployment = deployment.DeepCopy()
			zero := int32(0)
			deployment.Spec.Replicas = &zero
			if updateErr := r.direct().Update(ctx, deployment); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	pods := &corev1.PodList{}
	if err := r.direct().List(ctx, pods, client.InNamespace(endpoint.Status.WorkloadNamespace), client.MatchingLabels(resources.ManagedLabels(endpoint))); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if len(pods.Items) > 0 {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	namespace := &corev1.Namespace{}
	err = r.direct().Get(ctx, client.ObjectKey{Name: endpoint.Status.WorkloadNamespace}, namespace)
	if err == nil {
		if deleteErr := r.direct().Delete(ctx, namespace); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return ctrl.Result{}, deleteErr
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	updated := endpoint.DeepCopy()
	controllerutil.RemoveFinalizer(updated, r.finalizer())
	if err := r.Client.Update(ctx, updated); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *EndpointReconciler) ensureBaseResources(ctx context.Context, endpoint *servingv1alpha1.InferenceEndpoint, profile catalog.Profile) error {
	for _, obj := range []client.Object{resources.WorkloadNamespace(endpoint), resources.ResourceQuota(endpoint, profile), resources.LimitRange(endpoint, profile), resources.ServiceAccount(endpoint), resources.DefaultDenyNetworkPolicy(endpoint), resources.DNSNetworkPolicy(endpoint), resources.GatewayIngressNetworkPolicy(endpoint), resources.PrometheusIngressNetworkPolicy(endpoint), resources.GatewayLogRole(endpoint), resources.GatewayLogRoleBinding(endpoint)} {
		if err := createOrUpdate(ctx, r.direct(), obj); err != nil {
			return fmt.Errorf("ensure %T: %w", obj, err)
		}
	}
	return nil
}

func (r *EndpointReconciler) ensureServingResources(ctx context.Context, endpoint *servingv1alpha1.InferenceEndpoint, model catalog.Model, profile catalog.Profile, placement resources.CachePlacement, replicas int32, paused bool) error {
	for _, obj := range []client.Object{resources.Deployment(endpoint, model, profile, placement, replicas, r.SimulationMode), resources.Service(endpoint, placement.CacheHash)} {
		if err := createOrUpdate(ctx, r.direct(), obj); err != nil {
			return fmt.Errorf("ensure %T: %w", obj, err)
		}
	}
	if r.EnableKEDA {
		if err := createOrUpdate(ctx, r.direct(), resources.ScaledObject(endpoint, r.SimulationMode, paused)); err != nil {
			return fmt.Errorf("ensure KEDA ScaledObject: %w", err)
		}
	}
	return nil
}

func (r *EndpointReconciler) scaleDeployment(ctx context.Context, deployment *appsv1.Deployment, replicas int32) error {
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == replicas {
		return nil
	}
	current := &appsv1.Deployment{}
	if err := r.direct().Get(ctx, client.ObjectKeyFromObject(deployment), current); err != nil {
		return err
	}
	if current.Spec.Replicas != nil && *current.Spec.Replicas == replicas {
		return nil
	}
	base := current.DeepCopy()
	current.Spec.Replicas = &replicas
	return r.direct().Patch(ctx, current, client.MergeFrom(base))
}

func (r *EndpointReconciler) clearActivation(ctx context.Context, endpoint *servingv1alpha1.InferenceEndpoint) error {
	current := &servingv1alpha1.InferenceEndpoint{}
	if err := r.direct().Get(ctx, client.ObjectKeyFromObject(endpoint), current); err != nil {
		return err
	}
	if current.Annotations[platform.ActivationAnnotation] == "" {
		return nil
	}
	base := current.DeepCopy()
	delete(current.Annotations, platform.ActivationAnnotation)
	return r.direct().Patch(ctx, current, client.MergeFrom(base))
}

func (r *EndpointReconciler) ensureModelCache(ctx context.Context, model catalog.Model) (*servingv1alpha1.ModelCache, error) {
	cache := &servingv1alpha1.ModelCache{ObjectMeta: metav1.ObjectMeta{Name: catalog.ModelCacheNameForModel(model), Labels: map[string]string{resources.LabelManaged: resources.ManagedValue, resources.LabelComponent: "prefetch", resources.LabelCacheHash: catalog.CacheHashForModel(model)}}, Spec: servingv1alpha1.ModelCacheSpec{ModelID: model.ID, Revision: model.Revision, Digest: model.SimulationArtifact.Digest, SizeBytes: model.SimulationArtifact.SizeBytes, NodePoolSelector: catalog.CopySelector(model.NodePoolSelector), RetentionPolicy: servingv1alpha1.RetentionPolicy(catalog.DefaultRetentionPolicy)}}
	cache.Default()
	if err := createOrUpdate(ctx, r.direct(), cache); err != nil {
		return nil, err
	}
	current := &servingv1alpha1.ModelCache{}
	if err := r.direct().Get(ctx, client.ObjectKey{Name: cache.Name}, current); err != nil {
		return nil, err
	}
	return current, nil
}

func (r *EndpointReconciler) resolvePlacement(ctx context.Context, endpoint *servingv1alpha1.InferenceEndpoint, modelCache *servingv1alpha1.ModelCache, requiredGPU int32) (*resources.CachePlacement, string, string, error) {
	labelKey := catalog.CacheLabelKey(modelCache.Spec.ModelID, modelCache.Spec.Revision)
	nodes := &corev1.NodeList{}
	if err := r.direct().List(ctx, nodes); err != nil {
		return nil, "", "", err
	}
	eligibleWarm := make([]corev1.Node, 0)
	for _, node := range nodes.Items {
		if !selectorMatches(node.Labels, modelCache.Spec.NodePoolSelector) || !nodeReady(node) {
			continue
		}
		if node.Labels[labelKey] == "ready" && gpuAllocatable(node) >= int64(requiredGPU) {
			eligibleWarm = append(eligibleWarm, node)
		}
	}
	if len(eligibleWarm) > 0 {
		sort.Slice(eligibleWarm, func(i, j int) bool { return eligibleWarm[i].Name < eligibleWarm[j].Name })
		return &resources.CachePlacement{NodeName: eligibleWarm[0].Name, CacheHash: catalog.CacheHash(modelCache.Spec.ModelID, modelCache.Spec.Revision), CacheState: "Hit", ExpectedDigest: modelCache.Spec.Digest, ExpectedSize: modelCache.Spec.SizeBytes}, "", "", nil
	}
	if condition := servingv1alpha1.MessageFromModelCache(modelCache.Status); condition != "" {
		reason := servingv1alpha1.ReasonFromModelCache(modelCache.Status)
		return nil, reason, condition, nil
	}
	message := "Waiting for ModelCache materialization on a GPU node."
	if endpoint.Spec.Placement.CachePreference == servingv1alpha1.CachePreferencePreferred {
		deadline := endpoint.CreationTimestamp.Time.Add(time.Duration(endpoint.Spec.Placement.MaxColdStartFallbackSeconds) * time.Second)
		if !endpoint.CreationTimestamp.IsZero() && r.clock().Now().After(deadline) {
			message = "Preferred warm-cache deadline passed; waiting for cache controller to finish explicit cold-node materialization."
		}
	}
	return nil, servingv1alpha1.ReasonLoadingWeights, message, nil
}

func selectorMatches(labels, selector map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func desiredReplicasFor(endpoint *servingv1alpha1.InferenceEndpoint) int32 {
	if endpoint.Spec.Scaling.MinReplicas > 0 {
		return endpoint.Spec.Scaling.MinReplicas
	}
	return 1
}

func (r *EndpointReconciler) shouldScaleToZero(endpoint *servingv1alpha1.InferenceEndpoint) bool {
	if endpoint.Spec.Scaling.MinReplicas != 0 || endpoint.Status.LastActivityTime == nil {
		return false
	}
	idleDeadline := endpoint.Status.LastActivityTime.Add(time.Duration(endpoint.Spec.Scaling.IdleTimeoutSeconds) * time.Second)
	return !r.clock().Now().Before(idleDeadline)
}

func (r *EndpointReconciler) requeueAfter(endpoint *servingv1alpha1.InferenceEndpoint) time.Duration {
	if endpoint.Spec.Scaling.MinReplicas != 0 || endpoint.Status.LastActivityTime == nil {
		return 0
	}
	deadline := endpoint.Status.LastActivityTime.Add(time.Duration(endpoint.Spec.Scaling.IdleTimeoutSeconds) * time.Second)
	remaining := deadline.Sub(r.clock().Now())
	if remaining < 0 {
		return 0
	}
	if remaining > 30*time.Second {
		return 30 * time.Second
	}
	return remaining
}

func (r *EndpointReconciler) setReady(endpoint *servingv1alpha1.InferenceEndpoint, reason, message string) {
	now := r.clock().Now()
	endpoint.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionTrue, reason, message, endpoint.Generation, now)
	endpoint.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionFalse, servingv1alpha1.ReasonRolloutComplete, "Desired state has been applied.", endpoint.Generation, now)
	endpoint.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionFalse, servingv1alpha1.ReasonAsExpected, "Endpoint is healthy.", endpoint.Generation, now)
	endpoint.Status.RecomputePhase(false)
}

func (r *EndpointReconciler) setProgressing(endpoint *servingv1alpha1.InferenceEndpoint, reason, message string) {
	now := r.clock().Now()
	endpoint.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message, endpoint.Generation, now)
	endpoint.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionTrue, reason, message, endpoint.Generation, now)
	endpoint.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionFalse, servingv1alpha1.ReasonAsExpected, "Endpoint is converging toward desired state.", endpoint.Generation, now)
	endpoint.Status.RecomputePhase(false)
}

func (r *EndpointReconciler) setDegraded(endpoint *servingv1alpha1.InferenceEndpoint, reason, message string) {
	now := r.clock().Now()
	endpoint.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message, endpoint.Generation, now)
	endpoint.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionFalse, reason, "Controller cannot progress until the issue is resolved.", endpoint.Generation, now)
	endpoint.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionTrue, reason, message, endpoint.Generation, now)
	endpoint.Status.RecomputePhase(false)
}

func (r *EndpointReconciler) setDeleting(endpoint *servingv1alpha1.InferenceEndpoint, message string) {
	now := r.clock().Now()
	endpoint.Status.SetCondition(servingv1alpha1.ConditionReady, metav1.ConditionFalse, servingv1alpha1.ReasonTerminating, message, endpoint.Generation, now)
	endpoint.Status.SetCondition(servingv1alpha1.ConditionProgressing, metav1.ConditionTrue, servingv1alpha1.ReasonTerminating, message, endpoint.Generation, now)
	endpoint.Status.SetCondition(servingv1alpha1.ConditionDegraded, metav1.ConditionFalse, servingv1alpha1.ReasonAsExpected, "Deletion is in progress.", endpoint.Generation, now)
	endpoint.Status.RecomputePhase(true)
}

func (r *EndpointReconciler) updateStatusIfChanged(ctx context.Context, endpoint *servingv1alpha1.InferenceEndpoint, original *servingv1alpha1.InferenceEndpointStatus) error {
	if reflect.DeepEqual(endpoint.Status, *original) {
		return nil
	}
	base := endpoint.DeepCopy()
	base.Status = *original
	return r.direct().Status().Patch(ctx, endpoint, client.MergeFrom(base))
}

func (r *EndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&servingv1alpha1.InferenceEndpoint{}).Watches(&servingv1alpha1.ModelCache{}, handler.EnqueueRequestsFromMapFunc(r.requestsForModelCache)).Complete(r)
}

func (r *EndpointReconciler) requestsForModelCache(ctx context.Context, obj client.Object) []reconcile.Request {
	modelCache, ok := obj.(*servingv1alpha1.ModelCache)
	if !ok {
		return nil
	}
	endpoints := &servingv1alpha1.InferenceEndpointList{}
	if err := r.Client.List(ctx, endpoints, client.InNamespace(r.managedNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for _, endpoint := range endpoints.Items {
		if endpoint.Spec.Model.ID == modelCache.Spec.ModelID && endpoint.Spec.Model.Revision == modelCache.Spec.Revision {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&endpoint)})
		}
	}
	return requests
}

func (r *EndpointReconciler) direct() client.Client {
	if r.DirectClient != nil {
		return r.DirectClient
	}
	return r.Client
}

func (r *EndpointReconciler) managedNamespace() string {
	if r.ManagedNamespace != "" {
		return r.ManagedNamespace
	}
	return servingv1alpha1.EmberSystemNamespace
}

func (r *EndpointReconciler) finalizer() string {
	if r.Finalizer != "" {
		return r.Finalizer
	}
	return servingv1alpha1.FinalizerEndpointCleanup
}

func (r *EndpointReconciler) clock() Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return realClock{}
}
