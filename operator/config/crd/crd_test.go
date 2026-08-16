package crd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCRDKustomizationIncludesModelCache(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("kustomization.yaml"))
	if err != nil {
		t.Fatalf("read kustomization: %v", err)
	}
	if !strings.Contains(string(data), "serving.ember.dev_modelcaches.yaml") {
		t.Fatal("expected ModelCache CRD in kustomization")
	}
}

func TestModelCacheCRDIsClusterScoped(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("bases", "serving.ember.dev_modelcaches.yaml"))
	if err != nil {
		t.Fatalf("read modelcache CRD: %v", err)
	}
	text := string(data)
	for _, needle := range []string{"name: modelcaches.serving.ember.dev", "scope: Cluster", "kind: ModelCache", "referencingEndpoints"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected %q in ModelCache CRD", needle)
		}
	}
}
