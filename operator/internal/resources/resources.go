package resources

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/RuokeZhang/ember/internal/cachefs"
	"github.com/RuokeZhang/ember/internal/catalog"
	"github.com/RuokeZhang/ember/internal/platform"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	resourceapi "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	LabelManaged     = platform.LabelManaged
	LabelEndpointUID = platform.LabelEndpointUID
	LabelOwner       = platform.LabelOwner
	LabelAdmission   = "ember.dev/admission-policy"
	LabelComponent   = platform.LabelComponent
	LabelModelCache  = "ember.dev/model-cache"
	LabelCacheHash   = "ember.dev/cache-hash"

	ManagedValue   = "true"
	AdmissionValue = "hostpath-cache-only"

	NamespacePSAEnforce = "pod-security.kubernetes.io/enforce"
	NamespacePSAAudit   = "pod-security.kubernetes.io/audit"
	NamespacePSAWarn    = "pod-security.kubernetes.io/warn"

	EngineName                   = platform.EngineName
	EnginePort                   = 8000
	PrefetchServiceAccountName   = "ember-prefetch"
	PrefetchImage                = "ember-prefetch:dev"
	PrefetchJobTTLSeconds        = int32(300)
	ScaledObjectName             = "engine-autoscaler"
	KEDAPausedAnnotation         = "autoscaling.keda.sh/paused"
	KEDAPausedReplicasAnnotation = "autoscaling.keda.sh/paused-replicas"
	PrometheusAddress            = "http://ember-prometheus.ember-system.svc.cluster.local:9090"
	SimulationTokenDelay         = "1s"
)

var ScaledObjectGVK = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}

type CachePlacement struct {
	NodeName       string
	CacheHash      string
	CacheState     string
	ExpectedDigest string
	ExpectedSize   int64
}

func WorkloadNamespaceName(uid types.UID) string {
	prefix := string(uid)
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return "ember-ep-" + prefix
}

func ManagedLabels(endpoint *servingv1alpha1.InferenceEndpoint) map[string]string {
	return map[string]string{
		LabelManaged:     ManagedValue,
		LabelEndpointUID: string(endpoint.UID),
		LabelOwner:       endpoint.Spec.OwnerID,
	}
}

func LabelsForObject(endpoint *servingv1alpha1.InferenceEndpoint) map[string]string {
	labels := ManagedLabels(endpoint)
	labels[LabelComponent] = EngineName
	return labels
}

func NamespaceLabels(endpoint *servingv1alpha1.InferenceEndpoint) map[string]string {
	labels := ManagedLabels(endpoint)
	labels[LabelAdmission] = AdmissionValue
	labels[NamespacePSAEnforce] = "privileged"
	labels[NamespacePSAAudit] = "privileged"
	labels[NamespacePSAWarn] = "privileged"
	return labels
}

func WorkloadNamespace(endpoint *servingv1alpha1.InferenceEndpoint) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   WorkloadNamespaceName(endpoint.UID),
			Labels: NamespaceLabels(endpoint),
		},
	}
}

func ResourceQuota(endpoint *servingv1alpha1.InferenceEndpoint, profile catalog.Profile) *corev1.ResourceQuota {
	maxReplicas := endpoint.Spec.Scaling.MaxReplicas
	if maxReplicas < 1 {
		maxReplicas = 1
	}
	gpuQuota := resourceapi.MustParse(strconv.Itoa(int(maxReplicas * profile.GPUCount)))
	memoryQuota := resourceapi.MustParse(profile.MemoryRequest)
	memoryQuota.Set(memoryQuota.Value() * int64(maxReplicas))

	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "endpoint-quota",
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourcePods:                            resourceapi.MustParse(strconv.Itoa(int(maxReplicas + 1))),
				corev1.ResourceRequestsMemory:                  memoryQuota,
				corev1.ResourceName("requests.nvidia.com/gpu"): gpuQuota,
				corev1.ResourceName("count/jobs.batch"):        resourceapi.MustParse("1"),
			},
		},
	}
}

