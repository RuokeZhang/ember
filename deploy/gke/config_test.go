package gkeconfig

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestGKEOverlayUsesRealRuntimeAndPortablePostgres(t *testing.T) {
	data, err := os.ReadFile("kustomization.yaml")
	if err != nil {
		t.Fatalf("read GKE kustomization: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"--prefetch-image=EMBER_PREFETCH_IMAGE",
		"path: /spec/template/spec/nodeSelector",
		"path: /spec/template/spec/tolerations",
		"value: standard-rwo",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("GKE overlay missing %q", required)
		}
	}
	for _, forbidden := range []string{"--simulation-mode", "fake-gpu-daemonset.yaml", "postgres-storage.yaml"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("GKE overlay must not contain %q", forbidden)
		}
	}
}

func TestPostgresServiceIsHeadlessForNetworkPolicy(t *testing.T) {
	var service corev1.Service
	decodeYAMLFile(t, "../../controlapi/config/postgres_service.yaml", &service)
	if service.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatal("Postgres Service must resolve directly to its pod for selector-based egress policies")
	}
}

func TestControlAPINetworkPoliciesRestrictEgress(t *testing.T) {
	base := readNetworkPolicy(t, "../../controlapi/config/network_policy.yaml", "ember-control-api")
	if len(base.Spec.Egress) != 3 {
		t.Fatalf("expected three base control API egress rules, got %d", len(base.Spec.Egress))
	}
	assertSelectorRule(t, findEgressRule(t, base, corev1.ProtocolTCP, 5432), nil, map[string]string{
		"app.kubernetes.io/name": "ember-postgres",
		"component":              "database",
	})
	assertSelectorRule(t, findEgressRule(t, base, corev1.ProtocolTCP, 8080), nil, map[string]string{
		"app.kubernetes.io/name": "ember-gateway",
		"component":              "gateway",
	})
	dns := findEgressRule(t, base, corev1.ProtocolUDP, 53)
	assertPorts(t, dns, map[corev1.Protocol]int32{
		corev1.ProtocolUDP: 53,
		corev1.ProtocolTCP: 53,
	})
	assertSelectorRule(t, dns, map[string]string{
		"kubernetes.io/metadata.name": "kube-system",
	}, map[string]string{
		"k8s-app": "kube-dns",
	})

	gke := readNetworkPolicy(t, "control-api-dns-network-policy.yaml", "ember-control-api-gke-dns")
	if !reflect.DeepEqual(gke.Spec.PodSelector.MatchLabels, map[string]string{
		"app.kubernetes.io/name": "ember-control-api",
		"component":              "control-api",
	}) {
		t.Fatalf("unexpected GKE DNS pod selector: %#v", gke.Spec.PodSelector.MatchLabels)
	}
	if !reflect.DeepEqual(gke.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}) || len(gke.Spec.Egress) != 1 {
		t.Fatalf("unexpected GKE DNS policy shape: %#v", gke.Spec)
	}
	gkeDNS := gke.Spec.Egress[0]
	assertPorts(t, gkeDNS, map[corev1.Protocol]int32{
		corev1.ProtocolUDP: 53,
		corev1.ProtocolTCP: 53,
	})
	if len(gkeDNS.To) != 1 || gkeDNS.To[0].IPBlock == nil || gkeDNS.To[0].IPBlock.CIDR != "GKE_DNS_CIDR" ||
		gkeDNS.To[0].NamespaceSelector != nil || gkeDNS.To[0].PodSelector != nil {
		t.Fatalf("GKE DNS policy must allow only the injected resolver CIDR: %#v", gkeDNS.To)
	}
}

