package publicgatewayconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayUsesGlobalManagedHTTPS(t *testing.T) {
	gateway := readFile(t, "gateway.yaml")
	for _, required := range []string{
		"gatewayClassName: gke-l7-global-external-managed",
		"type: NamedAddress",
		"value: EMBER_GATEWAY_IP_NAME",
		"hostname: EMBER_GATEWAY_HOST",
		"protocol: HTTPS",
		"port: 443",
		"mode: Terminate",
		"networking.gke.io/pre-shared-certs: EMBER_GATEWAY_CERTIFICATE",
		"from: Same",
	} {
		if !strings.Contains(gateway, required) {
			t.Fatalf("Gateway manifest missing %q", required)
		}
	}
	if strings.Contains(gateway, "protocol: HTTP\n") {
		t.Fatal("public Gateway must not expose a plaintext HTTP listener")
	}
}

func TestRoutePublishesOnlyV1(t *testing.T) {
	route := readFile(t, "route.yaml")
	for _, required := range []string{
		"sectionName: https",
		"- EMBER_GATEWAY_HOST",
		"type: PathPrefix",
		"value: /v1",
		"name: ember-gateway",
		"port: 8080",
	} {
		if !strings.Contains(route, required) {
			t.Fatalf("HTTPRoute manifest missing %q", required)
		}
	}
	for _, forbidden := range []string{"/healthz", "/metrics", "value: /\n"} {
		if strings.Contains(route, forbidden) {
			t.Fatalf("HTTPRoute must not publish %q", forbidden)
		}
	}

	service := readFile(t, "../../../gateway/config/service.yaml")
	if strings.Contains(service, "type: LoadBalancer") || strings.Contains(service, "type: NodePort") {
		t.Fatal("ember-gateway Service must remain cluster-internal")
	}
}

func TestPoliciesProtectGatewayBackend(t *testing.T) {
	health := readFile(t, "health_check_policy.yaml")
	for _, required := range []string{
		"kind: HealthCheckPolicy",
		"portSpecification: USE_FIXED_PORT",
		"port: 8080",
		"requestPath: /healthz",
		"kind: Service",
		"name: ember-gateway",
	} {
		if !strings.Contains(health, required) {
			t.Fatalf("HealthCheckPolicy missing %q", required)
		}
		if strings.Contains(health, "logConfig:") {
			t.Fatal("health-check request logging should remain disabled to avoid probe log cost")
		}
	}

	backend := readFile(t, "backend_policy.yaml")
	for _, required := range []string{
		"kind: GCPBackendPolicy",
		"timeoutSec: 180",
		"kind: Service",
		"name: ember-gateway",
	} {
		if !strings.Contains(backend, required) {
			t.Fatalf("GCPBackendPolicy missing %q", required)
		}
	}

	gatewayPolicy := readFile(t, "gateway_policy.yaml")
	for _, required := range []string{
		"kind: GCPGatewayPolicy",
		"sslPolicy: EMBER_GATEWAY_SSL_POLICY",
		"group: gateway.networking.k8s.io",
		"kind: Gateway",
		"name: ember-public",
	} {
		if !strings.Contains(gatewayPolicy, required) {
			t.Fatalf("GCPGatewayPolicy missing %q", required)
		}
	}
}

