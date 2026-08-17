package gkeconfig

import (
	"os"
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

func TestClusterScriptArmsCleanupBeforeGPUAndAllowsDriverDownload(t *testing.T) {
	data, err := os.ReadFile("../../scripts/gke-cluster.sh")
	if err != nil {
		t.Fatalf("read GKE cluster script: %v", err)
	}
	text := string(data)
	if strings.Count(text, "--scopes=gke-default") != 2 {
		t.Fatalf("expected explicit storage-capable scopes on both node pools, got:\n%s", text)
	}
	arm := strings.Index(text, "if ! cost_guard arm; then")
	createGPU := strings.Index(text, "container node-pools create")
	if arm < 0 || createGPU < 0 || arm > createGPU {
		t.Fatal("cost guard must be armed before the GPU node pool is created")
	}
}
