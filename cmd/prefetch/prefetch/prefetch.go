package prefetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/RuokeZhang/ember/internal/cachefs"
	"github.com/RuokeZhang/ember/internal/catalog"
	"github.com/RuokeZhang/ember/operator/cacheartifact"
)

const maxManifestSize = 1 << 20

var (
	cacheHashPattern = regexp.MustCompile(`^[a-f0-9]{8,64}$`)
	digestPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	revisionPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

type Options struct {
	Root           string
	CacheHash      string
	ExpectedDigest string
	ExpectedSize   int64
	ModelID        string
	Revision       string
	Synthetic      bool
	VerifyOnly     bool
	PrepareRoot    bool
	Logger         *slog.Logger

	Source     *catalog.ModelSource
	HTTPClient *http.Client
}

type completionManifest struct {
	CacheHash string              `json:"cacheHash"`
	Digest    string              `json:"digest"`
	SizeBytes int64               `json:"sizeBytes"`
	Mode      string              `json:"mode"`
	ModelID   string              `json:"modelID,omitempty"`
	Revision  string              `json:"revision,omitempty"`
	Files     []catalog.ModelFile `json:"files"`
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
	if !digestPattern.MatchString(options.ExpectedDigest) {
		return fmt.Errorf("expected digest must be a lowercase sha256 digest")
	}
	if options.ExpectedSize < 0 {
		return fmt.Errorf("expected size must not be negative")
	}
	if options.VerifyOnly {
		logger.InfoContext(ctx, "verifying existing cache directory", "root", root, "cacheHash", options.CacheHash)
		return verifyDirectory(finalDir, options.CacheHash, options.ExpectedDigest, options.ExpectedSize)
	}

	var source catalog.ModelSource
	if !options.Synthetic {
		source, err = resolveSource(options)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create cache root: %w", err)
	}
	if err := verifyIfPresent(finalDir, options.CacheHash, options.ExpectedDigest, options.ExpectedSize); err == nil {
		logger.InfoContext(ctx, "cache already present and verified", "cacheHash", options.CacheHash)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		logger.InfoContext(ctx, "removing invalid cache entry before rewrite", "cacheHash", options.CacheHash, "error", err)
		if err := removeManagedDir(root, finalDir, options.CacheHash); err != nil {
			return err
		}
	}
	if err := removeManagedDir(root, tmpDir, options.CacheHash+".tmp"); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = removeManagedDir(root, tmpDir, options.CacheHash+".tmp")
		}
	}()

	var manifest completionManifest
	if options.Synthetic {
		logger.InfoContext(ctx, "materializing synthetic safetensors artifact", "root", root, "cacheHash", options.CacheHash)
		manifest, err = materializeSynthetic(tmpDir, options)
	} else {
		logger.InfoContext(ctx, "materializing immutable model files", "root", root, "cacheHash", options.CacheHash, "modelID", options.ModelID, "revision", options.Revision)
		manifest, err = materializeReal(ctx, tmpDir, options, source, logger)
	}
	if err != nil {
		return err
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := writeFileAndSync(filepath.Join(tmpDir, cacheartifact.ManifestFileName), manifestBytes, 0o644); err != nil {
		return err
	}
	if err := syncDirectories(tmpDir); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		if verifyErr := verifyDirectory(finalDir, options.CacheHash, options.ExpectedDigest, options.ExpectedSize); verifyErr == nil {
			logger.InfoContext(ctx, "cache concurrently materialized and verified", "cacheHash", options.CacheHash)
			return nil
		}
		return fmt.Errorf("rename staging directory: %w", err)
	}
	committed = true
	if err := syncPath(root); err != nil {
		return err
	}
	if err := verifyDirectory(finalDir, options.CacheHash, options.ExpectedDigest, options.ExpectedSize); err != nil {
		return err
	}
	logger.InfoContext(ctx, "cache materialized successfully", "cacheHash", options.CacheHash, "path", finalDir)
	return nil
}

