package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyKustomizationRendersBothResources(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("kustomization.yaml"))
	if err != nil {
		t.Fatalf("read kustomization: %v", err)
	}
	text := string(data)
	for _, needle := range []string{"validating_admission_policy.yaml", "validating_admission_policy_binding.yaml"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected %q in policy kustomization", needle)
		}
	}
}

func TestValidatingAdmissionPolicyHasRequiredPodGuards(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("validating_admission_policy.yaml"))
	if err != nil {
		t.Fatalf("read policy yaml: %v", err)
	}
	text := string(data)
	for _, needle := range []string{
		"apiVersion: admissionregistration.k8s.io/v1",
		"kind: ValidatingAdmissionPolicy",
		`resources: ["pods"]`,
		"object.spec.serviceAccountName == 'engine'",
		"object.spec.automountServiceAccountToken == false",
		"object.metadata.labels['ember.dev/managed'] == 'true'",
		"object.metadata.labels['ember.dev/endpoint-uid'] != ''",
		"object.metadata.labels['ember.dev/owner'] != ''",
		"object.metadata.labels['component'] == 'engine'",
		"object.spec.securityContext.runAsNonRoot",
		"/var/lib/ember/models",
		"allowPrivilegeEscalation == false",
		"readOnlyRootFilesystem",
		"capabilities.drop.exists(cap, cap == 'ALL')",
		"seccompProfile.type == 'RuntimeDefault'",
		"Containers may mount hostPath-backed volumes only as read-only.",
		"Init containers may mount hostPath-backed volumes only as read-only.",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected %q in policy yaml", needle)
		}
	}
	if strings.Contains(text, `resources: ["jobs"]`) {
		t.Fatal("workload validating policy must not target Jobs")
	}
}

func TestPolicyBindingScopesOnlyGeneratedWorkloadNamespaces(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("validating_admission_policy_binding.yaml"))
	if err != nil {
		t.Fatalf("read policy binding yaml: %v", err)
	}
	text := string(data)
	for _, needle := range []string{
		"kind: ValidatingAdmissionPolicyBinding",
		"policyName: ember-workload-pod-policy",
		"validationActions:",
		"- Deny",
		"namespaceSelector:",
		"ember.dev/admission-policy: hostpath-cache-only",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected %q in policy binding yaml", needle)
		}
	}
	if strings.Contains(text, "ember-system") {
		t.Fatal("policy binding should rely on the admission label selector rather than naming ember-system")
	}
}

func TestDefaultKustomizationIncludesPolicy(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "default", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read default kustomization: %v", err)
	}
	if !strings.Contains(string(data), "../policy") {
		t.Fatal("expected default kustomization to include ../policy")
	}
}
