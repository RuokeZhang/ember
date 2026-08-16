package gateway

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RuokeZhang/ember/internal/platform"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var scaledObjectListGVK = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObjectList"}

type EndpointInspection struct {
	ObservedAt       time.Time                 `json:"observedAt"`
	EndpointUID      string                    `json:"endpointUID"`
	Namespace        string                    `json:"namespace"`
	Resources        []InspectionResource      `json:"resources"`
	Pods             []InspectionPod           `json:"pods"`
	NetworkPolicies  []InspectionNetworkPolicy `json:"networkPolicies"`
	Events           []InspectionEvent         `json:"events"`
	SecurityControls []InspectionSecurity      `json:"securityControls"`
}

type InspectionResource struct {
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Status    string            `json:"status"`
	Summary   string            `json:"summary"`
	Details   map[string]string `json:"details,omitempty"`
}

type InspectionPod struct {
	Name          string    `json:"name"`
	Phase         string    `json:"phase"`
	Ready         bool      `json:"ready"`
	Node          string    `json:"node,omitempty"`
	Image         string    `json:"image,omitempty"`
	RestartCount  int32     `json:"restartCount"`
	RequestedGPUs int64     `json:"requestedGPUs"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
}

type InspectionNetworkPolicy struct {
	Name        string   `json:"name"`
	PolicyTypes []string `json:"policyTypes"`
	Ingress     int      `json:"ingressRules"`
	Egress      int      `json:"egressRules"`
}

type InspectionEvent struct {
	Type       string    `json:"type"`
	Reason     string    `json:"reason"`
	Message    string    `json:"message"`
	Count      int32     `json:"count"`
	ObjectKind string    `json:"objectKind"`
	ObjectName string    `json:"objectName"`
	LastSeen   time.Time `json:"lastSeen"`
}

type InspectionSecurity struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Evidence string `json:"evidence"`
}

func (s *KubernetesStore) InspectEndpoint(ctx context.Context, endpoint *servingv1alpha1.InferenceEndpoint) (*EndpointInspection, error) {
	if s.Core == nil {
		return nil, fmt.Errorf("Kubernetes Core client is unavailable")
	}
	namespace := endpoint.Status.WorkloadNamespace
	if namespace == "" {
		return nil, fmt.Errorf("endpoint has no workload namespace")
	}
	selector := labels.Set{platform.LabelEndpointUID: string(endpoint.UID)}.AsSelector().String()
	result := &EndpointInspection{
		ObservedAt:  s.now(),
		EndpointUID: string(endpoint.UID),
		Namespace:   namespace,
		Resources: []InspectionResource{{
			Kind:      "Namespace",
			Name:      namespace,
			Namespace: namespace,
			Status:    "Active",
			Summary:   "Dedicated endpoint workload boundary",
		}},
	}

	quotas, err := s.Core.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list resource quotas: %w", err)
	}
	for _, quota := range quotas.Items {
		details := map[string]string{}
		for name, quantity := range quota.Status.Hard {
			details["hard."+string(name)] = quantity.String()
		}
		for name, quantity := range quota.Status.Used {
			details["used."+string(name)] = quantity.String()
		}
		result.Resources = append(result.Resources, InspectionResource{
			Kind:      "ResourceQuota",
			Name:      quota.Name,
			Namespace: namespace,
			Status:    "Enforced",
			Summary:   quotaSummary(quota.Status.Hard, quota.Status.Used),
			Details:   details,
		})
	}

	limitRanges, err := s.Core.CoreV1().LimitRanges(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list limit ranges: %w", err)
	}
	for _, limitRange := range limitRanges.Items {
		result.Resources = append(result.Resources, InspectionResource{
			Kind:      "LimitRange",
			Name:      limitRange.Name,
			Namespace: namespace,
			Status:    "Enforced",
			Summary:   "Container defaults and maximums are bounded",
		})
	}

	deployments, err := s.Core.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	for _, deployment := range deployments.Items {
		desired := int32(0)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		details := map[string]string{
			"desiredReplicas": strconv.Itoa(int(desired)),
			"readyReplicas":   strconv.Itoa(int(deployment.Status.ReadyReplicas)),
			"strategy":        string(deployment.Spec.Strategy.Type),
		}
		if node := deployment.Spec.Template.Spec.NodeSelector["ember.dev/gpu"]; node != "" {
			details["gpuClass"] = node
		}
		result.Resources = append(result.Resources, InspectionResource{
			Kind:      "Deployment",
			Name:      deployment.Name,
			Namespace: namespace,
			Status:    rolloutStatus(desired, deployment.Status.ReadyReplicas),
			Summary:   fmt.Sprintf("%d/%d replicas ready · %s strategy", deployment.Status.ReadyReplicas, desired, deployment.Spec.Strategy.Type),
			Details:   details,
		})
	}

	services, err := s.Core.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	for _, service := range services.Items {
		ports := make([]string, 0, len(service.Spec.Ports))
		for _, port := range service.Spec.Ports {
			ports = append(ports, fmt.Sprintf("%s:%d", port.Name, port.Port))
		}
		result.Resources = append(result.Resources, InspectionResource{
			Kind:      "Service",
			Name:      service.Name,
			Namespace: namespace,
			Status:    "Routable",
			Summary:   strings.Join(ports, ", "),
			Details:   map[string]string{"type": string(service.Spec.Type)},
		})
	}

	hpas, err := s.Core.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list horizontal pod autoscalers: %w", err)
	}
	for _, hpa := range hpas.Items {
		minReplicas := int32(1)
		if hpa.Spec.MinReplicas != nil {
			minReplicas = *hpa.Spec.MinReplicas
		}
		result.Resources = append(result.Resources, InspectionResource{
			Kind:      "HorizontalPodAutoscaler",
			Name:      hpa.Name,
			Namespace: namespace,
			Status:    "Active",
			Summary:   fmt.Sprintf("current %d · desired %d · range %d–%d", hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas, minReplicas, hpa.Spec.MaxReplicas),
			Details: map[string]string{
				"currentReplicas": strconv.Itoa(int(hpa.Status.CurrentReplicas)),
				"desiredReplicas": strconv.Itoa(int(hpa.Status.DesiredReplicas)),
				"minReplicas":     strconv.Itoa(int(minReplicas)),
				"maxReplicas":     strconv.Itoa(int(hpa.Spec.MaxReplicas)),
			},
		})
	}

	scaledObjects := &unstructured.UnstructuredList{}
	scaledObjects.SetGroupVersionKind(scaledObjectListGVK)
	if err := s.Client.List(ctx, scaledObjects, client.InNamespace(namespace), client.MatchingLabels{platform.LabelEndpointUID: string(endpoint.UID)}); err != nil {
		return nil, fmt.Errorf("list scaled objects: %w", err)
	}
	for _, object := range scaledObjects.Items {
		minReplicas, _, _ := unstructured.NestedInt64(object.Object, "spec", "minReplicaCount")
		maxReplicas, _, _ := unstructured.NestedInt64(object.Object, "spec", "maxReplicaCount")
		polling, _, _ := unstructured.NestedInt64(object.Object, "spec", "pollingInterval")
		paused := object.GetAnnotations()["autoscaling.keda.sh/paused"] == "true"
		status := "Watching queue depth"
		if paused {
			status = "Paused at zero"
		}
		result.Resources = append(result.Resources, InspectionResource{
			Kind:      "ScaledObject",
			Name:      object.GetName(),
			Namespace: namespace,
			Status:    status,
			Summary:   fmt.Sprintf("Prometheus scaler · range %d–%d · poll %ds", minReplicas, maxReplicas, polling),
			Details: map[string]string{
				"minReplicas":     strconv.FormatInt(minReplicas, 10),
				"maxReplicas":     strconv.FormatInt(maxReplicas, 10),
				"pollingInterval": strconv.FormatInt(polling, 10),
				"paused":          strconv.FormatBool(paused),
			},
		})
	}

	pods, err := s.Core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	for _, pod := range pods.Items {
		view := InspectionPod{
			Name:          pod.Name,
			Phase:         string(pod.Status.Phase),
			Ready:         podReady(pod),
			Node:          pod.Spec.NodeName,
			RestartCount:  podRestartCount(pod),
			RequestedGPUs: requestedGPUs(pod),
		}
		if len(pod.Spec.Containers) > 0 {
			view.Image = pod.Spec.Containers[0].Image
		}
		if pod.Status.StartTime != nil {
			view.StartedAt = pod.Status.StartTime.Time
		}
		result.Pods = append(result.Pods, view)
	}

	policies, err := s.Core.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list network policies: %w", err)
	}
	for _, policy := range policies.Items {
		types := make([]string, 0, len(policy.Spec.PolicyTypes))
		for _, policyType := range policy.Spec.PolicyTypes {
			types = append(types, string(policyType))
		}
		result.NetworkPolicies = append(result.NetworkPolicies, InspectionNetworkPolicy{
			Name:        policy.Name,
			PolicyTypes: types,
			Ingress:     len(policy.Spec.Ingress),
			Egress:      len(policy.Spec.Egress),
		})
	}

	events, err := s.Core.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	sort.Slice(events.Items, func(i, j int) bool {
		return eventLastSeen(events.Items[i]).After(eventLastSeen(events.Items[j]))
	})
	for index, event := range events.Items {
		if index == 25 {
			break
		}
		result.Events = append(result.Events, InspectionEvent{
			Type:       event.Type,
			Reason:     event.Reason,
			Message:    event.Message,
			Count:      event.Count,
			ObjectKind: event.InvolvedObject.Kind,
			ObjectName: event.InvolvedObject.Name,
			LastSeen:   eventLastSeen(event),
		})
	}

	result.SecurityControls = securityControls(deployments.Items, result.NetworkPolicies)
	sort.Slice(result.Resources, func(i, j int) bool {
		if result.Resources[i].Kind == result.Resources[j].Kind {
			return result.Resources[i].Name < result.Resources[j].Name
		}
		return result.Resources[i].Kind < result.Resources[j].Kind
	})
	sort.Slice(result.Pods, func(i, j int) bool { return result.Pods[i].Name < result.Pods[j].Name })
	sort.Slice(result.NetworkPolicies, func(i, j int) bool { return result.NetworkPolicies[i].Name < result.NetworkPolicies[j].Name })
	return result, nil
}

func quotaSummary(hard, used corev1.ResourceList) string {
	gpuName := corev1.ResourceName("requests.nvidia.com/gpu")
	gpuHard := hard[gpuName]
	gpuUsed := used[gpuName]
	podHard := hard[corev1.ResourcePods]
	podUsed := used[corev1.ResourcePods]
	return fmt.Sprintf("GPU %s/%s · Pods %s/%s", gpuUsed.String(), gpuHard.String(), podUsed.String(), podHard.String())
}

func rolloutStatus(desired, ready int32) string {
	if desired == 0 {
		return "Scaled to zero"
	}
	if ready >= desired {
		return "Ready"
	}
	return "Progressing"
}

func podReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podRestartCount(pod corev1.Pod) int32 {
	var total int32
	for _, status := range pod.Status.ContainerStatuses {
		total += status.RestartCount
	}
	return total
}

func requestedGPUs(pod corev1.Pod) int64 {
	var total int64
	for _, container := range pod.Spec.Containers {
		if quantity, ok := container.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; ok {
			total += quantity.Value()
		}
	}
	return total
}

func eventLastSeen(event corev1.Event) time.Time {
	switch {
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case !event.CreationTimestamp.IsZero():
		return event.CreationTimestamp.Time
	default:
		return time.Time{}
	}
}

func securityControls(deployments []appsv1.Deployment, policies []InspectionNetworkPolicy) []InspectionSecurity {
	controls := []InspectionSecurity{{
		Name:     "CNI policy enforcement",
		State:    "unknown",
		Evidence: "NetworkPolicy objects are declared; enforcement depends on the cluster CNI.",
	}}
	if len(deployments) == 0 {
		return append(controls, InspectionSecurity{
			Name:     "Serving pod security",
			State:    "pending",
			Evidence: "Deployment has not been created yet.",
		})
	}
	podSpec := deployments[0].Spec.Template.Spec
	automountDisabled := podSpec.AutomountServiceAccountToken != nil && !*podSpec.AutomountServiceAccountToken
	controls = append(controls, InspectionSecurity{
		Name:     "Kubernetes API credentials",
		State:    passState(automountDisabled),
		Evidence: fmt.Sprintf("automountServiceAccountToken=%t", !automountDisabled),
	})
	runAsNonRoot := podSpec.SecurityContext != nil && podSpec.SecurityContext.RunAsNonRoot != nil && *podSpec.SecurityContext.RunAsNonRoot
	controls = append(controls, InspectionSecurity{
		Name:     "Non-root execution",
		State:    passState(runAsNonRoot),
		Evidence: "Pod security context requires runAsNonRoot.",
	})
	if len(podSpec.Containers) > 0 {
		container := podSpec.Containers[0]
		readOnlyRoot := container.SecurityContext != nil && container.SecurityContext.ReadOnlyRootFilesystem != nil && *container.SecurityContext.ReadOnlyRootFilesystem
		dropsAll := false
		if container.SecurityContext != nil && container.SecurityContext.Capabilities != nil {
			for _, capability := range container.SecurityContext.Capabilities.Drop {
				if capability == "ALL" {
					dropsAll = true
				}
			}
		}
		cacheReadOnly := false
		for _, mount := range container.VolumeMounts {
			if mount.Name == "model-cache" {
				cacheReadOnly = mount.ReadOnly
			}
		}
		controls = append(controls,
			InspectionSecurity{Name: "Read-only root filesystem", State: passState(readOnlyRoot), Evidence: "Engine container root filesystem is immutable."},
			InspectionSecurity{Name: "Linux capabilities", State: passState(dropsAll), Evidence: "Engine container drops ALL capabilities."},
			InspectionSecurity{Name: "Model cache mount", State: passState(cacheReadOnly), Evidence: "Verified model cache is mounted read-only."},
		)
	}
	defaultDeny := false
	for _, policy := range policies {
		if policy.Name == "default-deny" {
			defaultDeny = true
		}
	}
	controls = append(controls,
		InspectionSecurity{Name: "Default-deny policy intent", State: passState(defaultDeny), Evidence: "Dedicated namespace contains an ingress and egress default-deny policy."},
		InspectionSecurity{Name: "Node-local cache exception", State: "warning", Evidence: "Serving uses a narrow read-only hostPath cache; hostile multi-tenant isolation is out of scope for the MVP."},
	)
	return controls
}

func passState(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}
