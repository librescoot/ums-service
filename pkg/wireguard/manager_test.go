package wireguard

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncFromUSBCreatesPrivateConfig(t *testing.T) {
	dataDir := t.TempDir()
	usbDir := t.TempDir()
	manager := &Manager{configDir: filepath.Join(dataDir, "wireguard")}
	usbConfigDir := filepath.Join(usbDir, "wireguard")
	if err := os.MkdirAll(usbConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const filename = "vpn.conf"
	if err := os.WriteFile(filepath.Join(usbConfigDir, filename), []byte("PrivateKey = secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := manager.SyncFromUSB(context.Background(), usbDir)
	if err != nil {
		t.Fatalf("SyncFromUSB: %v", err)
	}
	if !changed {
		t.Fatal("SyncFromUSB did not report a change")
	}
	info, err := os.Stat(filepath.Join(manager.configDir, filename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o, want 600", info.Mode().Perm())
	}
}
