package rpm

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/librescoot/ums-service/pkg/dbc"
	"github.com/librescoot/ums-service/pkg/umslog"
)

type Installer struct {
	dbcInterface *dbc.Interface
}

func New(dbcInterface *dbc.Interface) *Installer {
	return &Installer{
		dbcInterface: dbcInterface,
	}
}

func (i *Installer) PrepareUSB(ctx context.Context, usbMountPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mdbDir := filepath.Join(usbMountPath, "rpms", "mdb")
	if err := os.MkdirAll(mdbDir, 0755); err != nil {
		return fmt.Errorf("failed to create rpms/mdb directory: %w", err)
	}

	dbcDir := filepath.Join(usbMountPath, "rpms", "dbc")
	if err := os.MkdirAll(dbcDir, 0755); err != nil {
		return fmt.Errorf("failed to create rpms/dbc directory: %w", err)
	}

	log.Println("Created rpms directories on USB drive")
	return nil
}

func (i *Installer) ProcessRPMs(ctx context.Context, dbcTimeout time.Duration, logger *umslog.Logger, usbMountPath string) error {
	if err := i.processMDBRPMs(usbMountPath); err != nil {
		return fmt.Errorf("failed to process MDB RPMs: %w", err)
	}

	if err := i.processDBCRPMs(ctx, dbcTimeout, logger, usbMountPath); err != nil {
		return fmt.Errorf("failed to process DBC RPMs: %w", err)
	}

	return nil
}

func collectRPMs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".rpm") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	return paths
}

func (i *Installer) processMDBRPMs(usbMountPath string) error {
	rpms := collectRPMs(filepath.Join(usbMountPath, "rpms", "mdb"))
	if len(rpms) == 0 {
		return nil
	}

	log.Printf("Installing %d MDB RPM(s)", len(rpms))

	args := append([]string{"-Uvh", "--force"}, rpms...)
	cmd := exec.Command("rpm", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rpm install failed: %v, output: %s", err, string(output))
	}

	log.Printf("MDB RPM install output: %s", string(output))
	return nil
}

const dbcRPMDir = "/tmp/ums-rpms"

func (i *Installer) processDBCRPMs(ctx context.Context, timeout time.Duration, logger *umslog.Logger, usbMountPath string) error {
	rpms := collectRPMs(filepath.Join(usbMountPath, "rpms", "dbc"))
	if len(rpms) == 0 {
		return nil
	}

	if !i.dbcInterface.IsEnabled() {
		return fmt.Errorf("DBC interface not enabled for RPM installation")
	}

	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Printf("Installing %d DBC RPM(s)", len(rpms))

	if _, err := i.dbcInterface.RunCommand(opCtx, fmt.Sprintf("mkdir -p %s", dbcRPMDir)); err != nil {
		return fmt.Errorf("failed to create remote RPM directory: %w", err)
	}

	var remoteFiles []string
	for _, localPath := range rpms {
		filename := filepath.Base(localPath)
		remotePath := fmt.Sprintf("%s/%s", dbcRPMDir, filename)

		var progress dbc.ProgressFunc
		if logger != nil {
			progress = logger.ProgressCallback(filename)
		}
		if err := i.dbcInterface.TransferFile(opCtx, localPath, remotePath, progress); err != nil {
			if logger != nil {
				logger.ClearProgress()
			}
			return fmt.Errorf("failed to transfer %s to DBC: %w", filename, err)
		}
		remoteFiles = append(remoteFiles, remotePath)
	}
	if logger != nil {
		logger.ClearProgress()
	}

	installCmd := fmt.Sprintf("rpm -Uvh --force %s", strings.Join(remoteFiles, " "))
	output, err := i.dbcInterface.RunCommand(opCtx, installCmd)
	if err != nil {
		// Clean up even on failure — use a fresh short context in case
		// the outer one is already done.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		i.dbcInterface.RunCommand(cleanupCtx, fmt.Sprintf("rm -rf %s", dbcRPMDir))
		cleanupCancel()
		return fmt.Errorf("DBC rpm install failed: %v", err)
	}

	log.Printf("DBC RPM install output: %s", output)

	if _, err := i.dbcInterface.RunCommand(opCtx, fmt.Sprintf("rm -rf %s", dbcRPMDir)); err != nil {
		log.Printf("Failed to clean up remote RPMs: %v", err)
	}

	return nil
}