func resolveSource(options Options) (catalog.ModelSource, error) {
	var source catalog.ModelSource
	allowHTTP := false
	if options.Source != nil {
		source = cloneSource(*options.Source)
		allowHTTP = true
	} else {
		if strings.TrimSpace(options.ModelID) == "" {
			return catalog.ModelSource{}, fmt.Errorf("model ID is required outside synthetic mode")
		}
		model, ok := catalog.LookupModel(options.ModelID)
		if !ok {
			return catalog.ModelSource{}, fmt.Errorf("model %q is not in the reviewed catalog", options.ModelID)
		}
		if options.Revision != model.Revision {
			return catalog.ModelSource{}, fmt.Errorf("revision %q does not match reviewed catalog revision %q", options.Revision, model.Revision)
		}
		if options.ExpectedDigest != model.Digest || options.ExpectedSize != model.SizeBytes {
			return catalog.ModelSource{}, fmt.Errorf("expected artifact metadata does not match the reviewed catalog")
		}
		source = cloneSource(model.Source)
	}
	if strings.TrimSpace(options.ModelID) == "" || strings.TrimSpace(options.Revision) == "" {
		return catalog.ModelSource{}, fmt.Errorf("model ID and revision are required outside synthetic mode")
	}
	if source.Revision != options.Revision {
		return catalog.ModelSource{}, fmt.Errorf("source revision %q does not match requested revision %q", source.Revision, options.Revision)
	}
	if err := validateSource(source, allowHTTP); err != nil {
		return catalog.ModelSource{}, err
	}
	if got := catalog.ModelFilesDigest(source.Files); got != options.ExpectedDigest {
		return catalog.ModelSource{}, fmt.Errorf("source manifest digest mismatch: got %s want %s", got, options.ExpectedDigest)
	}
	if got := catalog.ModelFilesSize(source.Files); got != options.ExpectedSize {
		return catalog.ModelSource{}, fmt.Errorf("source manifest size mismatch: got %d want %d", got, options.ExpectedSize)
	}
	sort.Slice(source.Files, func(i, j int) bool {
		return source.Files[i].Path < source.Files[j].Path
	})
	return source, nil
}

