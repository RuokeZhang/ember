package kindconfig

import (
	"os"
	"strings"
	"testing"
)

func TestKindUsesReviewedInRepositoryGPUPlugin(t *testing.T) {
	data, err := os.ReadFile("fake-gpu-daemonset.yaml")
	if err != nil {
		t.Fatalf("read fake GPU manifest: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"image: ember-fake-gpu:dev",
		"ember.dev/gpu: l4",
		"path: /var/lib/kubelet/device-plugins",
		"automountServiceAccountToken: false",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("fake GPU manifest missing %q", required)
		}
	}

	if strings.Contains(strings.ToLower(text), "fake-gpu-operator") {
		t.Fatal("Kind deployment must not install an external fake GPU operator")
	}
}

func TestSimulationModeIsKindOnly(t *testing.T) {
	kindData, err := os.ReadFile("kustomization.yaml")
	if err != nil {
		t.Fatalf("read Kind kustomization: %v", err)
	}
	if !strings.Contains(string(kindData), "value: --simulation-mode") {
		t.Fatal("Kind overlay must explicitly enable simulation mode")
	}
	baseData, err := os.ReadFile("../../operator/config/manager/deployment.yaml")
	if err != nil {
		t.Fatalf("read base manager deployment: %v", err)
	}
	if strings.Contains(string(baseData), "--simulation-mode") {
		t.Fatal("base manager deployment must default to the real runtime")
	}
}
