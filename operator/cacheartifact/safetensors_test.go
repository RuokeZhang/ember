package cacheartifact

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSafetensorsAcceptsOptionalMetadata(t *testing.T) {
	data := safetensorsBytes(`{"weight":{"dtype":"U8","shape":[4],"data_offsets":[0,4]}}`, []byte{1, 2, 3, 4})
	if err := ValidateSafetensorsBytes(data); err != nil {
		t.Fatalf("expected metadata-free safetensors to be valid: %v", err)
	}
}

func TestValidateSafetensorsFileReadsHeaderWithoutLoadingPayload(t *testing.T) {
	data := safetensorsBytes(`{"__metadata__":{"format":"pt"},"weight":{"dtype":"U8","shape":[4],"data_offsets":[0,4]}}`, []byte{1, 2, 3, 4})
	path := filepath.Join(t.TempDir(), "weights.safetensors")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write test artifact: %v", err)
	}
	if err := ValidateSafetensorsFile(path); err != nil {
		t.Fatalf("validate file: %v", err)
	}
}

func TestValidateSafetensorsRejectsPayloadGaps(t *testing.T) {
	data := safetensorsBytes(`{"first":{"dtype":"U8","shape":[1],"data_offsets":[0,1]},"second":{"dtype":"U8","shape":[1],"data_offsets":[2,3]}}`, []byte{1, 2, 3})
	err := ValidateSafetensorsBytes(data)
	if err == nil || !strings.Contains(err.Error(), "starts at 2 after payload offset 1") {
		t.Fatalf("expected payload gap rejection, got %v", err)
	}
}

func TestSimulationArtifactRemainsValid(t *testing.T) {
	if err := ValidateSafetensorsBytes(SimulationArtifactBytes()); err != nil {
		t.Fatalf("simulation artifact invalid: %v", err)
	}
}

func safetensorsBytes(header string, payload []byte) []byte {
	data := make([]byte, 8+len(header)+len(payload))
	binary.LittleEndian.PutUint64(data[:8], uint64(len(header)))
	copy(data[8:], header)
	copy(data[8+len(header):], payload)
	return data
}
