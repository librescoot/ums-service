package update

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/ums-service/pkg/dbc"
)

type Loader struct {
	otaDir       string
	dbcOtaDir    string
	client       *ipc.Client
	dbcInterface *dbc.Interface
}

func New(client *ipc.Client, dbcInterface *dbc.Interface) *Loader {
	return &Loader{
		otaDir:       "/data/ota",
		dbcOtaDir:    "/data/ota",
		client:       client,
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
		if !strings.HasPrefix(filename, "librescoot-") || (!strings.HasSuffix(filename, ".mender") && !strings.HasSuffix(filename, ".delta")) {
			continue
		}

		srcPath := filepath.Join(updateDir, filename)

		if strings.Contains(filename, "librescoot-mdb") {
			if err := l.processMDBUpdate(srcPath); err != nil {
				return fmt.Errorf("failed to process MDB update: %w", err)
			}
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

	if err := os.MkdirAll(l.otaDir, 0755); err != nil {
		return fmt.Errorf("failed to create OTA directory: %w", err)
	}

	dstPath := filepath.Join(l.otaDir, filename)

	// Copy instead of rename — source is on vfat, destination on ext4
	if err := copyFile(srcPath, dstPath); err != nil {
		return fmt.Errorf("failed to copy update file: %w", err)
	}

	_, err := l.client.LPush("scooter:update:mdb", fmt.Sprintf("update-from-file:%s", dstPath))
	if err != nil {
		return fmt.Errorf("failed to notify update service: %w", err)
	}

	log.Printf("Successfully queued MDB update: %s", filename)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	return out.Sync()
}

func (l *Loader) processDBCUpdate(srcPath string) error {
	filename := filepath.Base(srcPath)
	log.Printf("Processing DBC update: %s", filename)

	if !l.dbcInterface.IsEnabled() {
		return fmt.Errorf("DBC interface not enabled for update")
	}

	remotePath := filepath.Join(l.dbcOtaDir, filename)

	if _, err := l.dbcInterface.RunCommand(fmt.Sprintf("mkdir -p %s", l.dbcOtaDir)); err != nil {
		return fmt.Errorf("failed to create remote OTA directory: %w", err)
	}

	if err := l.dbcInterface.CopyFile(srcPath, remotePath); err != nil {
		return fmt.Errorf("failed to copy update to DBC: %w", err)
	}

	log.Printf("Copied DBC update to %s", remotePath)

	_, err := l.client.LPush("scooter:update:dbc", fmt.Sprintf("update-from-file:%s", remotePath))
	if err != nil {
		return fmt.Errorf("failed to notify update service: %w", err)
	}

	log.Printf("Successfully queued DBC update: %s", filename)
	return nil
}

