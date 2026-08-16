package cachefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareRootMakesDirectoryGroupWritable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "models")
	if err := prepareRoot(root, os.Getgid()); err != nil {
		t.Fatalf("prepare root: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o770 {
		t.Fatalf("expected mode 0770, got %#o", got)
	}
}

func TestPrepareRootRejectsRelativePath(t *testing.T) {
	if err := prepareRoot("relative/models", os.Getgid()); err == nil {
		t.Fatal("expected relative path rejection")
	}
}
