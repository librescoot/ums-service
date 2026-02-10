package update

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/librescoot/ums-service/pkg/dbc"
	"github.com/librescoot/ums-service/pkg/redis"
)

type Loader struct {
	otaDir         string
	dbcOtaDir      string
	redisPublisher *redis.Publisher
	dbcInterface   *dbc.Interface
}

func New(dbcInterface *dbc.Interface) *Loader {
	return &Loader{
		otaDir:       "/data/ota",
		dbcOtaDir:    "/data/ota",
		dbcInterface: dbcInterface,
	}
}

func (l *Loader) SetRedisPublisher(pub *redis.Publisher) {
	l.redisPublisher = pub
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

	if l.redisPublisher == nil {
		return fmt.Errorf("redis publisher not configured")
	}

	dstPath := filepath.Join(l.otaDir, filename)

	if err := os.Rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("failed to move update file: %w", err)
	}

	if err := l.redisPublisher.PushMDBUpdate(dstPath); err != nil {
		return fmt.Errorf("failed to notify update service: %w", err)
	}

	log.Printf("Successfully queued MDB update: %s", filename)
	return nil
}

func (l *Loader) processDBCUpdate(srcPath string) error {
	filename := filepath.Base(srcPath)
	log.Printf("Processing DBC update: %s", filename)

	if l.redisPublisher == nil {
		return fmt.Errorf("redis publisher not configured")
	}

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

	if err := l.redisPublisher.PushDBCUpdate(remotePath); err != nil {
		return fmt.Errorf("failed to notify update service: %w", err)
	}

	log.Printf("Successfully queued DBC update: %s", filename)
	return nil
}

func (l *Loader) NeedReboot() bool {
	return false
}
