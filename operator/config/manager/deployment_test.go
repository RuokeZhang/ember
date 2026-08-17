package manager

import (
	"os"
	"strings"
	"testing"
)

func TestDeploymentMatchesLocalImageContract(t *testing.T) {
	data, err := os.ReadFile("deployment.yaml")
	if err != nil {
		t.Fatalf("read deployment: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"image: ember-operator:dev",
		"- /ember-operator",
		"- --enable-keda",
		"serviceAccountName: ember-operator-manager",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("deployment missing %q", required)
		}
	}
	if strings.Contains(text, "--simulation-mode") {
		t.Fatal("base deployment must default to the real runtime")
	}
}
