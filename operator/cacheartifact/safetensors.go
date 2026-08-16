package cacheartifact

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	ArtifactFileName = "weights.safetensors"
	ManifestFileName = "manifest.json"
	SimulationFormat = "ember-simulation"
)

type Metadata struct {
	Digest    string
	SizeBytes int64
}

type tensorHeader struct {
	DType       string   `json:"dtype"`
	Shape       []int64  `json:"shape"`
	DataOffsets [2]int64 `json:"data_offsets"`
}

func SimulationArtifactBytes() []byte {
	header := []byte(`{"__metadata__":{"format":"` + SimulationFormat + `"},"weight":{"dtype":"U8","shape":[4],"data_offsets":[0,4]}}`)
	payload := []byte{0x45, 0x4d, 0x42, 0x52}
	buf := make([]byte, 8+len(header)+len(payload))
	binary.LittleEndian.PutUint64(buf[:8], uint64(len(header)))
	copy(buf[8:], header)
	copy(buf[8+len(header):], payload)
	return buf
}

func SimulationMetadata() Metadata {
	data := SimulationArtifactBytes()
	return Metadata{Digest: DigestBytes(data), SizeBytes: int64(len(data))}
}

func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ValidateSafetensorsBytes(data []byte) error {
	if len(data) < 8 {
		return fmt.Errorf("safetensors file too small")
	}
	headerLength := binary.LittleEndian.Uint64(data[:8])
	if headerLength == 0 {
		return fmt.Errorf("safetensors header is empty")
	}
	if headerLength > uint64(len(data)-8) {
		return fmt.Errorf("safetensors header length exceeds file size")
	}
	headerBytes := data[8 : 8+headerLength]
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(headerBytes, &raw); err != nil {
		return fmt.Errorf("invalid safetensors header JSON: %w", err)
	}
	metadata, ok := raw["__metadata__"]
	if !ok {
		return fmt.Errorf("missing __metadata__ section")
	}
	var meta struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(metadata, &meta); err != nil {
		return fmt.Errorf("invalid __metadata__ section: %w", err)
	}
	if meta.Format == "" {
		return fmt.Errorf("missing safetensors format metadata")
	}

	payloadLength := int64(len(data) - 8 - int(headerLength))
	var tensorCount int
	var maxEnd int64
	for name, value := range raw {
		if name == "__metadata__" {
			continue
		}
		var tensor tensorHeader
		if err := json.Unmarshal(value, &tensor); err != nil {
			return fmt.Errorf("invalid tensor header %q: %w", name, err)
		}
		if tensor.DType == "" {
			return fmt.Errorf("tensor %q missing dtype", name)
		}
		if len(tensor.Shape) == 0 {
			return fmt.Errorf("tensor %q missing shape", name)
		}
		start := tensor.DataOffsets[0]
		end := tensor.DataOffsets[1]
		if start < 0 || end < start {
			return fmt.Errorf("tensor %q has invalid data offsets", name)
		}
		if end > payloadLength {
			return fmt.Errorf("tensor %q extends beyond payload", name)
		}
		if end > maxEnd {
			maxEnd = end
		}
		tensorCount++
	}
	if tensorCount == 0 {
		return fmt.Errorf("safetensors file contains no tensors")
	}
	if maxEnd != payloadLength {
		return fmt.Errorf("payload length mismatch: max tensor end %d, payload %d", maxEnd, payloadLength)
	}
	return nil
}
