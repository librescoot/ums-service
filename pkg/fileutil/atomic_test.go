package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(path, []byte("new"), 0o640); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Errorf("contents = %q, want new", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".state.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temporary files remain: %v", matches)
	}
}

func TestCopyFileAtomicReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "destination")
	if err := os.WriteFile(src, []byte("source contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CopyFileAtomic(src, dst, 0o644); err != nil {
		t.Fatalf("CopyFileAtomic: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "source contents" {
		t.Errorf("contents = %q", data)
	}
}
