package prefetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RuokeZhang/ember/internal/cachefs"
	"github.com/RuokeZhang/ember/operator/cacheartifact"
)

var cacheHashPattern = regexp.MustCompile(`^[a-f0-9]{8,64}$`)

type Options struct {
	Root           string
	CacheHash      string
	ExpectedDigest string
	ExpectedSize   int64
	Synthetic      bool
	VerifyOnly     bool
	PrepareRoot    bool
	Logger         *slog.Logger
}

type completionManifest struct {
	CacheHash string `json:"cacheHash"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	Mode      string `json:"mode"`
}

func Run(ctx context.Context, options Options) error {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	if options.PrepareRoot {
		logger.InfoContext(ctx, "preparing cache root", "root", options.Root)
		return cachefs.PrepareRoot(options.Root)
	}
	root, finalDir, tmpDir, err := validatePaths(options.Root, options.CacheHash)
	if err != nil {
		logger.ErrorContext(ctx, "invalid prefetch paths", "error", err)
		return err
	}
	if options.ExpectedDigest == "" {
		return fmt.Errorf("expected digest is required")
	}
	if options.VerifyOnly {
		logger.InfoContext(ctx, "verifying existing cache directory", "root", root, "cacheHash", options.CacheHash)
		return verifyDirectory(finalDir, options.ExpectedDigest, options.ExpectedSize)
	}
	if !options.Synthetic {
		return fmt.Errorf("only synthetic mode is supported in Phase 2")
	}
	logger.InfoContext(ctx, "materializing synthetic safetensors artifact", "root", root, "cacheHash", options.CacheHash)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create cache root: %w", err)
	}
	if err := verifyIfPresent(finalDir, options.ExpectedDigest, options.ExpectedSize); err == nil {
		logger.InfoContext(ctx, "cache already present and verified", "cacheHash", options.CacheHash)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		logger.InfoContext(ctx, "existing cache requires rewrite", "cacheHash", options.CacheHash, "error", err)
	}
	if err := removeStagingDir(root, tmpDir); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	artifactPath := filepath.Join(tmpDir, cacheartifact.ArtifactFileName)
	manifestPath := filepath.Join(tmpDir, cacheartifact.ManifestFileName)
	data := cacheartifact.SimulationArtifactBytes()
	if err := cacheartifact.ValidateSafetensorsBytes(data); err != nil {
		return fmt.Errorf("generated safetensors artifact invalid: %w", err)
	}
	if digest := cacheartifact.DigestBytes(data); digest != options.ExpectedDigest {
		return fmt.Errorf("generated digest %s does not match expected %s", digest, options.ExpectedDigest)
	}
	if options.ExpectedSize > 0 && int64(len(data)) != options.ExpectedSize {
		return fmt.Errorf("generated size %d does not match expected %d", len(data), options.ExpectedSize)
	}
	if err := writeFileAndSync(artifactPath, data, 0o644); err != nil {
		return err
	}
	manifestBytes, err := json.MarshalIndent(completionManifest{
		CacheHash: options.CacheHash,
		Digest:    options.ExpectedDigest,
		SizeBytes: int64(len(data)),
		Mode:      "synthetic",
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := writeFileAndSync(manifestPath, manifestBytes, 0o644); err != nil {
		return err
	}
	if err := syncPath(tmpDir); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return fmt.Errorf("rename staging directory: %w", err)
	}
	if err := syncPath(root); err != nil {
		return err
	}
	if err := verifyDirectory(finalDir, options.ExpectedDigest, options.ExpectedSize); err != nil {
		return err
	}
	logger.InfoContext(ctx, "cache materialized successfully", "cacheHash", options.CacheHash, "path", finalDir)
	return nil
}

func verifyIfPresent(finalDir, expectedDigest string, expectedSize int64) error {
	if _, err := os.Stat(finalDir); err != nil {
		return err
	}
	return verifyDirectory(finalDir, expectedDigest, expectedSize)
}

func verifyDirectory(finalDir, expectedDigest string, expectedSize int64) error {
	artifactPath := filepath.Join(finalDir, cacheartifact.ArtifactFileName)
	manifestPath := filepath.Join(finalDir, cacheartifact.ManifestFileName)
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fs.ErrNotExist
		}
		return fmt.Errorf("read artifact: %w", err)
	}
	if err := cacheartifact.ValidateSafetensorsBytes(data); err != nil {
		return fmt.Errorf("validate safetensors artifact: %w", err)
	}
	if got := cacheartifact.DigestBytes(data); got != expectedDigest {
		return fmt.Errorf("artifact digest mismatch: got %s want %s", got, expectedDigest)
	}
	if expectedSize > 0 && int64(len(data)) != expectedSize {
		return fmt.Errorf("artifact size mismatch: got %d want %d", len(data), expectedSize)
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest completionManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.Digest != expectedDigest {
		return fmt.Errorf("manifest digest mismatch: got %s want %s", manifest.Digest, expectedDigest)
	}
	return nil
}

func validatePaths(root, cacheHash string) (string, string, string, error) {
	if strings.TrimSpace(root) == "" {
		return "", "", "", fmt.Errorf("root is required")
	}
	if !cacheHashPattern.MatchString(cacheHash) {
		return "", "", "", fmt.Errorf("cache hash must be lowercase hex without path separators")
	}
	cleanRoot := filepath.Clean(root)
	if !filepath.IsAbs(cleanRoot) {
		return "", "", "", fmt.Errorf("root must be an absolute path")
	}
	finalDir := filepath.Join(cleanRoot, cacheHash)
	tmpDir := filepath.Join(cleanRoot, cacheHash+".tmp")
	for _, candidate := range []string{finalDir, tmpDir} {
		rel, err := filepath.Rel(cleanRoot, candidate)
		if err != nil {
			return "", "", "", err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", "", "", fmt.Errorf("resolved path escapes root")
		}
	}
	return cleanRoot, finalDir, tmpDir, nil
}

func removeStagingDir(root, tmpDir string) error {
	if _, err := os.Stat(tmpDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat staging directory: %w", err)
	}
	rel, err := filepath.Rel(root, tmpDir)
	if err != nil {
		return err
	}
	if filepath.Dir(rel) != "." || !strings.HasSuffix(rel, ".tmp") {
		return fmt.Errorf("refusing to remove unexpected staging path %q", tmpDir)
	}
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("remove stale staging directory: %w", err)
	}
	return nil
}

func writeFileAndSync(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

func syncPath(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open path for sync %s: %w", path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync path %s: %w", path, err)
	}
	return nil
}
