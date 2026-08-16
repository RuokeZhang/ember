package prefetch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RuokeZhang/ember/operator/cacheartifact"
)

func TestSyntheticArtifactDigestIsDeterministic(t *testing.T) {
	metadata := cacheartifact.SimulationMetadata()
	data := cacheartifact.SimulationArtifactBytes()
	if got := cacheartifact.DigestBytes(data); got != metadata.Digest {
		t.Fatalf("expected deterministic digest %q, got %q", metadata.Digest, got)
	}
	if int64(len(data)) != metadata.SizeBytes {
		t.Fatalf("expected size %d, got %d", metadata.SizeBytes, len(data))
	}
}

func TestRunCreatesAtomicFinalPath(t *testing.T) {
	root := newScratchDir(t)
	metadata := cacheartifact.SimulationMetadata()
	options := Options{Root: root, CacheHash: "abcdef1234567890", ExpectedDigest: metadata.Digest, ExpectedSize: metadata.SizeBytes, Synthetic: true}
	if err := Run(context.Background(), options); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	finalDir := filepath.Join(root, options.CacheHash)
	if _, err := os.Stat(finalDir); err != nil {
		t.Fatalf("expected final cache directory: %v", err)
	}
	if _, err := os.Stat(finalDir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected staging directory removed, got %v", err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(finalDir, cacheartifact.ManifestFileName))
	if err != nil {
		t.Fatalf("expected completion manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest["digest"] != metadata.Digest {
		t.Fatalf("expected manifest digest %q, got %#v", metadata.Digest, manifest["digest"])
	}
}

func TestVerifyOnly(t *testing.T) {
	root := newScratchDir(t)
	metadata := cacheartifact.SimulationMetadata()
	options := Options{Root: root, CacheHash: "abcdef1234567890", ExpectedDigest: metadata.Digest, ExpectedSize: metadata.SizeBytes, Synthetic: true}
	if err := Run(context.Background(), options); err != nil {
		t.Fatalf("seed run failed: %v", err)
	}
	options = Options{Root: root, CacheHash: "abcdef1234567890", ExpectedDigest: metadata.Digest, ExpectedSize: metadata.SizeBytes, VerifyOnly: true}
	if err := Run(context.Background(), options); err != nil {
		t.Fatalf("verify-only failed: %v", err)
	}
}

func TestDigestMismatchFails(t *testing.T) {
	root := newScratchDir(t)
	options := Options{Root: root, CacheHash: "abcdef1234567890", ExpectedDigest: "sha256:deadbeef", Synthetic: true}
	if err := Run(context.Background(), options); err == nil {
		t.Fatal("expected digest mismatch failure")
	}
	if _, err := os.Stat(filepath.Join(root, options.CacheHash)); !os.IsNotExist(err) {
		t.Fatalf("expected no final directory on failure, got %v", err)
	}
}

func TestSafetensorsRejection(t *testing.T) {
	root := newScratchDir(t)
	metadata := cacheartifact.SimulationMetadata()
	finalDir := filepath.Join(root, "abcdef1234567890")
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		t.Fatalf("mkdir final dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(finalDir, cacheartifact.ArtifactFileName), []byte("not-safetensors"), 0o644); err != nil {
		t.Fatalf("write invalid artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(finalDir, cacheartifact.ManifestFileName), []byte(`{"digest":"`+metadata.Digest+`"}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	options := Options{Root: root, CacheHash: "abcdef1234567890", ExpectedDigest: metadata.Digest, VerifyOnly: true}
	if err := Run(context.Background(), options); err == nil || !strings.Contains(err.Error(), "validate safetensors artifact") {
		t.Fatalf("expected safetensors validation failure, got %v", err)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	root := newScratchDir(t)
	metadata := cacheartifact.SimulationMetadata()
	options := Options{Root: root, CacheHash: "../evil", ExpectedDigest: metadata.Digest, Synthetic: true}
	if err := Run(context.Background(), options); err == nil {
		t.Fatal("expected path traversal rejection")
	}
}

func newScratchDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Join(wd, "testdata", ".runtime", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UTC().Format("20060102150405.000000000"))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}
