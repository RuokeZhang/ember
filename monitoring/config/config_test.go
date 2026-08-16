package config

import (
	"os"
	"strings"
	"testing"
)

func TestPrometheusScrapesOnlyEnginePodsAndUsesPinnedImage(t *testing.T) {
	configBytes, err := os.ReadFile("prometheus.yaml")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config := string(configBytes)
	for _, required := range []string{"role: pod", "regex: engine", "target_label: endpoint_uid", "scrape_interval: 2s"} {
		if !strings.Contains(config, required) {
			t.Fatalf("Prometheus config missing %q", required)
		}
	}
	deploymentBytes, err := os.ReadFile("deployment.yaml")
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	deployment := string(deploymentBytes)
	for _, required := range []string{"prom/prometheus:v3.5.0@sha256:", "automountServiceAccountToken: false", "expirationSeconds: 3600", "readOnlyRootFilesystem: true"} {
		if !strings.Contains(deployment, required) {
			t.Fatalf("Prometheus deployment missing %q", required)
		}
	}
}

func TestPrometheusRBACIsReadOnlyAndSecretFree(t *testing.T) {
	data, err := os.ReadFile("rbac.yaml")
	if err != nil {
		t.Fatalf("read RBAC: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "secrets") || strings.Contains(text, "create") || strings.Contains(text, "delete") || strings.Contains(text, "patch") || strings.Contains(text, "update") {
		t.Fatalf("Prometheus RBAC must stay read-only and Secret-free:\n%s", text)
	}
}