func LimitRange(endpoint *servingv1alpha1.InferenceEndpoint, profile catalog.Profile) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "endpoint-limits",
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{{
				Type: corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU:    resourceapi.MustParse(profile.CPURequest),
					corev1.ResourceMemory: resourceapi.MustParse(profile.MemoryRequest),
				},
				Default: corev1.ResourceList{
					corev1.ResourceCPU:    resourceapi.MustParse(profile.CPULimit),
					corev1.ResourceMemory: resourceapi.MustParse(profile.MemoryLimit),
				},
				Max: corev1.ResourceList{
					corev1.ResourceCPU:    resourceapi.MustParse(profile.CPULimit),
					corev1.ResourceMemory: resourceapi.MustParse(profile.MemoryLimit),
				},
			}},
		},
	}
}

func ServiceAccount(endpoint *servingv1alpha1.InferenceEndpoint) *corev1.ServiceAccount {
	disabled := false
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      EngineName,
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		AutomountServiceAccountToken: &disabled,
	}
}

func PrefetchServiceAccount() *corev1.ServiceAccount {
	disabled := false
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PrefetchServiceAccountName,
			Namespace: servingv1alpha1.EmberSystemNamespace,
			Labels: map[string]string{
				LabelManaged:   ManagedValue,
				LabelComponent: "prefetch",
			},
		},
		AutomountServiceAccountToken: &disabled,
	}
}

func DefaultDenyNetworkPolicy(endpoint *servingv1alpha1.InferenceEndpoint) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-deny",
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

func DNSNetworkPolicy(endpoint *servingv1alpha1.InferenceEndpoint) *networkingv1.NetworkPolicy {
	port53 := intstr.FromInt(53)
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-dns-egress",
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Port: &port53, Protocol: &udp}, {Port: &port53, Protocol: &tcp}},
			}},
		},
	}
}

func GatewayIngressNetworkPolicy(endpoint *servingv1alpha1.InferenceEndpoint) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-gateway-ingress",
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": servingv1alpha1.EmberSystemNamespace}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: "gateway"}},
				}},
			}},
		},
	}
}

func PrometheusIngressNetworkPolicy(endpoint *servingv1alpha1.InferenceEndpoint) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-prometheus-ingress",
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: EngineName}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": servingv1alpha1.EmberSystemNamespace}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: "prometheus"}},
				}},
			}},
		},
	}
}

func GatewayLogRole(endpoint *servingv1alpha1.InferenceEndpoint) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ember-gateway-engine-reader",
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods", "services", "resourcequotas", "limitranges", "events"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"autoscaling"}, Resources: []string{"horizontalpodautoscalers"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"keda.sh"}, Resources: []string{"scaledobjects"}, Verbs: []string{"get", "list"}},
		},
	}
}

func GatewayLogRoleBinding(endpoint *servingv1alpha1.InferenceEndpoint) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ember-gateway-engine-reader",
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      platform.GatewayServiceAccountName,
			Namespace: servingv1alpha1.EmberSystemNamespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     "ember-gateway-engine-reader",
		},
	}
}

