package catalog

import (
	"testing"

	"github.com/RuokeZhang/ember/operator/cacheartifact"
)

func TestCacheHashAndLabelKeyAreDeterministic(t *testing.T) {
	model, ok := LookupModel("qwen2.5-7b-instruct-awq")
	if !ok {
		t.Fatal("expected catalog model")
	}
	hash := CacheHashForModel(model)
	if len(hash) != 16 {
		t.Fatalf("expected 16-char cache hash, got %q", hash)
	}
	if CacheHash(model.ID, model.Revision) != hash {
		t.Fatal("cache hash should be deterministic")
	}
	if CacheLabelKeyForModel(model) != "cache.ember.dev/"+hash {
		t.Fatalf("unexpected cache label key %q", CacheLabelKeyForModel(model))
	}
	if ModelCacheNameForModel(model) != "mc-"+hash {
		t.Fatalf("unexpected model cache name %q", ModelCacheNameForModel(model))
	}
}

func TestSimulationArtifactMetadataIsExplicitAndSeparate(t *testing.T) {
	model, _ := LookupModel("qwen2.5-7b-instruct-awq")
	metadata := cacheartifact.SimulationMetadata()
	if model.SimulationArtifact.Digest != metadata.Digest || model.SimulationArtifact.SizeBytes != metadata.SizeBytes {
		t.Fatalf("expected simulation artifact metadata to match generated artifact, got %#v", model.SimulationArtifact)
	}
	if model.SimulationArtifact.Digest == model.Digest {
		t.Fatal("simulation artifact digest must not equal reviewed real model digest")
	}
	if model.EngineImageForMode(true) != model.SimulationImage || model.EngineImageForMode(false) != model.EngineImage {
		t.Fatal("expected simulation and real runtime images to remain separate")
	}
}

func TestRealModelManifestIsImmutableAndSelfConsistent(t *testing.T) {
	model, _ := LookupModel("qwen2.5-7b-instruct-awq")
	if model.Revision != "b25037543e9394b818fdfca67ab2a00ecc7dd641" || model.Source.Revision != model.Revision {
		t.Fatalf("unexpected immutable revision: model=%q source=%q", model.Revision, model.Source.Revision)
	}
	if model.Source.BaseURL != "https://huggingface.co" || model.Source.Repository != "Qwen/Qwen2.5-7B-Instruct-AWQ" {
		t.Fatalf("unexpected reviewed source: %#v", model.Source)
	}
	if len(model.Source.Files) != 9 {
		t.Fatalf("expected nine runtime files, got %d", len(model.Source.Files))
	}
	if got := ModelFilesDigest(model.Source.Files); got != model.Digest {
		t.Fatalf("manifest digest mismatch: got %q want %q", got, model.Digest)
	}
	if got := ModelFilesSize(model.Source.Files); got != model.SizeBytes {
		t.Fatalf("manifest size mismatch: got %d want %d", got, model.SizeBytes)
	}
}

func TestLookupModelReturnsIndependentManifest(t *testing.T) {
	first, _ := LookupModel("qwen2.5-7b-instruct-awq")
	first.Source.Files[0].Digest = "sha256:mutated"
	first.NodePoolSelector[DefaultGPUNodeLabelKey] = "mutated"

	second, _ := LookupModel("qwen2.5-7b-instruct-awq")
	if second.Source.Files[0].Digest == "sha256:mutated" || second.NodePoolSelector[DefaultGPUNodeLabelKey] == "mutated" {
		t.Fatal("catalog lookup returned mutable shared state")
	}
}
