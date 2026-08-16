package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestModelCacheDefaultsAndNodeUpsert(t *testing.T) {
	cache := &ModelCache{}
	cache.Default()
	if cache.Spec.RetentionPolicy != RetentionPolicyLRUWithFloor {
		t.Fatalf("expected default retention policy %q, got %q", RetentionPolicyLRUWithFloor, cache.Spec.RetentionPolicy)
	}
	stamp := metav1.NewTime(time.Now().UTC())
	cache.Status.UpsertNode("node-a", ModelCacheNodeStateLoading, 10, nil, nil, "loading")
	cache.Status.UpsertNode("node-a", ModelCacheNodeStateReady, 20, &stamp, nil, "ready")
	if len(cache.Status.Nodes) != 1 {
		t.Fatalf("expected one node entry, got %d", len(cache.Status.Nodes))
	}
	if cache.Status.Nodes[0].State != ModelCacheNodeStateReady || cache.Status.Nodes[0].ProgressBytes != 20 {
		t.Fatalf("expected updated node state, got %#v", cache.Status.Nodes[0])
	}
	cache.Status.RemoveNode("node-a")
	if len(cache.Status.Nodes) != 0 {
		t.Fatalf("expected node removal, got %#v", cache.Status.Nodes)
	}
}