func TestGKEDeployDiscoversDNSAndMigratesPostgresService(t *testing.T) {
	data, err := os.ReadFile("../../scripts/gke-deploy.sh")
	if err != nil {
		t.Fatalf("read GKE deploy script: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"get service kube-dns",
		`dns_service_cidr="${dns_service_ip}/32"`,
		`s|GKE_DNS_CIDR|${dns_service_cidr}|g`,
		`"${postgres_cluster_ip}" != "None"`,
		"delete service ember-postgres --wait=true",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("GKE deploy script missing %q", required)
		}
	}
	if strings.Index(text, "delete service ember-postgres --wait=true") > strings.Index(text, `apply -f "${rendered}"`) {
		t.Fatal("legacy Postgres Service must be deleted before applying the headless Service")
	}
}

func TestCloudBuildTargetsAMD64RepositoryImages(t *testing.T) {
	data, err := os.ReadFile("cloudbuild.yaml")
	if err != nil {
		t.Fatalf("read Cloud Build config: %v", err)
	}
	text := string(data)
	if strings.Count(text, "--platform=linux/amd64") != 4 {
		t.Fatalf("expected four amd64 image builds, got:\n%s", text)
	}
	for _, image := range []string{"ember-operator", "ember-prefetch", "ember-gateway", "ember-control-api"} {
		if !strings.Contains(text, "${_REGISTRY}/"+image+":${_IMAGE_TAG}") {
			t.Fatalf("Cloud Build config missing %s", image)
		}
	}
}

func TestCloudBuildSubmissionUsesArtifactRegion(t *testing.T) {
	data, err := os.ReadFile("../../scripts/gke-build-images.sh")
	if err != nil {
		t.Fatalf("read GKE image build script: %v", err)
	}
	if !strings.Contains(string(data), `--region="${REGION}"`) {
		t.Fatal("Cloud Build submission must use the configured Artifact Registry region")
	}
}

func TestClusterScriptArmsCleanupBeforeGPUAndAllowsDriverDownload(t *testing.T) {
	data, err := os.ReadFile("../../scripts/gke-cluster.sh")
	if err != nil {
		t.Fatalf("read GKE cluster script: %v", err)
	}
	text := string(data)
	if strings.Count(text, "--scopes=gke-default") != 2 {
		t.Fatalf("expected explicit storage-capable scopes on both node pools, got:\n%s", text)
	}
	arm := strings.Index(text, "if ! ALLOW_MISSING_GPU_POOL=true cost_guard arm; then")
	createGPU := strings.Index(text, "container node-pools create")
	if arm < 0 || createGPU < 0 || arm > createGPU {
		t.Fatal("cost guard must be armed before the GPU node pool is created")
	}
	for _, required := range []string{
		"--async",
		"NODE_POOL_CREATE_TIMEOUT_MINUTES",
		`wait_for_container_operation "${operation_name}"`,
		"validate_existing_gpu_pool",
		".error.code // 0",
		".autoscaling.enabled // false",
		".autoscaling.minNodeCount // 0",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("GKE cluster script missing long-running node-pool safeguard %q", required)
		}
	}
}

func TestCostGuardRequiresExistingGPUPoolByDefault(t *testing.T) {
	log, err := runCostGuardArm(t, false)
	if err == nil {
		t.Fatal("cost guard accepted a missing GPU pool without bootstrap mode")
	}
	if !strings.Contains(log, "container node-pools describe") {
		t.Fatalf("cost guard did not validate the GPU pool:\n%s", log)
	}
	if strings.Contains(log, "tasks create-http-task") {
		t.Fatalf("cost guard scheduled tasks after GPU pool validation failed:\n%s", log)
	}
}

func TestCostGuardCanArmBeforeGPUPoolExistsDuringBootstrap(t *testing.T) {
	log, err := runCostGuardArm(t, true)
	if err != nil {
		t.Fatalf("arm cost guard in bootstrap mode: %v\n%s", err, log)
	}
	if strings.Contains(log, "container node-pools describe") {
		t.Fatalf("bootstrap mode still required the GPU pool:\n%s", log)
	}
	if strings.Count(log, "tasks create-http-task") != 2 {
		t.Fatalf("bootstrap mode did not schedule both deletion tasks:\n%s", log)
	}
}

