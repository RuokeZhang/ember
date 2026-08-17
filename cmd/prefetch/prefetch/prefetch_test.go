package prefetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RuokeZhang/ember/internal/catalog"
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

func TestRunDownloadsAndVerifiesImmutableModelManifest(t *testing.T) {
	root := newScratchDir(t)
	files := map[string][]byte{
		"config.json":         []byte(`{"model_type":"test"}`),
		"tokenizer.json":      []byte(`{"version":"1.0"}`),
		"weights.safetensors": cacheartifact.SimulationArtifactBytes(),
	}
	source := catalog.ModelSource{
		BaseURL:    "",
		Repository: "test/model",
		Revision:   "immutable-revision",
	}
	for _, filePath := range []string{"config.json", "tokenizer.json", "weights.safetensors"} {
		data := files[filePath]
		source.Files = append(source.Files, catalog.ModelFile{
			Path:        filePath,
			Digest:      cacheartifact.DigestBytes(data),
			SizeBytes:   int64(len(data)),
			Safetensors: strings.HasSuffix(filePath, ".safetensors"),
		})
	}
	requests := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		prefix := "/test/model/resolve/immutable-revision/"
		if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, prefix) {
			http.NotFound(w, request)
			return
		}
		filePath := strings.TrimPrefix(request.URL.Path, prefix)
		data, ok := files[filePath]
		if !ok {
			http.NotFound(w, request)
			return
		}
		requests[filePath]++
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		_, _ = w.Write(data)
	}))
	defer server.Close()
	source.BaseURL = server.URL

	options := Options{
		Root:           root,
		CacheHash:      "abcdef1234567890",
		ExpectedDigest: catalog.ModelFilesDigest(source.Files),
		ExpectedSize:   catalog.ModelFilesSize(source.Files),
		ModelID:        "test-model",
		Revision:       source.Revision,
		Source:         &source,
		HTTPClient:     server.Client(),
	}
	if err := Run(context.Background(), options); err != nil {
		t.Fatalf("real prefetch failed: %v", err)
	}
	for filePath := range files {
		if requests[filePath] != 1 {
			t.Fatalf("expected one exact request for %q, got %d", filePath, requests[filePath])
		}
	}
	verifyOptions := Options{
		Root:           root,
		CacheHash:      options.CacheHash,
		ExpectedDigest: options.ExpectedDigest,
		ExpectedSize:   options.ExpectedSize,
		VerifyOnly:     true,
	}
	if err := Run(context.Background(), verifyOptions); err != nil {
		t.Fatalf("verify real cache failed: %v", err)
	}
}

func TestRealSourcePathTraversalRejectedBeforeDownload(t *testing.T) {
	root := newScratchDir(t)
	source := catalog.ModelSource{
		BaseURL:    "http://127.0.0.1",
		Repository: "test/model",
		Revision:   "immutable-revision",
		Files: []catalog.ModelFile{{
			Path:      "../outside",
			Digest:    "sha256:" + strings.Repeat("0", 64),
			SizeBytes: 1,
		}},
	}
	options := Options{
		Root:           root,
		CacheHash:      "abcdef1234567890",
		ExpectedDigest: catalog.ModelFilesDigest(source.Files),
		ExpectedSize:   1,
		ModelID:        "test-model",
		Revision:       source.Revision,
		Source:         &source,
	}
	if err := Run(context.Background(), options); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("expected source path traversal rejection, got %v", err)
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
	options := Options{Root: root, CacheHash: "abcdef1234567890", ExpectedDigest: metadata.Digest, ExpectedSize: metadata.SizeBytes, Synthetic: true}
	if err := Run(context.Background(), options); err != nil {
		t.Fatalf("seed run failed: %v", err)
	}
	invalid := bytes.Repeat([]byte("x"), int(metadata.SizeBytes))
	if err := os.WriteFile(filepath.Join(root, options.CacheHash, cacheartifact.ArtifactFileName), invalid, 0o644); err != nil {
		t.Fatalf("corrupt artifact: %v", err)
	}
	options = Options{Root: root, CacheHash: "abcdef1234567890", ExpectedDigest: metadata.Digest, ExpectedSize: metadata.SizeBytes, VerifyOnly: true}
	if err := Run(context.Background(), options); err == nil || !strings.Contains(err.Error(), "validate safetensors file") {
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
