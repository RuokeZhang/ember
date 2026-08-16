package controllers

import (
	"context"
	"fmt"
	"reflect"
	"time"

	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	"github.com/RuokeZhang/ember/operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func createOrUpdate(ctx context.Context, c client.Client, desired client.Object) error {
	key := client.ObjectKeyFromObject(desired)
	existing := desired.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return c.Create(ctx, desired)
		}
		return err
	}

	switch current := existing.(type) {
	case *corev1.Namespace:
		target := desired.(*corev1.Namespace)
		if reflect.DeepEqual(current.Labels, target.Labels) {
			return nil
		}
		current.Labels = target.Labels
		return c.Update(ctx, current)
	case *corev1.ResourceQuota:
		target := desired.(*corev1.ResourceQuota)
		if reflect.DeepEqual(current.Labels, target.Labels) && apiequality.Semantic.DeepEqual(current.Spec, target.Spec) {
			return nil
		}
		current.Labels = target.Labels
		current.Spec = target.Spec
		return c.Update(ctx, current)
	case *corev1.LimitRange:
		target := desired.(*corev1.LimitRange)
		if reflect.DeepEqual(current.Labels, target.Labels) && apiequality.Semantic.DeepEqual(current.Spec, target.Spec) {
			return nil
		}
		current.Labels = target.Labels
		current.Spec = target.Spec
		return c.Update(ctx, current)
	case *corev1.ServiceAccount:
		target := desired.(*corev1.ServiceAccount)
		if reflect.DeepEqual(current.Labels, target.Labels) && reflect.DeepEqual(current.AutomountServiceAccountToken, target.AutomountServiceAccountToken) {
			return nil
		}
		current.Labels = target.Labels
		current.AutomountServiceAccountToken = target.AutomountServiceAccountToken
		return c.Update(ctx, current)
	case *networkingv1.NetworkPolicy:
		target := desired.(*networkingv1.NetworkPolicy)
		if reflect.DeepEqual(current.Labels, target.Labels) && apiequality.Semantic.DeepEqual(current.Spec, target.Spec) {
			return nil
		}
		current.Labels = target.Labels
		current.Spec = target.Spec
		return c.Update(ctx, current)
	case *appsv1.Deployment:
		target := desired.(*appsv1.Deployment)
		targetSpec := target.Spec
		targetSpec.Replicas = current.Spec.Replicas
		if reflect.DeepEqual(current.Labels, target.Labels) && apiequality.Semantic.DeepEqual(current.Spec, targetSpec) {
			return nil
		}
		current.Labels = target.Labels
		current.Spec = targetSpec
		return c.Update(ctx, current)
	case *corev1.Service:
		target := desired.(*corev1.Service)
		if reflect.DeepEqual(current.Labels, target.Labels) && apiequality.Semantic.DeepEqual(current.Spec.Selector, target.Spec.Selector) && apiequality.Semantic.DeepEqual(current.Spec.Ports, target.Spec.Ports) && current.Spec.Type == target.Spec.Type {
			return nil
		}
		current.Labels = target.Labels
		current.Spec.Selector = target.Spec.Selector
		current.Spec.Ports = target.Spec.Ports
		current.Spec.Type = target.Spec.Type
		return c.Update(ctx, current)
	case *rbacv1.Role:
		target := desired.(*rbacv1.Role)
		if reflect.DeepEqual(current.Labels, target.Labels) && reflect.DeepEqual(current.Rules, target.Rules) {
			return nil
		}
		current.Labels = target.Labels
		current.Rules = target.Rules
		return c.Update(ctx, current)
	case *rbacv1.RoleBinding:
		target := desired.(*rbacv1.RoleBinding)
		if reflect.DeepEqual(current.Labels, target.Labels) && reflect.DeepEqual(current.Subjects, target.Subjects) && reflect.DeepEqual(current.RoleRef, target.RoleRef) {
			return nil
		}
		current.Labels = target.Labels
		current.Subjects = target.Subjects
		current.RoleRef = target.RoleRef
		return c.Update(ctx, current)
	case *servingv1alpha1.ModelCache:
		target := desired.(*servingv1alpha1.ModelCache)
		if reflect.DeepEqual(current.Labels, target.Labels) && apiequality.Semantic.DeepEqual(current.Spec, target.Spec) {
			return nil
		}
		current.Labels = target.Labels
		current.Spec = target.Spec
		return c.Update(ctx, current)
	case *unstructured.Unstructured:
		target := desired.(*unstructured.Unstructured)
		base := current.DeepCopy()
		labels := copyStringMap(current.GetLabels())
		if labels == nil {
			labels = map[string]string{}
		}
		for key, value := range target.GetLabels() {
			labels[key] = value
		}
		annotations := copyStringMap(current.GetAnnotations())
		if annotations == nil {
			annotations = map[string]string{}
		}
		delete(annotations, resources.KEDAPausedReplicasAnnotation)
		if value, ok := target.GetAnnotations()[resources.KEDAPausedAnnotation]; ok {
			annotations[resources.KEDAPausedAnnotation] = value
		} else {
			delete(annotations, resources.KEDAPausedAnnotation)
		}
		targetSpec := target.Object["spec"]
		if reflect.DeepEqual(current.GetLabels(), labels) && reflect.DeepEqual(current.GetAnnotations(), annotations) && reflect.DeepEqual(current.Object["spec"], targetSpec) {
			return nil
		}
		current.SetLabels(labels)
		current.SetAnnotations(annotations)
		current.Object["spec"] = targetSpec
		return c.Patch(ctx, current, client.MergeFrom(base))
	default:
		return fmt.Errorf("unsupported object type %T", desired)
	}
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func updateNodeLabel(ctx context.Context, c client.Client, nodeName, labelKey, labelValue string) error {
	node := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return err
	}
	node = node.DeepCopy()
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if labelValue == "" {
		if _, ok := node.Labels[labelKey]; !ok {
			return nil
		}
		delete(node.Labels, labelKey)
	} else {
		if node.Labels[labelKey] == labelValue {
			return nil
		}
		node.Labels[labelKey] = labelValue
	}
	return c.Update(ctx, node)
}

func deploymentRolloutReady(deployment *appsv1.Deployment, desired int32) bool {
	if desired == 0 {
		return deployment.Status.Replicas == 0
	}
	return deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.UpdatedReplicas >= desired && deployment.Status.ReadyReplicas >= desired && deployment.Status.AvailableReplicas >= desired
}

func nodeReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func gpuAllocatable(node corev1.Node) int64 {
	quantity, ok := node.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]
	if !ok {
		return 0
	}
	return quantity.Value()
}
