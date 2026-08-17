package resources

import (
	"strings"
	"testing"

	"github.com/RuokeZhang/ember/internal/catalog"
	servingv1alpha1 "github.com/RuokeZhang/ember/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestWorkloadNamespaceLabelsAndQuota(t *testing.T) {
	endpoint := validEndpoint()
	profile, _ := catalog.LookupProfile("standard")

	ns := WorkloadNamespace(endpoint)
	if ns.Name != "ember-ep-12345678" {
		t.Fatalf("expected deterministic namespace name, got %q", ns.Name)
	}
	if ns.Labels[NamespacePSAEnforce] != "privileged" || ns.Labels[LabelAdmission] == "" {
		t.Fatalf("expected privileged PSA and admission label, got %#v", ns.Labels)
	}

	quota := ResourceQuota(endpoint, profile)
	gpuQuota := quota.Spec.Hard["requests.nvidia.com/gpu"]
	if got := gpuQuota.String(); got != "3" {
		t.Fatalf("expected gpu quota 3, got %q", got)
	}
	jobsQuota := quota.Spec.Hard["count/jobs.batch"]
	if got := jobsQuota.String(); got != "1" {
		t.Fatalf("expected jobs quota 1, got %q", got)
	}
}

func TestDeploymentSecurityAndCacheVerification(t *testing.T) {
	endpoint := validEndpoint()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	profile, _ := catalog.LookupProfile("tp2")
	placement := CachePlacement{NodeName: "node-a", CacheHash: catalog.CacheHashForModel(model), CacheState: "Hit", ExpectedDigest: model.SimulationArtifact.Digest, ExpectedSize: model.SimulationArtifact.SizeBytes}

	deployment := Deployment(endpoint, model, profile, placement, 1, true, PrefetchImage)
	container := deployment.Spec.Template.Spec.Containers[0]
	initContainer := deployment.Spec.Template.Spec.InitContainers[0]

	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("expected GPU-safe recreate strategy, got %q", deployment.Spec.Strategy.Type)
	}
	if deployment.Spec.Template.Spec.NodeSelector["ember.dev/gpu"] != "l4" {
		t.Fatalf("expected l4 node selector, got %#v", deployment.Spec.Template.Spec.NodeSelector)
	}
	if deployment.Spec.Template.Spec.Affinity == nil || deployment.Spec.Template.Spec.Affinity.NodeAffinity == nil {
		t.Fatal("expected required node affinity to warm cache node")
	}
	gpuRequest := container.Resources.Requests["nvidia.com/gpu"]
	if got := gpuRequest.String(); got != "2" {
		t.Fatalf("expected gpu request 2, got %q", got)
	}
	if container.SecurityContext == nil || !*container.SecurityContext.ReadOnlyRootFilesystem || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("expected hardened container security context, got %#v", container.SecurityContext)
	}
	if len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("expected ALL capabilities dropped, got %#v", container.SecurityContext.Capabilities)
	}
	if len(container.VolumeMounts) != 3 {
		t.Fatalf("expected /tmp, /dev/shm, and cache mounts, got %#v", container.VolumeMounts)
	}
	if got := deployment.Spec.Template.Spec.Volumes[1].EmptyDir.SizeLimit.String(); got != "8Gi" {
		t.Fatalf("expected memory-backed /dev/shm size 8Gi, got %q", got)
	}
	if initContainer.Image != PrefetchImage {
		t.Fatalf("expected verify init container image %q, got %q", PrefetchImage, initContainer.Image)
	}
	if len(initContainer.Args) < 6 || initContainer.Args[0] != "--verify-only" {
		t.Fatalf("expected verify-only init container args, got %#v", initContainer.Args)
	}
	if !containsEnv(container.Env, "TOKEN_DELAY", SimulationTokenDelay) {
		t.Fatalf("expected deterministic simulation token delay, got %#v", container.Env)
	}
	if container.Image != model.SimulationImage || len(container.Args) != 0 {
		t.Fatalf("expected unchanged mock runtime, image=%q args=%#v", container.Image, container.Args)
	}
}

