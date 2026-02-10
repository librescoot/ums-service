package update

import (
	"fmt"
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

// SetRedisPublisher is kept for backward compatibility but is now a no-op
// The redis-ipc client is passed directly to New()
func (l *Loader) SetRedisPublisher(_ interface{}) {
	// No-op: redis-ipc client is set via New()
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

	dstPath := filepath.Join(l.otaDir, filename)

	if err := os.Rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("failed to move update file: %w", err)
	}

	_, err := l.client.LPush("scooter:update:mdb", fmt.Sprintf("update-from-file:%s", dstPath))
	if err != nil {
		return fmt.Errorf("failed to notify update service: %w", err)
	}

	log.Printf("Successfully queued MDB update: %s", filename)
	return nil
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

func (l *Loader) NeedReboot() bool {
	return false
}
