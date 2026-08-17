package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

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

type ModelFile struct {
	Path        string `json:"path"`
	Digest      string `json:"digest"`
	SizeBytes   int64  `json:"sizeBytes"`
	Safetensors bool   `json:"safetensors,omitempty"`
}

type ModelSource struct {
	BaseURL    string
	Repository string
	Revision   string
	Files      []ModelFile
}

type Model struct {
	ID                 string
	Revision           string
	Digest             string
	SizeBytes          int64
	EngineImage        string
	SimulationImage    string
	ServedModelName    string
	Quantization       string
	MaxModelLength     int32
	LoadDelaySeconds   int32
	AllowedProfiles    []string
	NodePoolSelector   map[string]string
	Source             ModelSource
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
		ID:               "qwen2.5-7b-instruct-awq",
		Revision:         "b25037543e9394b818fdfca67ab2a00ecc7dd641",
		Digest:           "sha256:41d12f80b6d62f01e9134f410ab177d907ccb025e41bbb651bd83e8e8304f010",
		SizeBytes:        5582381128,
		EngineImage:      "vllm/vllm-openai@sha256:6cf9808ca8810fc6c3fd0451c2e7784fb224590d81f7db338e7eaf3c02a33d33",
		SimulationImage:  "ember-mock-engine:dev",
		ServedModelName:  "qwen2.5-7b-instruct-awq",
		Quantization:     "awq",
		MaxModelLength:   32768,
		LoadDelaySeconds: 15,
		AllowedProfiles:  []string{"small", "standard", "tp2"},
		NodePoolSelector: map[string]string{DefaultGPUNodeLabelKey: DefaultGPUNodeLabelValue},
		Source: ModelSource{
			BaseURL:    "https://huggingface.co",
			Repository: "Qwen/Qwen2.5-7B-Instruct-AWQ",
			Revision:   "b25037543e9394b818fdfca67ab2a00ecc7dd641",
			Files: []ModelFile{
				{Path: "config.json", SizeBytes: 841, Digest: "sha256:ec0c1f5f875ad8bc1f78c5140c22dbdde1b55478442ad358e7a4d9ecf947a327"},
				{Path: "generation_config.json", SizeBytes: 243, Digest: "sha256:9df8ac8558514924f3481a609f1e5590ab4416efc600b12ad805155d79581c59"},
				{Path: "merges.txt", SizeBytes: 1671839, Digest: "sha256:599bab54075088774b1733fde865d5bd747cbcc7a547c5bc12610e874e26f5e3"},
				{Path: "model-00001-of-00002.safetensors", SizeBytes: 3996422976, Digest: "sha256:4ad6e70f93823a9228931615e04b5e9293854c236fb4bd5f85e27f3b600605ad", Safetensors: true},
				{Path: "model-00002-of-00002.safetensors", SizeBytes: 1574406784, Digest: "sha256:920a8cc9d3c81668f18014a69bb041ab8bab22a31640a713f90f5e7580e2f126", Safetensors: true},
				{Path: "model.safetensors.index.json", SizeBytes: 62662, Digest: "sha256:559b6859f37625080bf49eea80806ecc2f859a13b316b7fae2ff4432b6308e36"},
				{Path: "tokenizer.json", SizeBytes: 7031645, Digest: "sha256:c0382117ea329cdf097041132f6d735924b697924d6f6fc3945713e96ce87539"},
				{Path: "tokenizer_config.json", SizeBytes: 7305, Digest: "sha256:5b5d4f65d0acd3b2d56a35b56d374a36cbc1c8fa5cf3b3febbbfabf22f359583"},
				{Path: "vocab.json", SizeBytes: 2776833, Digest: "sha256:ca10d7e9fb3ed18575dd1e277a2579c16d108e32f27439684afa0e10b1440910"},
			},
		},
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
	if !ok {
		return Model{}, false
	}
	return cloneModel(model), true
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
		model, _ := LookupModel(id)
		values = append(values, model)
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

func ModelFilesDigest(files []ModelFile) string {
	sortedFiles := append([]ModelFile(nil), files...)
	sort.Slice(sortedFiles, func(i, j int) bool {
		return sortedFiles[i].Path < sortedFiles[j].Path
	})
	hasher := sha256.New()
	for _, file := range sortedFiles {
		hasher.Write([]byte(file.Path))
		hasher.Write([]byte{0})
		hasher.Write([]byte(strconv.FormatInt(file.SizeBytes, 10)))
		hasher.Write([]byte{0})
		hasher.Write([]byte(file.Digest))
		hasher.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

func ModelFilesSize(files []ModelFile) int64 {
	var size int64
	for _, file := range files {
		size += file.SizeBytes
	}
	return size
}

func (model Model) Artifact(simulationMode bool) Artifact {
	if simulationMode {
		return model.SimulationArtifact
	}
	return Artifact{
		Digest:      model.Digest,
		SizeBytes:   model.SizeBytes,
		Description: "Reviewed immutable model file manifest.",
	}
}

func (model Model) EngineImageForMode(simulationMode bool) string {
	if simulationMode {
		return model.SimulationImage
	}
	return model.EngineImage
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

func cloneModel(model Model) Model {
	model.AllowedProfiles = append([]string(nil), model.AllowedProfiles...)
	model.NodePoolSelector = CopySelector(model.NodePoolSelector)
	model.Source.Files = append([]ModelFile(nil), model.Source.Files...)
	return model
}