func TestDeploymentScriptEnforcesDNSAndCleanupSafety(t *testing.T) {
	script := readFile(t, "../../../scripts/gke-public-gateway.sh")
	for _, required := range []string{
		"compute addresses create",
		"compute ssl-certificates create",
		"compute ssl-policies create",
		"--profile=MODERN",
		"--min-tls-version=1.2",
		`"${DIG}" +short A`,
		`"${DIG}" +short AAAA`,
		"DNS only (gray cloud)",
		"wait_for_certificate",
		"Google-managed certificate domain status:",
		"keep_cluster_for_public_gateway",
		"./scripts/gcp-cost-guard.sh keep-cluster",
		`CLUSTER_NAME=${CLUSTER_NAME:-ember-gpu}`,
		"validate_backend",
		`(.observedGeneration // $generation) == $generation`,
		`type == "Reconciled"`,
		`type == "ResolvedRefs"`,
		`type == "Ready"`,
		"ADDRESS_DESCRIPTION",
		"CERTIFICATE_DESCRIPTION",
		"SSL_POLICY_DESCRIPTION",
		"validate_cloud_resources_for_destroy",
		`request_status "/healthz" "404"`,
		`request_status "/metrics" "404"`,
		"ember-jwt-keys",
		"--private-key-base64-stdin",
		`CONFIRM_DESTROY=${expected_confirmation}`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("public Gateway script missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"api.cloudflare.com",
		"CLOUDFLARE_API_TOKEN",
		"rm -rf",
		"--allow-unauthenticated",
		"Google-managed certificate domain validation failed",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("public Gateway script must not contain %q", forbidden)
		}
	}
	info, err := os.Stat("../../../scripts/gke-public-gateway.sh")
	if err != nil {
		t.Fatalf("stat public Gateway script: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("public Gateway script must be executable")
	}
}

func TestDestroyRejectsMissingConfirmationBeforeCloudCalls(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "gcloud.log")
	gcloudPath := filepath.Join(tempDir, "gcloud")
	jqPath := filepath.Join(tempDir, "jq")
	if err := os.WriteFile(gcloudPath, []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >>\"${GCLOUD_LOG}\"\nexit 99\n"), 0o755); err != nil {
		t.Fatalf("write fake gcloud: %v", err)
	}
	if err := os.WriteFile(jqPath, []byte("#!/usr/bin/env bash\nexit 99\n"), 0o755); err != nil {
		t.Fatalf("write fake jq: %v", err)
	}

	command := exec.Command("bash", "../../../scripts/gke-public-gateway.sh", "destroy")
	command.Env = append(os.Environ(),
		"PATH="+tempDir+":"+os.Getenv("PATH"),
		"GCLOUD="+gcloudPath,
		"GCLOUD_LOG="+logPath,
		"PROJECT_ID=ember-test1",
		"CLUSTER_NAME=ember-gpu",
		"CLUSTER_LOCATION=us-central1-a",
		"GATEWAY_HOST=api.example.com",
		"CONFIRM_DESTROY=",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("destroy accepted a missing confirmation")
	}
	if !strings.Contains(string(output), "set CONFIRM_DESTROY=ember-test1/api.example.com") {
		t.Fatalf("destroy did not print the exact required confirmation:\n%s", output)
	}
	if log, readErr := os.ReadFile(logPath); readErr == nil && len(log) != 0 {
		t.Fatalf("destroy called gcloud before confirmation:\n%s", log)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read fake gcloud log: %v", readErr)
	}
}

func TestDestroyRefusesCloudResourcesItDoesNotOwn(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "gcloud.log")
	gcloudPath := writeExecutable(t, tempDir, "gcloud", `#!/usr/bin/env bash
printf '%s\n' "$*" >>"${GCLOUD_LOG}"
case "$*" in
  *"compute addresses describe"*) printf 'resource was not found\n' >&2; exit 1 ;;
  *"compute ssl-certificates describe"*) printf '{}\n' ;;
  *) printf 'unexpected gcloud call: %s\n' "$*" >&2; exit 99 ;;
esac
`)
	writeExecutable(t, tempDir, "jq", "#!/usr/bin/env bash\nexit 1\n")

	command := exec.Command("bash", "../../../scripts/gke-public-gateway.sh", "destroy")
	command.Env = append(os.Environ(),
		"PATH="+tempDir+":"+os.Getenv("PATH"),
		"GCLOUD="+gcloudPath,
		"GCLOUD_LOG="+logPath,
		"PROJECT_ID=ember-test1",
		"CLUSTER_NAME=ember-gpu",
		"CLUSTER_LOCATION=us-central1-a",
		"GATEWAY_HOST=api.example.com",
		"CONFIRM_DESTROY=ember-test1/api.example.com",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("destroy accepted an unowned certificate")
	}
	if !strings.Contains(string(output), "not the dedicated managed certificate") {
		t.Fatalf("destroy did not explain the ownership mismatch:\n%s", output)
	}
	log := readFile(t, logPath)
	if strings.Contains(log, " delete ") {
		t.Fatalf("destroy mutated cloud resources after ownership validation failed:\n%s", log)
	}
}

