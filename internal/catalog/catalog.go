package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/RuokeZhang/ember/operator/cacheartifact"
)

const (
	CacheRoot                = "/var/lib/ember/models"
	DefaultRetentionPolicy   = "LRUWithFloor"
	DefaultGPUNodeLabelKey   = "ember.dev/gpu"
	DefaultGPUNodeLabelValue = "l4"
)

type Artifact struct {
	Digest      string
	SizeBytes   int64
	Description string
}

type Model struct {
	ID                 string
	Revision           string
	Digest             string
	SizeBytes          int64
	EngineImage        string
	LoadDelaySeconds   int32
	AllowedProfiles    []string
	NodePoolSelector   map[string]string
	SimulationArtifact Artifact
}

type Profile struct {
	Name          string
	GPUCount      int32
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
	ShmSize       string
}

var simulationArtifact = func() Artifact {
	metadata := cacheartifact.SimulationMetadata()
	return Artifact{
		Digest:      metadata.Digest,
		SizeBytes:   metadata.SizeBytes,
		Description: "Synthetic deterministic safetensors artifact for cache-controller and Kind validation.",
	}
}()

var models = map[string]Model{
	"qwen2.5-7b-instruct-awq": {
		ID:                 "qwen2.5-7b-instruct-awq",
		Revision:           "9c1f4ae",
		Digest:             "sha256:4f2aa8c5b932ed2c4324486f6d1f6f0a4b856d76f6e4e120b1d40db65e8d29b5",
		SizeBytes:          5872025600,
		EngineImage:        "ember-mock-engine:dev",
		LoadDelaySeconds:   15,
		AllowedProfiles:    []string{"small", "standard", "tp2"},
		NodePoolSelector:   map[string]string{DefaultGPUNodeLabelKey: DefaultGPUNodeLabelValue},
		SimulationArtifact: simulationArtifact,
	},
}

var profiles = map[string]Profile{
	"small": {
		Name:          "small",
		GPUCount:      1,
		CPURequest:    "2",
		CPULimit:      "4",
		MemoryRequest: "12Gi",
		MemoryLimit:   "12Gi",
		ShmSize:       "1Gi",
	},
	"standard": {
		Name:          "standard",
		GPUCount:      1,
		CPURequest:    "4",
		CPULimit:      "8",
		MemoryRequest: "20Gi",
		MemoryLimit:   "20Gi",
		ShmSize:       "2Gi",
	},
	"tp2": {
		Name:          "tp2",
		GPUCount:      2,
		CPURequest:    "8",
		CPULimit:      "16",
		MemoryRequest: "40Gi",
		MemoryLimit:   "40Gi",
		ShmSize:       "8Gi",
	},
}

func LookupModel(id string) (Model, bool) {
	model, ok := models[id]
	return model, ok
}

func LookupProfile(name string) (Profile, bool) {
	profile, ok := profiles[name]
	return profile, ok
}

func SimulationProfile(profile Profile) Profile {
	profile.CPURequest = "100m"
	profile.CPULimit = "500m"
	profile.MemoryRequest = "128Mi"
	profile.MemoryLimit = "512Mi"
	profile.ShmSize = "64Mi"
	return profile
}

func ModelIDs() []string {
	values := make([]string, 0, len(models))
	for id := range models {
		values = append(values, id)
	}
	sort.Strings(values)
	return values
}

func ProfileNames() []string {
	values := make([]string, 0, len(profiles))
	for id := range profiles {
		values = append(values, id)
	}
	sort.Strings(values)
	return values
}

func Models() []Model {
	ids := ModelIDs()
	values := make([]Model, 0, len(ids))
	for _, id := range ids {
		values = append(values, models[id])
	}
	return values
}

func Profiles() []Profile {
	names := ProfileNames()
	values := make([]Profile, 0, len(names))
	for _, name := range names {
		values = append(values, profiles[name])
	}
	return values
}

func CacheIdentity(modelID, revision string) string {
	return modelID + ":" + revision
}

func CacheHash(modelID, revision string) string {
	sum := sha256.Sum256([]byte(CacheIdentity(modelID, revision)))
	return hex.EncodeToString(sum[:8])
}

func CacheHashForModel(model Model) string {
	return CacheHash(model.ID, model.Revision)
}

func CacheLabelKey(modelID, revision string) string {
	return "cache.ember.dev/" + CacheHash(modelID, revision)
}

func CacheLabelKeyForModel(model Model) string {
	return CacheLabelKey(model.ID, model.Revision)
}

func ModelCacheName(modelID, revision string) string {
	return "mc-" + CacheHash(modelID, revision)
}

func ModelCacheNameForModel(model Model) string {
	return ModelCacheName(model.ID, model.Revision)
}

func CopySelector(selector map[string]string) map[string]string {
	if len(selector) == 0 {
		return nil
	}
	copied := make(map[string]string, len(selector))
	for key, value := range selector {
		copied[key] = value
	}
	return copied
}
