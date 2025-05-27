package update

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/librescoot/ums-service/pkg/dbc"
)

type Loader struct {
	otaDir      string
	dbcOtaDir   string
	needReboot  bool
	dbcInterface *dbc.Interface
}

func New(dbcInterface *dbc.Interface) *Loader {
	return &Loader{
		otaDir:       "/data/ota",
		dbcOtaDir:    "/data/ota",
		needReboot:   false,
		dbcInterface: dbcInterface,
	}
}

func (l *Loader) PrepareUSB(usbMountPath string) error {
	updateDir := filepath.Join(usbMountPath, "system-update")
	if err := os.MkdirAll(updateDir, 0755); err != nil {
		return fmt.Errorf("failed to create system-update directory: %w", err)
	}
	log.Println("Created system-update directory on USB drive")
	return nil
}

func (l *Loader) ProcessUpdates(usbMountPath string) error {
	updateDir := filepath.Join(usbMountPath, "system-update")
	
	entries, err := os.ReadDir(updateDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("No system-update directory found")
			return nil
		}
		return fmt.Errorf("failed to read update directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()
		if !strings.HasPrefix(filename, "librescoot-") || !strings.HasSuffix(filename, ".mender") {
			continue
		}

		srcPath := filepath.Join(updateDir, filename)

		if strings.Contains(filename, "librescoot-mdb") {
			if err := l.processMDBUpdate(srcPath); err != nil {
				return fmt.Errorf("failed to process MDB update: %w", err)
			}
			l.needReboot = true
		} else if strings.Contains(filename, "librescoot-dbc") {
			if err := l.processDBCUpdate(srcPath); err != nil {
				return fmt.Errorf("failed to process DBC update: %w", err)
			}
		}
	}

	return nil
}

func (l *Loader) processMDBUpdate(srcPath string) error {
	filename := filepath.Base(srcPath)
	log.Printf("Processing MDB update: %s", filename)

	// Run mender-update install directly from mount point
	cmd := exec.Command("mender-update", "install", srcPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mender-update install failed: %v, output: %s", err, string(output))
	}

	log.Printf("Successfully installed MDB update: %s", filename)
	return nil
}

func (l *Loader) processDBCUpdate(srcPath string) error {
	if !l.dbcInterface.IsEnabled() {
		return fmt.Errorf("DBC interface not enabled for update")
	}

	filename := filepath.Base(srcPath)
	remotePath := filepath.Join(l.dbcOtaDir, filename)

	// Create remote OTA directory
	if _, err := l.dbcInterface.RunCommand(fmt.Sprintf("mkdir -p %s", l.dbcOtaDir)); err != nil {
		return fmt.Errorf("failed to create remote OTA directory: %w", err)
	}

	// Copy file to DBC
	if err := l.dbcInterface.CopyFile(srcPath, remotePath); err != nil {
		return fmt.Errorf("failed to copy update to DBC: %w", err)
	}

	log.Printf("Copied DBC update to %s", remotePath)

	// Run mender-update install on DBC
	output, err := l.dbcInterface.RunCommand(fmt.Sprintf("mender-update install %s", remotePath))
	if err != nil {
		return fmt.Errorf("mender-update install failed on DBC: %v, output: %s", err, output)
	}

	log.Printf("Successfully installed DBC update: %s", filename)
	return nil
}

func (l *Loader) NeedReboot() bool {
	return l.needReboot
}