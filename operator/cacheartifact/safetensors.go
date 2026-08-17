package cacheartifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

const (
	ArtifactFileName = "weights.safetensors"
	ManifestFileName = "manifest.json"
	SimulationFormat = "ember-simulation"
	maxHeaderSize    = 64 << 20
)

type Metadata struct {
	Digest    string
	SizeBytes int64
}

type tensorHeader struct {
	DType       string   `json:"dtype"`
	Shape       *[]int64 `json:"shape"`
	DataOffsets *[]int64 `json:"data_offsets"`
}

type tensorRange struct {
	Name  string
	Start int64
	End   int64
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
	return validateSafetensors(bytes.NewReader(data), int64(len(data)))
}

func ValidateSafetensorsFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open safetensors file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat safetensors file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("safetensors path is not a regular file")
	}
	return validateSafetensors(file, info.Size())
}

func validateSafetensors(reader io.ReaderAt, fileSize int64) error {
	if fileSize < 8 {
		return fmt.Errorf("safetensors file too small")
	}
	var prefix [8]byte
	if _, err := reader.ReadAt(prefix[:], 0); err != nil {
		return fmt.Errorf("read safetensors header length: %w", err)
	}
	headerLength := binary.LittleEndian.Uint64(prefix[:])
	if headerLength == 0 {
		return fmt.Errorf("safetensors header is empty")
	}
	if headerLength > maxHeaderSize {
		return fmt.Errorf("safetensors header exceeds %d-byte safety limit", maxHeaderSize)
	}
	if headerLength > uint64(fileSize-8) {
		return fmt.Errorf("safetensors header length exceeds file size")
	}
	headerBytes := make([]byte, int(headerLength))
	if _, err := io.ReadFull(io.NewSectionReader(reader, 8, int64(headerLength)), headerBytes); err != nil {
		return fmt.Errorf("read safetensors header: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(headerBytes, &raw); err != nil {
		return fmt.Errorf("invalid safetensors header JSON: %w", err)
	}
	if metadata, ok := raw["__metadata__"]; ok {
		var values map[string]string
		if err := json.Unmarshal(metadata, &values); err != nil {
			return fmt.Errorf("invalid __metadata__ section: %w", err)
		}
	}

	payloadLength := fileSize - 8 - int64(headerLength)
	ranges := make([]tensorRange, 0, len(raw))
	for name, value := range raw {
		if name == "__metadata__" {
			continue
		}
		if name == "" {
			return fmt.Errorf("tensor name must not be empty")
		}
		var tensor tensorHeader
		if err := json.Unmarshal(value, &tensor); err != nil {
			return fmt.Errorf("invalid tensor header %q: %w", name, err)
		}
		if tensor.DType == "" {
			return fmt.Errorf("tensor %q missing dtype", name)
		}
		if tensor.Shape == nil {
			return fmt.Errorf("tensor %q missing shape", name)
		}
		for _, dimension := range *tensor.Shape {
			if dimension < 0 {
				return fmt.Errorf("tensor %q has a negative shape dimension", name)
			}
		}
		if tensor.DataOffsets == nil || len(*tensor.DataOffsets) != 2 {
			return fmt.Errorf("tensor %q must have exactly two data offsets", name)
		}
		start := (*tensor.DataOffsets)[0]
		end := (*tensor.DataOffsets)[1]
		if start < 0 || end < start {
			return fmt.Errorf("tensor %q has invalid data offsets", name)
		}
		if end > payloadLength {
			return fmt.Errorf("tensor %q extends beyond payload", name)
		}
		ranges = append(ranges, tensorRange{Name: name, Start: start, End: end})
	}
	if len(ranges) == 0 {
		return fmt.Errorf("safetensors file contains no tensors")
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start == ranges[j].Start {
			return ranges[i].End < ranges[j].End
		}
		return ranges[i].Start < ranges[j].Start
	})
	var cursor int64
	for _, current := range ranges {
		if current.Start != cursor {
			return fmt.Errorf("tensor %q starts at %d after payload offset %d", current.Name, current.Start, cursor)
		}
		cursor = current.End
	}
	if cursor != payloadLength {
		return fmt.Errorf("payload length mismatch: tensor data ends at %d, payload %d", cursor, payloadLength)
	}
	return nil
}