func validateSource(source catalog.ModelSource, allowHTTP bool) error {
	baseURL, err := url.Parse(source.BaseURL)
	if err != nil {
		return fmt.Errorf("parse source base URL: %w", err)
	}
	if baseURL.Scheme != "https" && !(allowHTTP && baseURL.Scheme == "http") {
		return fmt.Errorf("source base URL must use HTTPS")
	}
	if baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return fmt.Errorf("source base URL must contain only scheme, host, and optional path")
	}
	if err := validateRepository(source.Repository); err != nil {
		return err
	}
	if !revisionPattern.MatchString(source.Revision) {
		return fmt.Errorf("source revision contains unsupported characters")
	}
	if len(source.Files) == 0 {
		return fmt.Errorf("source file manifest must not be empty")
	}
	seen := make(map[string]struct{}, len(source.Files))
	for _, file := range source.Files {
		if err := validateModelFile(file); err != nil {
			return err
		}
		if _, ok := seen[file.Path]; ok {
			return fmt.Errorf("source file manifest contains duplicate path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
	}
	return nil
}

func validateRepository(repository string) error {
	if repository == "" || strings.Contains(repository, `\`) || path.IsAbs(repository) {
		return fmt.Errorf("source repository must be a relative slash-separated path")
	}
	clean := path.Clean(repository)
	if clean != repository || clean == "." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("source repository contains unsafe path components")
	}
	return nil
}

func validateModelFile(file catalog.ModelFile) error {
	if file.Path == "" || strings.Contains(file.Path, `\`) || path.IsAbs(file.Path) {
		return fmt.Errorf("model file path %q must be relative and slash-separated", file.Path)
	}
	clean := path.Clean(file.Path)
	if clean != file.Path || clean == "." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("model file path %q contains unsafe path components", file.Path)
	}
	if file.Path == cacheartifact.ManifestFileName {
		return fmt.Errorf("model file path %q conflicts with the completion manifest", file.Path)
	}
	if file.SizeBytes <= 0 {
		return fmt.Errorf("model file %q must have a positive size", file.Path)
	}
	if !digestPattern.MatchString(file.Digest) {
		return fmt.Errorf("model file %q has an invalid sha256 digest", file.Path)
	}
	if file.Safetensors && path.Ext(file.Path) != ".safetensors" {
		return fmt.Errorf("model file %q is marked safetensors without a .safetensors suffix", file.Path)
	}
	return nil
}

func materializeSynthetic(tmpDir string, options Options) (completionManifest, error) {
	data := cacheartifact.SimulationArtifactBytes()
	if err := cacheartifact.ValidateSafetensorsBytes(data); err != nil {
		return completionManifest{}, fmt.Errorf("generated safetensors artifact invalid: %w", err)
	}
	digest := cacheartifact.DigestBytes(data)
	if digest != options.ExpectedDigest {
		return completionManifest{}, fmt.Errorf("generated digest %s does not match expected %s", digest, options.ExpectedDigest)
	}
	if options.ExpectedSize > 0 && int64(len(data)) != options.ExpectedSize {
		return completionManifest{}, fmt.Errorf("generated size %d does not match expected %d", len(data), options.ExpectedSize)
	}
	if err := writeFileAndSync(filepath.Join(tmpDir, cacheartifact.ArtifactFileName), data, 0o644); err != nil {
		return completionManifest{}, err
	}
	file := catalog.ModelFile{
		Path:        cacheartifact.ArtifactFileName,
		Digest:      digest,
		SizeBytes:   int64(len(data)),
		Safetensors: true,
	}
	return completionManifest{
		CacheHash: options.CacheHash,
		Digest:    options.ExpectedDigest,
		SizeBytes: int64(len(data)),
		Mode:      "synthetic",
		Files:     []catalog.ModelFile{file},
	}, nil
}

func materializeReal(ctx context.Context, tmpDir string, options Options, source catalog.ModelSource, logger *slog.Logger) (completionManifest, error) {
	client := options.HTTPClient
	if client == nil {
		client = secureHTTPClient()
	}
	files := make([]catalog.ModelFile, 0, len(source.Files))
	for _, expected := range source.Files {
		logger.InfoContext(ctx, "downloading model file", "path", expected.Path, "sizeBytes", expected.SizeBytes)
		if err := downloadFile(ctx, client, source, expected, tmpDir); err != nil {
			return completionManifest{}, err
		}
		files = append(files, expected)
	}
	return completionManifest{
		CacheHash: options.CacheHash,
		Digest:    options.ExpectedDigest,
		SizeBytes: options.ExpectedSize,
		Mode:      "real",
		ModelID:   options.ModelID,
		Revision:  options.Revision,
		Files:     files,
	}, nil
}

func secureHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if request.URL.Scheme != "https" {
				return fmt.Errorf("refusing non-HTTPS redirect")
			}
			return nil
		},
	}
}

func downloadFile(ctx context.Context, client *http.Client, source catalog.ModelSource, expected catalog.ModelFile, root string) error {
	downloadURL, err := modelFileURL(source, expected.Path)
	if err != nil {
		return err
	}
	destination, err := modelFilePath(root, expected.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create model file directory: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("create model download request: %w", err)
	}
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "ember-prefetch/1")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download model file %q: %w", expected.Path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("download model file %q: unexpected HTTP status %s", expected.Path, response.Status)
	}
	if response.ContentLength >= 0 && response.ContentLength != expected.SizeBytes {
		return fmt.Errorf("download model file %q: content length %d does not match expected %d", expected.Path, response.ContentLength, expected.SizeBytes)
	}

	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create model file %q: %w", expected.Path, err)
	}
	hasher := sha256.New()
	written, copyErr := io.CopyBuffer(io.MultiWriter(file, hasher), io.LimitReader(response.Body, expected.SizeBytes+1), make([]byte, 1<<20))
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("write model file %q: %w", expected.Path, copyErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync model file %q: %w", expected.Path, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close model file %q: %w", expected.Path, closeErr)
	}
	if written != expected.SizeBytes {
		return fmt.Errorf("download model file %q: wrote %d bytes, expected %d", expected.Path, written, expected.SizeBytes)
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if digest != expected.Digest {
		return fmt.Errorf("download model file %q: digest %s does not match expected %s", expected.Path, digest, expected.Digest)
	}
	if expected.Safetensors {
		if err := cacheartifact.ValidateSafetensorsFile(destination); err != nil {
			return fmt.Errorf("validate safetensors file %q: %w", expected.Path, err)
		}
	}
	return nil
}

func modelFileURL(source catalog.ModelSource, filePath string) (string, error) {
	baseURL, err := url.Parse(source.BaseURL)
	if err != nil {
		return "", fmt.Errorf("parse source base URL: %w", err)
	}
	baseURL.Path = path.Join(baseURL.Path, source.Repository, "resolve", source.Revision, filePath)
	baseURL.RawPath = ""
	return baseURL.String(), nil
}

func modelFilePath(root, relativePath string) (string, error) {
	if err := validateModelFile(catalog.ModelFile{Path: relativePath, Digest: "sha256:" + strings.Repeat("0", 64), SizeBytes: 1}); err != nil {
		return "", err
	}
	candidate := filepath.Join(root, filepath.FromSlash(relativePath))
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("model file path escapes cache directory")
	}
	return candidate, nil
}

func verifyIfPresent(finalDir, cacheHash, expectedDigest string, expectedSize int64) error {
	if _, err := os.Lstat(finalDir); err != nil {
		return err
	}
	return verifyDirectory(finalDir, cacheHash, expectedDigest, expectedSize)
}

func verifyDirectory(finalDir, cacheHash, expectedDigest string, expectedSize int64) error {
	info, err := os.Lstat(finalDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fs.ErrNotExist
		}
		return fmt.Errorf("stat cache directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cache path is not a real directory")
	}
	manifest, err := readManifest(filepath.Join(finalDir, cacheartifact.ManifestFileName))
	if err != nil {
		return err
	}
	if manifest.CacheHash != cacheHash {
		return fmt.Errorf("manifest cache hash mismatch: got %s want %s", manifest.CacheHash, cacheHash)
	}
	if manifest.Digest != expectedDigest {
		return fmt.Errorf("manifest digest mismatch: got %s want %s", manifest.Digest, expectedDigest)
	}
	if expectedSize > 0 && manifest.SizeBytes != expectedSize {
		return fmt.Errorf("manifest size mismatch: got %d want %d", manifest.SizeBytes, expectedSize)
	}
	if len(manifest.Files) == 0 {
		return fmt.Errorf("manifest contains no files")
	}
	seen := make(map[string]catalog.ModelFile, len(manifest.Files))
	for _, file := range manifest.Files {
		if err := validateModelFile(file); err != nil {
			return fmt.Errorf("invalid completion manifest: %w", err)
		}
		if _, ok := seen[file.Path]; ok {
			return fmt.Errorf("manifest contains duplicate file %q", file.Path)
		}
		seen[file.Path] = file
	}
	if got := catalog.ModelFilesSize(manifest.Files); got != manifest.SizeBytes {
		return fmt.Errorf("manifest file sizes total %d, expected %d", got, manifest.SizeBytes)
	}
	switch manifest.Mode {
	case "synthetic":
		if len(manifest.Files) != 1 || manifest.Files[0].Path != cacheartifact.ArtifactFileName {
			return fmt.Errorf("synthetic manifest must contain only %s", cacheartifact.ArtifactFileName)
		}
		if manifest.Files[0].Digest != manifest.Digest || manifest.Files[0].SizeBytes != manifest.SizeBytes {
			return fmt.Errorf("synthetic manifest artifact metadata is inconsistent")
		}
	case "real":
		if manifest.ModelID == "" || manifest.Revision == "" {
			return fmt.Errorf("real manifest requires model ID and revision")
		}
		if got := catalog.ModelFilesDigest(manifest.Files); got != manifest.Digest {
			return fmt.Errorf("real manifest digest mismatch: got %s want %s", got, manifest.Digest)
		}
	default:
		return fmt.Errorf("unsupported manifest mode %q", manifest.Mode)
	}
	if err := verifyDirectoryEntries(finalDir, seen); err != nil {
		return err
	}
	for _, file := range manifest.Files {
		if err := verifyModelFile(finalDir, file); err != nil {
			return err
		}
	}
	return nil
}

func readManifest(manifestPath string) (completionManifest, error) {
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return completionManifest{}, fmt.Errorf("stat manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return completionManifest{}, fmt.Errorf("completion manifest is not a regular file")
	}
	if info.Size() > maxManifestSize {
		return completionManifest{}, fmt.Errorf("completion manifest exceeds %d-byte safety limit", maxManifestSize)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return completionManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest completionManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return completionManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return completionManifest{}, err
	}
	return manifest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode manifest: trailing JSON value")
		}
		return fmt.Errorf("decode manifest: %w", err)
	}
	return nil
}

func verifyDirectoryEntries(root string, expectedFiles map[string]catalog.ModelFile) error {
	expectedDirs := map[string]struct{}{".": {}}
	for filePath := range expectedFiles {
		for directory := path.Dir(filePath); directory != "."; directory = path.Dir(directory) {
			expectedDirs[directory] = struct{}{}
		}
	}
	found := make(map[string]struct{}, len(expectedFiles))
	err := filepath.WalkDir(root, func(currentPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, currentPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("cache entry %q must not be a symlink", rel)
		}
		if entry.IsDir() {
			if _, ok := expectedDirs[rel]; !ok {
				return fmt.Errorf("unexpected cache directory %q", rel)
			}
			return nil
		}
		if rel == cacheartifact.ManifestFileName {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("cache entry %q is not a regular file", rel)
		}
		if _, ok := expectedFiles[rel]; !ok {
			return fmt.Errorf("unexpected cache file %q", rel)
		}
		found[rel] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("inspect cache directory: %w", err)
	}
	for filePath := range expectedFiles {
		if _, ok := found[filePath]; !ok {
			return fmt.Errorf("cache file %q is missing", filePath)
		}
	}
	return nil
}

func verifyModelFile(root string, expected catalog.ModelFile) error {
	filePath, err := modelFilePath(root, expected.Path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(filePath)
	if err != nil {
		return fmt.Errorf("stat model file %q: %w", expected.Path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("model file %q is not a regular file", expected.Path)
	}
	if info.Size() != expected.SizeBytes {
		return fmt.Errorf("model file %q size mismatch: got %d want %d", expected.Path, info.Size(), expected.SizeBytes)
	}
	if expected.Safetensors {
		if err := cacheartifact.ValidateSafetensorsFile(filePath); err != nil {
			return fmt.Errorf("validate safetensors file %q: %w", expected.Path, err)
		}
	}
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open model file %q: %w", expected.Path, err)
	}
	hasher := sha256.New()
	_, copyErr := io.CopyBuffer(hasher, file, make([]byte, 1<<20))
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("hash model file %q: %w", expected.Path, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close model file %q: %w", expected.Path, closeErr)
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if digest != expected.Digest {
		return fmt.Errorf("model file %q digest mismatch: got %s want %s", expected.Path, digest, expected.Digest)
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

func removeManagedDir(root, target, expectedBase string) error {
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat managed cache path: %w", err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if filepath.Dir(rel) != "." || filepath.Base(rel) != expectedBase {
		return fmt.Errorf("refusing to remove unexpected cache path %q", target)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove cache path %q because it is not a real directory", target)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove cache directory: %w", err)
	}
	return nil
}

func writeFileAndSync(filePath string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", filePath, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", filePath, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync %s: %w", filePath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filePath, err)
	}
	return nil
}

func syncDirectories(root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(currentPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, currentPath)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("list cache directories for sync: %w", err)
	}
	sort.Slice(directories, func(i, j int) bool {
		return len(directories[i]) > len(directories[j])
	})
	for _, directory := range directories {
		if err := syncPath(directory); err != nil {
			return err
		}
	}
	return nil
}

func syncPath(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open path for sync %s: %w", path, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync path %s: %w", path, err)
	}
	return nil
}

func cloneSource(source catalog.ModelSource) catalog.ModelSource {
	source.Files = append([]catalog.ModelFile(nil), source.Files...)
	return source
}