func TestDeployRendersAndAppliesReadyGateway(t *testing.T) {
	tempDir := t.TempDir()
	gcloudLog := filepath.Join(tempDir, "gcloud.log")
	kubectlLog := filepath.Join(tempDir, "kubectl.log")
	appliedManifest := filepath.Join(tempDir, "applied.yaml")

	gcloudPath := writeExecutable(t, tempDir, "gcloud", `#!/usr/bin/env bash
printf '%s\n' "$*" >>"${GCLOUD_LOG}"
case "$*" in
  *"container clusters describe"*) printf '{}\n' ;;
  *"container clusters get-credentials"*) ;;
  *"services enable"*) ;;
  *"compute addresses describe"*) printf '{}\n' ;;
  *"compute ssl-certificates describe"*) printf '{}\n' ;;
  *"compute ssl-policies describe"*) printf '{}\n' ;;
  *"tasks queues describe"*) ;;
  *"tasks list"*)
    printf '%s\n' \
      'projects/example-project/locations/us-central1/queues/ember-cost-guard/tasks/guard-ember-gpu-20250101000000-1-gpu' \
      'projects/example-project/locations/us-central1/queues/ember-cost-guard/tasks/guard-ember-gpu-20250101000000-1-cluster'
    ;;
  *"tasks delete"*) ;;
  *) printf 'unexpected gcloud call: %s\n' "$*" >&2; exit 99 ;;
esac
`)
	writeExecutable(t, tempDir, "jq", `#!/usr/bin/env bash
case "$*" in
  *"gatewayApiConfig.channel"*) printf 'CHANNEL_STANDARD\n' ;;
  *"httpLoadBalancing.disabled"*) printf 'false\n' ;;
  *"addressType"*) exit 0 ;;
  *".address // empty"*) printf '203.0.113.10\n' ;;
  *"managed.domains =="*) exit 0 ;;
  *"minTlsVersion"*) exit 0 ;;
  *"spec.ports"*) exit 0 ;;
  *"Reconciled"*) exit 0 ;;
  *"Attached"*) exit 0 ;;
  *"Programmed"*) exit 0 ;;
  *"managed.status"*) printf 'ACTIVE\n' ;;
  *) printf 'unexpected jq call: %s\n' "$*" >&2; exit 99 ;;
esac
`)
	kubectlPath := writeExecutable(t, tempDir, "kubectl", `#!/usr/bin/env bash
printf '%s\n' "$*" >>"${KUBECTL_LOG}"
case "$*" in
  *"wait --for=condition=Accepted gatewayclass/gke-l7-global-external-managed"*) ;;
  *"get service ember-gateway -o json"*) printf '{}\n' ;;
  *"wait --for=condition=Available deployment/ember-gateway"*) ;;
  "kustomize deploy/gke/public-gateway")
    printf '%s\n' \
      'host: EMBER_GATEWAY_HOST' \
      'address: EMBER_GATEWAY_IP_NAME' \
      'certificate: EMBER_GATEWAY_CERTIFICATE' \
      'policy: EMBER_GATEWAY_SSL_POLICY'
    ;;
  "apply -f "*) cp "$3" "${APPLIED_MANIFEST}" ;;
  *"get gateway/ember-public -o jsonpath="*) printf '203.0.113.10' ;;
  *"get healthcheckpolicy/ember-gateway -o json"*) printf '{}\n' ;;
  *"get gcpbackendpolicy/ember-gateway -o json"*) printf '{}\n' ;;
  *"get gcpgatewaypolicy/ember-public -o json"*) printf '{}\n' ;;
  *"get httproute/ember-public -o json"*) printf '{}\n' ;;
  *"get gateway/ember-public -o json"*) printf '{}\n' ;;
  *) printf 'unexpected kubectl call: %s\n' "$*" >&2; exit 99 ;;
esac
`)
	digPath := writeExecutable(t, tempDir, "dig", `#!/usr/bin/env bash
case "$2" in
  A) printf '203.0.113.10\n' ;;
  AAAA) ;;
  *) exit 99 ;;
esac
`)
	writeExecutable(t, tempDir, "gke-gcloud-auth-plugin", "#!/usr/bin/env bash\nexit 0\n")

	command := exec.Command("bash", "../../../scripts/gke-public-gateway.sh", "deploy")
	command.Env = append(os.Environ(),
		"PATH="+tempDir+":"+os.Getenv("PATH"),
		"GCLOUD="+gcloudPath,
		"KUBECTL="+kubectlPath,
		"DIG="+digPath,
		"GCLOUD_LOG="+gcloudLog,
		"KUBECTL_LOG="+kubectlLog,
		"APPLIED_MANIFEST="+appliedManifest,
		"PROJECT_ID=example-project",
		"PROJECT_NUMBER=123456789",
		"CLUSTER_NAME=ember-gpu",
		"CLUSTER_LOCATION=us-central1-a",
		"TASKS_LOCATION=us-central1",
		"GATEWAY_HOST=api.example.com",
		"GATEWAY_READY_TIMEOUT_MINUTES=1",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("deploy public Gateway: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Public Gateway ready: https://api.example.com/v1") {
		t.Fatalf("deploy did not report readiness:\n%s", output)
	}

	manifest := readFile(t, appliedManifest)
	for _, value := range []string{
		"api.example.com",
		"ember-gateway-ip",
		"ember-gateway-cert",
		"ember-gateway-modern",
	} {
		if !strings.Contains(manifest, value) {
			t.Fatalf("rendered manifest missing %q:\n%s", value, manifest)
		}
	}
	if strings.Contains(manifest, "EMBER_GATEWAY_") {
		t.Fatalf("rendered manifest retained placeholders:\n%s", manifest)
	}

	gcloudCalls := readFile(t, gcloudLog)
	if !strings.Contains(gcloudCalls, "tasks delete guard-ember-gpu-20250101000000-1-cluster") {
		t.Fatalf("deploy did not remove the cluster deletion timer:\n%s", gcloudCalls)
	}
	if strings.Contains(gcloudCalls, "tasks delete guard-ember-gpu-20250101000000-1-gpu") {
		t.Fatalf("deploy removed the GPU deletion timer:\n%s", gcloudCalls)
	}
}

func writeExecutable(t *testing.T, directory, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
