package cachefs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	RuntimeUserID  int64 = 65532
	RuntimeGroupID int64 = 65532
)

func PrepareRoot(root string) error {
	return prepareRoot(root, int(RuntimeGroupID))
}

func prepareRoot(root string, groupID int) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("cache root is required")
	}
	cleanRoot := filepath.Clean(root)
	if !filepath.IsAbs(cleanRoot) {
		return fmt.Errorf("cache root must be absolute")
	}
	if err := os.MkdirAll(cleanRoot, 0o750); err != nil {
		return fmt.Errorf("create cache root: %w", err)
	}
	info, err := os.Lstat(cleanRoot)
	if err != nil {
		return fmt.Errorf("inspect cache root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("cache root must be a directory, not a symlink")
	}
	if err := os.Chown(cleanRoot, -1, groupID); err != nil {
		return fmt.Errorf("set cache root group: %w", err)
	}
	if err := os.Chmod(cleanRoot, 0o770); err != nil {
		return fmt.Errorf("set cache root permissions: %w", err)
	}
	return nil
}
