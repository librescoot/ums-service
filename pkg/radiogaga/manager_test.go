package radiogaga

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFromUSBRepairsMatchingConfigPermissions(t *testing.T) {
	dataDir := t.TempDir()
	usbDir := t.TempDir()
	contents := []byte("token: secret\n")
	manager := &Manager{
		srcPath: filepath.Join(dataDir, "radio-gaga", configFile),
		dirName: usbSubdir,
	}
	if err := os.MkdirAll(filepath.Dir(manager.srcPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.srcPath, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	usbConfigDir := filepath.Join(usbDir, usbSubdir)
	if err := os.MkdirAll(usbConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usbConfigDir, configFile), contents, 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := manager.CopyFromUSB(context.Background(), usbDir)
	if err != nil {
		t.Fatalf("CopyFromUSB: %v", err)
	}
	if !changed {
		t.Fatal("CopyFromUSB did not report the permission repair")
	}
	info, err := os.Stat(manager.srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o, want 600", info.Mode().Perm())
	}
}