func Deployment(endpoint *servingv1alpha1.InferenceEndpoint, model catalog.Model, profile catalog.Profile, placement CachePlacement, replicas int32, simulationMode bool, prefetchImage string) *appsv1.Deployment {
	runAsUser := int64(65532)
	runAsGroup := int64(65532)
	replicasCopy := replicas
	falseValue := false
	directory := corev1.HostPathDirectory
	directoryOrCreate := corev1.HostPathDirectoryOrCreate
	labels := LabelsForObject(endpoint)
	labels[LabelCacheHash] = placement.CacheHash
	prefetchImage = configuredPrefetchImage(prefetchImage)
	engineImage := model.EngineImageForMode(simulationMode)
	healthPath := "/health"
	engineArgs := []string{
		"--model", "/models/cache",
		"--served-model-name", model.ServedModelName,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(EnginePort),
		"--tensor-parallel-size", strconv.Itoa(int(profile.GPUCount)),
		"--quantization", model.Quantization,
		"--load-format", "safetensors",
		"--max-model-len", strconv.Itoa(int(model.MaxModelLength)),
		"--gpu-memory-utilization", "0.90",
	}
	engineEnv := []corev1.EnvVar{
		{Name: "HOME", Value: "/tmp"},
		{Name: "TMPDIR", Value: "/tmp"},
		{Name: "HF_HOME", Value: "/tmp/huggingface"},
		{Name: "HF_HUB_OFFLINE", Value: "1"},
		{Name: "TRANSFORMERS_OFFLINE", Value: "1"},
		{Name: "XDG_CACHE_HOME", Value: "/tmp/cache"},
		{Name: "TORCH_HOME", Value: "/tmp/torch"},
		{Name: "CUDA_CACHE_PATH", Value: "/tmp/cuda"},
		{Name: "VLLM_CACHE_ROOT", Value: "/tmp/vllm"},
		{Name: "VLLM_NO_USAGE_STATS", Value: "1"},
		{Name: "DO_NOT_TRACK", Value: "1"},
	}
	var startupProbe *corev1.Probe
	if simulationMode {
		healthPath = "/healthz"
		engineArgs = nil
		engineEnv = []corev1.EnvVar{
			{Name: "MODEL_ID", Value: model.ID},
			{Name: "MODEL_REVISION", Value: model.Revision},
			{Name: "MODEL_PATH", Value: "/models/cache"},
			{Name: "LOAD_DELAY", Value: fmt.Sprintf("%ds", model.LoadDelaySeconds)},
			{Name: "MOCK_RESPONSE", Value: "Ember reconciled this GPU inference endpoint successfully."},
			{Name: "TOKEN_DELAY", Value: SimulationTokenDelay},
		}
	} else {
		startupProbe = &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: healthPath, Port: intstr.FromInt(EnginePort)}},
			PeriodSeconds:    10,
			TimeoutSeconds:   5,
			FailureThreshold: 90,
		}
	}

	podSpec := corev1.PodSpec{
		ServiceAccountName:           EngineName,
		AutomountServiceAccountToken: &falseValue,
		NodeSelector:                 map[string]string{catalog.DefaultGPUNodeLabelKey: catalog.DefaultGPUNodeLabelValue},
		Tolerations: []corev1.Toleration{{
			Key:      "nvidia.com/gpu",
			Operator: corev1.TolerationOpEqual,
			Value:    "present",
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr(true),
			RunAsUser:      &runAsUser,
			RunAsGroup:     &runAsGroup,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		InitContainers: []corev1.Container{{
			Name:            "verify-cache",
			Image:           prefetchImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Args: []string{
				"--verify-only",
				"--root", catalog.CacheRoot,
				"--cache-hash", placement.CacheHash,
				"--expected-digest", placement.ExpectedDigest,
				"--expected-size", strconv.FormatInt(placement.ExpectedSize, 10),
			},
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             ptr(true),
				AllowPrivilegeEscalation: ptr(false),
				ReadOnlyRootFilesystem:   ptr(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resourceapi.MustParse("50m"), corev1.ResourceMemory: resourceapi.MustParse("64Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resourceapi.MustParse("250m"), corev1.ResourceMemory: resourceapi.MustParse("128Mi")},
			},
			VolumeMounts: []corev1.VolumeMount{{Name: "cache-root", MountPath: catalog.CacheRoot, ReadOnly: true}, {Name: "tmp", MountPath: "/tmp"}},
		}},
		Containers: []corev1.Container{{
			Name:            EngineName,
			Image:           engineImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: EnginePort}},
			Args:            engineArgs,
			Env:             engineEnv,
			StartupProbe:    startupProbe,
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: healthPath, Port: intstr.FromInt(EnginePort)}},
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				TimeoutSeconds:      2,
				FailureThreshold:    12,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: healthPath, Port: intstr.FromInt(EnginePort)}},
				InitialDelaySeconds: 15,
				PeriodSeconds:       10,
				TimeoutSeconds:      2,
				FailureThreshold:    6,
			},
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             ptr(true),
				AllowPrivilegeEscalation: ptr(false),
				ReadOnlyRootFilesystem:   ptr(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}, {Name: "dev-shm", MountPath: "/dev/shm"}, {Name: "model-cache", MountPath: "/models/cache", ReadOnly: true}},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:                    resourceapi.MustParse(profile.CPURequest),
					corev1.ResourceMemory:                 resourceapi.MustParse(profile.MemoryRequest),
					corev1.ResourceName("nvidia.com/gpu"): resourceapi.MustParse(strconv.Itoa(int(profile.GPUCount))),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:                    resourceapi.MustParse(profile.CPULimit),
					corev1.ResourceMemory:                 resourceapi.MustParse(profile.MemoryLimit),
					corev1.ResourceName("nvidia.com/gpu"): resourceapi.MustParse(strconv.Itoa(int(profile.GPUCount))),
				},
			},
		}},
		Volumes: []corev1.Volume{{
			Name:         "tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}, {
			Name:         "dev-shm",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: quantityPtr(profile.ShmSize)}},
		}, {
			Name:         "cache-root",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: catalog.CacheRoot, Type: &directoryOrCreate}},
		}, {
			Name:         "model-cache",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: catalog.CacheRoot + "/" + placement.CacheHash, Type: &directory}},
		}},
	}
	if placement.NodeName != "" {
		podSpec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{placement.NodeName}}}}}}}}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      EngineName,
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicasCopy,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: podSpec},
		},
	}
}

