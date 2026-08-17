package gkeconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
