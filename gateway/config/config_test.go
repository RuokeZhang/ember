package config

import (
	"os"
	"strings"
	"testing"
)

func TestGatewayDeploymentUsesProjectedCredentialAndPublicKeyOnly(t *testing.T) {
	data, err := os.ReadFile("deployment.yaml")
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"image: ember-gateway:dev",
		"automountServiceAccountToken: false",
		"serviceAccountToken:",
		"expirationSeconds: 3600",
		"key: public.key",
		"readOnlyRootFilesystem: true",
		"component: gateway",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("gateway deployment missing %q", required)
		}
	}
	if strings.Contains(text, "key: private.key") {
		t.Fatal("gateway must never mount the JWT private key")
	}
}

func TestGatewayRBACCannotReadSecretsOrMutatePods(t *testing.T) {
	data, err := os.ReadFile("rbac.yaml")
	if err != nil {
		t.Fatalf("read RBAC: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "secrets") || strings.Contains(text, "pods") {
		t.Fatalf("gateway system role must not grant Secret or Pod access:\n%s", text)
	}
	for _, required := range []string{"resources: [inferenceendpoints]", "resources: [inferenceendpoints/status]"} {
		if !strings.Contains(text, required) {
			t.Fatalf("gateway RBAC missing %q", required)
		}
	}
}