func Service(endpoint *servingv1alpha1.InferenceEndpoint, cacheHash string) *corev1.Service {
	selector := LabelsForObject(endpoint)
	selector[LabelCacheHash] = cacheHash
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      EngineName,
			Namespace: WorkloadNamespaceName(endpoint.UID),
			Labels:    ManagedLabels(endpoint),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports:    []corev1.ServicePort{{Name: "http", Port: EnginePort, TargetPort: intstr.FromInt(EnginePort)}},
		},
	}
}

func ScaledObject(endpoint *servingv1alpha1.InferenceEndpoint, simulationMode, paused bool) *unstructured.Unstructured {
	minReplicas := endpoint.Spec.Scaling.MinReplicas
	if minReplicas < 1 {
		minReplicas = 1
	}
	pollingInterval := int64(15)
	if simulationMode {
		pollingInterval = 2
	}
	metricSuffix := strings.ReplaceAll(string(endpoint.UID), "-", "")
	if len(metricSuffix) > 12 {
		metricSuffix = metricSuffix[:12]
	}
	query := fmt.Sprintf(`(sum(vllm:num_requests_waiting{namespace=%q,endpoint_uid=%q}) / clamp_min(count(vllm:num_requests_waiting{namespace=%q,endpoint_uid=%q}), 1)) or vector(0)`, WorkloadNamespaceName(endpoint.UID), string(endpoint.UID), WorkloadNamespaceName(endpoint.UID), string(endpoint.UID))
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1",
		"kind":       "ScaledObject",
		"metadata": map[string]any{
			"name":      ScaledObjectName,
			"namespace": WorkloadNamespaceName(endpoint.UID),
			"labels":    stringMapToAny(ManagedLabels(endpoint)),
		},
		"spec": map[string]any{
			"scaleTargetRef": map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"name":       EngineName,
			},
			"pollingInterval": pollingInterval,
			"cooldownPeriod":  int64(60),
			"minReplicaCount": int64(minReplicas),
			"maxReplicaCount": int64(endpoint.Spec.Scaling.MaxReplicas),
			"advanced": map[string]any{
				"horizontalPodAutoscalerConfig": map[string]any{
					"behavior": map[string]any{
						"scaleDown": map[string]any{"stabilizationWindowSeconds": int64(60)},
						"scaleUp":   map[string]any{"stabilizationWindowSeconds": int64(0)},
					},
				},
			},
			"triggers": []any{map[string]any{
				"type": "prometheus",
				"metadata": map[string]any{
					"serverAddress":       PrometheusAddress,
					"metricName":          "ember_queue_depth_" + metricSuffix,
					"query":               query,
					"threshold":           strconv.Itoa(int(endpoint.Spec.Scaling.TargetQueueDepth)),
					"activationThreshold": "0",
					"ignoreNullValues":    "false",
				},
			}},
		},
	}}
	object.SetGroupVersionKind(ScaledObjectGVK)
	if paused {
		object.SetAnnotations(map[string]string{KEDAPausedAnnotation: "true"})
	}
	return object
}

func ModelCacheLabels(modelCache *servingv1alpha1.ModelCache) map[string]string {
	return map[string]string{
		LabelManaged:    ManagedValue,
		LabelComponent:  "prefetch",
		LabelModelCache: modelCache.Name,
		LabelCacheHash:  catalog.CacheHash(modelCache.Spec.ModelID, modelCache.Spec.Revision),
	}
}