func TestRealDeploymentUsesPinnedOfflineVLLMRuntime(t *testing.T) {
	endpoint := validEndpoint()
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	profile, _ := catalog.LookupProfile("standard")
	placement := CachePlacement{NodeName: "node-a", CacheHash: catalog.CacheHashForModel(model), CacheState: "Hit", ExpectedDigest: model.Digest, ExpectedSize: model.SizeBytes}

	deployment := Deployment(endpoint, model, profile, placement, 1, false, "registry.example/ember-prefetch@sha256:1234")
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Image != model.EngineImage || !strings.Contains(container.Image, "@sha256:") {
		t.Fatalf("expected digest-pinned real runtime, got %q", container.Image)
	}
	for _, expected := range []string{"--model", "/models/cache", "--served-model-name", model.ServedModelName, "--tensor-parallel-size", "1", "--quantization", "awq", "--load-format", "safetensors", "--max-model-len", "32768"} {
		if !contains(container.Args, expected) {
			t.Fatalf("real runtime args missing %q: %#v", expected, container.Args)
		}
	}
	if contains(container.Args, "--trust-remote-code") {
		t.Fatalf("real runtime must not trust remote model code: %#v", container.Args)
	}
	for _, env := range []struct {
		name  string
		value string
	}{
		{name: "HF_HUB_OFFLINE", value: "1"},
		{name: "TRANSFORMERS_OFFLINE", value: "1"},
		{name: "VLLM_NO_USAGE_STATS", value: "1"},
		{name: "HOME", value: "/tmp"},
	} {
		if !containsEnv(container.Env, env.name, env.value) {
			t.Fatalf("real runtime env missing %s=%s: %#v", env.name, env.value, container.Env)
		}
	}
	if container.StartupProbe == nil || container.StartupProbe.HTTPGet.Path != "/health" {
		t.Fatalf("expected bounded vLLM startup probe, got %#v", container.StartupProbe)
	}
	if container.ReadinessProbe.HTTPGet.Path != "/health" || container.LivenessProbe.HTTPGet.Path != "/health" {
		t.Fatalf("expected vLLM health probes, readiness=%#v liveness=%#v", container.ReadinessProbe, container.LivenessProbe)
	}
}

