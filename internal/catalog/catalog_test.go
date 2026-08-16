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
}