func PrefetchJob(modelCache *servingv1alpha1.ModelCache, nodeName string, simulationMode bool, prefetchImage string) *batchv1.Job {
	falseValue := false
	rootUser := int64(0)
	rootGroup := int64(0)
	runAsUser := cachefs.RuntimeUserID
	runAsGroup := cachefs.RuntimeGroupID
	cacheHash := catalog.CacheHash(modelCache.Spec.ModelID, modelCache.Spec.Revision)
	prefetchImage = configuredPrefetchImage(prefetchImage)
	directoryOrCreate := corev1.HostPathDirectoryOrCreate
	labels := ModelCacheLabels(modelCache)
	labels["ember.dev/node-name"] = nodeName
	args := []string{
		"--root", catalog.CacheRoot,
		"--cache-hash", cacheHash,
		"--expected-digest", modelCache.Spec.Digest,
		"--expected-size", strconv.FormatInt(modelCache.Spec.SizeBytes, 10),
	}

	if simulationMode {
		args = append(args, "--synthetic")
	} else {
		args = append(args, "--model-id", modelCache.Spec.ModelID, "--revision", modelCache.Spec.Revision)
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PrefetchJobName(modelCache),
			Namespace: servingv1alpha1.EmberSystemNamespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: servingv1alpha1.SchemeGroupVersion.String(),
				Kind:       "ModelCache",
				Name:       modelCache.Name,
				UID:        modelCache.UID,
				Controller: ptr(true),
			}},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr(PrefetchJobTTLSeconds),
			BackoffLimit:            ptr(int32(1)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           PrefetchServiceAccountName,
					AutomountServiceAccountToken: &falseValue,
					RestartPolicy:                corev1.RestartPolicyNever,
					NodeSelector:                 map[string]string{"kubernetes.io/hostname": nodeName},
					Tolerations: []corev1.Toleration{{
						Key:      "nvidia.com/gpu",
						Operator: corev1.TolerationOpEqual,
						Value:    "present",
						Effect:   corev1.TaintEffectNoSchedule,
					}},
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr(true), RunAsUser: &runAsUser, RunAsGroup: &runAsGroup, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					InitContainers: []corev1.Container{{
						Name:            "prepare-cache-root",
						Image:           prefetchImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            []string{"--prepare-root", "--root", catalog.CacheRoot},
						SecurityContext: &corev1.SecurityContext{RunAsNonRoot: ptr(false), RunAsUser: &rootUser, RunAsGroup: &rootGroup, AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"CHOWN"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
						Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resourceapi.MustParse("10m"), corev1.ResourceMemory: resourceapi.MustParse("8Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resourceapi.MustParse("50m"), corev1.ResourceMemory: resourceapi.MustParse("32Mi")}},
						VolumeMounts:    []corev1.VolumeMount{{Name: "cache-root", MountPath: catalog.CacheRoot}},
					}},
					Containers: []corev1.Container{{
						Name:            "prefetch",
						Image:           prefetchImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            args,
						SecurityContext: &corev1.SecurityContext{RunAsNonRoot: ptr(true), AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
						Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resourceapi.MustParse("100m"), corev1.ResourceMemory: resourceapi.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resourceapi.MustParse("500m"), corev1.ResourceMemory: resourceapi.MustParse("512Mi")}},
						VolumeMounts:    []corev1.VolumeMount{{Name: "cache-root", MountPath: catalog.CacheRoot}, {Name: "tmp", MountPath: "/tmp"}},
					}},
					Volumes: []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}, {Name: "cache-root", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: catalog.CacheRoot, Type: &directoryOrCreate}}}},
				},
			},
		},
	}
}

func configuredPrefetchImage(image string) string {
	if strings.TrimSpace(image) == "" {
		return PrefetchImage
	}
	return image
}

func PrefetchJobName(modelCache *servingv1alpha1.ModelCache) string {
	return "prefetch-" + catalog.CacheHash(modelCache.Spec.ModelID, modelCache.Spec.Revision)
}

func EndpointURL(endpoint *servingv1alpha1.InferenceEndpoint) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", EngineName, WorkloadNamespaceName(endpoint.UID), EnginePort)
}

func ptr[T any](value T) *T {
	return &value
}

func stringMapToAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func quantityPtr(value string) *resourceapi.Quantity {
	q := resourceapi.MustParse(value)
	return &q
}