func TestPrefetchJobSecurityAndArgs(t *testing.T) {
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	cache := &servingv1alpha1.ModelCache{ObjectMeta: metav1.ObjectMeta{Name: catalog.ModelCacheNameForModel(model), UID: types.UID("cache-uid")}, Spec: servingv1alpha1.ModelCacheSpec{ModelID: model.ID, Revision: model.Revision, Digest: model.SimulationArtifact.Digest, SizeBytes: model.SimulationArtifact.SizeBytes}}

	job := PrefetchJob(cache, "node-a", true, PrefetchImage)
	if job.Spec.Template.Spec.ServiceAccountName != PrefetchServiceAccountName {
		t.Fatalf("expected prefetch SA %q, got %q", PrefetchServiceAccountName, job.Spec.Template.Spec.ServiceAccountName)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != PrefetchJobTTLSeconds {
		t.Fatalf("expected job TTL %d, got %#v", PrefetchJobTTLSeconds, job.Spec.TTLSecondsAfterFinished)
	}
	if len(job.Spec.Template.Spec.Tolerations) != 1 || job.Spec.Template.Spec.Tolerations[0].Key != "nvidia.com/gpu" {
		t.Fatalf("prefetch job must tolerate the GPU node taint: %#v", job.Spec.Template.Spec.Tolerations)
	}
	if len(job.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected one cache-root init container, got %#v", job.Spec.Template.Spec.InitContainers)
	}
	initContainer := job.Spec.Template.Spec.InitContainers[0]
	if initContainer.Name != "prepare-cache-root" || !contains(initContainer.Args, "--prepare-root") {
		t.Fatalf("expected narrowly scoped cache-root preparation, got %#v", initContainer)
	}
	if initContainer.SecurityContext == nil || initContainer.SecurityContext.RunAsUser == nil || *initContainer.SecurityContext.RunAsUser != 0 {
		t.Fatalf("cache-root preparation must explicitly run as root: %#v", initContainer.SecurityContext)
	}
	if len(initContainer.SecurityContext.Capabilities.Add) != 1 || initContainer.SecurityContext.Capabilities.Add[0] != "CHOWN" {
		t.Fatalf("cache-root preparation must retain only CHOWN: %#v", initContainer.SecurityContext.Capabilities)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != PrefetchImage {
		t.Fatalf("expected prefetch image %q, got %q", PrefetchImage, container.Image)
	}
	if !contains(container.Args, "--synthetic") {
		t.Fatalf("expected synthetic mode args, got %#v", container.Args)
	}
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "node-a" {
		t.Fatalf("expected pinned node selector, got %#v", job.Spec.Template.Spec.NodeSelector)
	}
	if container.SecurityContext == nil || !*container.SecurityContext.ReadOnlyRootFilesystem || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("expected hardened prefetch security context, got %#v", container.SecurityContext)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "ModelCache" {
		t.Fatalf("expected model cache owner reference, got %#v", job.OwnerReferences)
	}

	realCache := cache.DeepCopy()
	realCache.Spec.Digest = model.Digest
	realCache.Spec.SizeBytes = model.SizeBytes
	realJob := PrefetchJob(realCache, "node-a", false, "registry.example/ember-prefetch@sha256:1234")
	realArgs := realJob.Spec.Template.Spec.Containers[0].Args
	if contains(realArgs, "--synthetic") || !contains(realArgs, "--model-id") || !contains(realArgs, model.ID) || !contains(realArgs, "--revision") || !contains(realArgs, model.Revision) {
		t.Fatalf("expected immutable real prefetch args, got %#v", realArgs)
	}
}

func TestNetworkPoliciesRestrictIngressAndAllowDNS(t *testing.T) {
	endpoint := validEndpoint()
	ingress := GatewayIngressNetworkPolicy(endpoint)
	if got := ingress.Spec.Ingress[0].From[0].PodSelector.MatchLabels[LabelComponent]; got != "gateway" {
		t.Fatalf("expected gateway pod selector, got %q", got)
	}
	if got := ingress.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != servingv1alpha1.EmberSystemNamespace {
		t.Fatalf("expected gateway namespace selector, got %q", got)
	}
	dns := DNSNetworkPolicy(endpoint)
	if len(dns.Spec.Egress) != 1 || len(dns.Spec.Egress[0].Ports) != 2 {
		t.Fatalf("expected dual-protocol DNS egress, got %#v", dns.Spec.Egress)
	}
}

func TestGatewayLogRBACIsReadOnlyAndNamespaceScoped(t *testing.T) {
	endpoint := validEndpoint()
	role := GatewayLogRole(endpoint)
	binding := GatewayLogRoleBinding(endpoint)
	if role.Namespace != WorkloadNamespaceName(endpoint.UID) || binding.Namespace != role.Namespace {
		t.Fatalf("expected workload namespace RBAC, role=%q binding=%q", role.Namespace, binding.Namespace)
	}
	if len(role.Rules) != 6 || role.Rules[0].Verbs[0] != "get" || role.Rules[1].Resources[0] != "pods/log" {
		t.Fatalf("unexpected gateway log role: %#v", role.Rules)
	}
	for _, expected := range []string{"deployments", "networkpolicies", "horizontalpodautoscalers", "scaledobjects"} {
		found := false
		for _, rule := range role.Rules {
			for _, resource := range rule.Resources {
				if resource == expected {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("gateway inspector role missing %q: %#v", expected, role.Rules)
		}
	}
	for _, rule := range role.Rules {
		for _, verb := range rule.Verbs {
			if verb == "create" || verb == "update" || verb == "patch" || verb == "delete" {
				t.Fatalf("gateway workload role must be read-only: %#v", role.Rules)
			}
		}
	}
	if binding.Subjects[0].Name != "ember-gateway" || binding.Subjects[0].Namespace != servingv1alpha1.EmberSystemNamespace {
		t.Fatalf("unexpected gateway role binding subject: %#v", binding.Subjects)
	}
}

func TestScaledObjectUsesQueueDepthAndExplicitPause(t *testing.T) {
	endpoint := validEndpoint()
	endpoint.Spec.Scaling.MinReplicas = 0
	endpoint.Spec.Scaling.MaxReplicas = 3
	endpoint.Spec.Scaling.TargetQueueDepth = 4
	scaledObject := ScaledObject(endpoint, true, true)
	if scaledObject.GroupVersionKind() != ScaledObjectGVK {
		t.Fatalf("unexpected ScaledObject GVK: %s", scaledObject.GroupVersionKind())
	}
	if scaledObject.GetAnnotations()[KEDAPausedAnnotation] != "true" {
		t.Fatalf("expected idle pause annotation, got %#v", scaledObject.GetAnnotations())
	}
	spec := scaledObject.Object["spec"].(map[string]any)
	if spec["minReplicaCount"] != int64(1) || spec["maxReplicaCount"] != int64(3) || spec["pollingInterval"] != int64(2) {
		t.Fatalf("unexpected scaling bounds: %#v", spec)
	}
	trigger := spec["triggers"].([]any)[0].(map[string]any)
	metadata := trigger["metadata"].(map[string]any)
	if metadata["threshold"] != "4" || !strings.Contains(metadata["query"].(string), `vllm:num_requests_waiting`) || !strings.Contains(metadata["query"].(string), string(endpoint.UID)) || !strings.Contains(metadata["query"].(string), "or vector(0)") {
		t.Fatalf("unexpected Prometheus trigger: %#v", metadata)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsEnv(values []corev1.EnvVar, name, value string) bool {
	for _, item := range values {
		if item.Name == name && item.Value == value {
			return true
		}
	}
	return false
}

func validEndpoint() *servingv1alpha1.InferenceEndpoint {
	model, _ := catalog.LookupModel("qwen2.5-7b-instruct-awq")
	endpoint := &servingv1alpha1.InferenceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "ep-1", Namespace: servingv1alpha1.EmberSystemNamespace, UID: types.UID("12345678-1234-1234-1234-1234567890ab")}, Spec: servingv1alpha1.InferenceEndpointSpec{OwnerID: "usr_31d2", Model: servingv1alpha1.InferenceEndpointModelSpec{ID: model.ID, Revision: model.Revision}, Profile: servingv1alpha1.ProfileStandard, Scaling: servingv1alpha1.InferenceEndpointScalingSpec{MinReplicas: 0, MaxReplicas: 3, TargetQueueDepth: 4, IdleTimeoutSeconds: 900}, Placement: servingv1alpha1.InferenceEndpointPlacementSpec{CachePreference: servingv1alpha1.CachePreferencePreferred, MaxColdStartFallbackSeconds: 120}}}
	endpoint.Default()
	return endpoint
}

var _ = batchv1.Job{}
