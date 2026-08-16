package rbac

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterRoleCoversModelCachesNodesAndJobsWithoutSecrets(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("cluster_role.yaml"))
	if err != nil {
		t.Fatalf("read cluster role: %v", err)
	}
	text := string(data)
	for _, needle := range []string{"resources: [modelcaches]", "resources: [modelcaches/status]", "resources: [namespaces, serviceaccounts, services, pods, nodes]", "resources: [pods/log]", "apiGroups: [batch]", "resources: [jobs]", "resources: [roles, rolebindings]", "resources: [scaledobjects]"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected %q in cluster role", needle)
		}
	}
	if strings.Contains(text, "secrets") {
		t.Fatal("cluster role must not grant secret access")
	}
}