func decodeYAMLFile(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func readNetworkPolicy(t *testing.T, path, name string) networkingv1.NetworkPolicy {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var policy networkingv1.NetworkPolicy
		if err := decoder.Decode(&policy); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode %s: %v", path, err)
		}
		if policy.Name == name {
			return policy
		}
	}
	t.Fatalf("network policy %s not found in %s", name, path)
	return networkingv1.NetworkPolicy{}
}

func findEgressRule(t *testing.T, policy networkingv1.NetworkPolicy, protocol corev1.Protocol, port int32) networkingv1.NetworkPolicyEgressRule {
	t.Helper()
	for _, rule := range policy.Spec.Egress {
		for _, candidate := range rule.Ports {
			if candidate.Protocol != nil && *candidate.Protocol == protocol &&
				candidate.Port != nil && candidate.Port.IntVal == port {
				return rule
			}
		}
	}
	t.Fatalf("network policy %s missing %s/%d egress rule", policy.Name, protocol, port)
	return networkingv1.NetworkPolicyEgressRule{}
}

func assertPorts(t *testing.T, rule networkingv1.NetworkPolicyEgressRule, expected map[corev1.Protocol]int32) {
	t.Helper()
	actual := make(map[corev1.Protocol]int32, len(rule.Ports))
	for _, port := range rule.Ports {
		if port.Protocol == nil || port.Port == nil {
			t.Fatalf("network policy rule has an incomplete port: %#v", port)
		}
		actual[*port.Protocol] = port.Port.IntVal
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("unexpected network policy ports: got %#v, want %#v", actual, expected)
	}
}

func assertSelectorRule(t *testing.T, rule networkingv1.NetworkPolicyEgressRule, namespaceLabels, podLabels map[string]string) {
	t.Helper()
	if len(rule.To) != 1 || rule.To[0].IPBlock != nil {
		t.Fatalf("network policy rule must have one selector peer: %#v", rule.To)
	}
	peer := rule.To[0]
	if namespaceLabels == nil {
		if peer.NamespaceSelector != nil {
			t.Fatalf("unexpected namespace selector: %#v", peer.NamespaceSelector)
		}
	} else if peer.NamespaceSelector == nil || !reflect.DeepEqual(peer.NamespaceSelector.MatchLabels, namespaceLabels) {
		t.Fatalf("unexpected namespace selector: %#v", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil || !reflect.DeepEqual(peer.PodSelector.MatchLabels, podLabels) {
		t.Fatalf("unexpected pod selector: %#v", peer.PodSelector)
	}
}

func runCostGuardArm(t *testing.T, allowMissing bool) (string, error) {
	t.Helper()
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "gcloud.log")
	gcloudPath := filepath.Join(tempDir, "gcloud")
	fakeGcloud := `#!/usr/bin/env bash
printf '%s\n' "$*" >>"${GCLOUD_LOG}"
if [[ "$*" == *"container node-pools describe"* ]]; then
  exit 1
fi
`
	if err := os.WriteFile(gcloudPath, []byte(fakeGcloud), 0o755); err != nil {
		t.Fatalf("write fake gcloud: %v", err)
	}

	command := exec.Command("bash", "../../scripts/gcp-cost-guard.sh", "arm")
	command.Env = append(os.Environ(),
		"GCLOUD="+gcloudPath,
		"GCLOUD_LOG="+logPath,
		"PROJECT_ID=example-project",
		"PROJECT_NUMBER=123456789",
		"CLUSTER_NAME=ember-gpu",
		"CLUSTER_LOCATION=us-central1-a",
		"GPU_NODE_POOL=l4-spot",
		"TASKS_LOCATION=us-central1",
		"ALLOW_MISSING_GPU_POOL=false",
	)
	if allowMissing {
		command.Env = append(command.Env, "ALLOW_MISSING_GPU_POOL=true")
	}
	output, commandErr := command.CombinedOutput()
	log, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read fake gcloud log: %v\n%s", readErr, output)
	}
	return string(log), commandErr
}
