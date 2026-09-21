package uplink

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFromUSBCreatesPrivateConfig(t *testing.T) {
	dataDir := t.TempDir()
	usbDir := t.TempDir()
	manager := &Manager{
		srcPath: filepath.Join(dataDir, "uplink", configFile),
		dirName: usbSubdir,
	}
	usbConfigDir := filepath.Join(usbDir, usbSubdir)
	if err := os.MkdirAll(usbConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usbConfigDir, configFile), []byte("token: secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := manager.CopyFromUSB(context.Background(), usbDir)
	if err != nil {
		t.Fatalf("CopyFromUSB: %v", err)
	}
	if !changed {
		t.Fatal("CopyFromUSB did not report a change")
	}
	info, err := os.Stat(manager.srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o, want 600", info.Mode().Perm())
	}
}
