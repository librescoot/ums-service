package scripts

import (
	"context"
	"errors"
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

type Runner struct {
	dbcInterface *dbc.Interface
}

func New(dbcInterface *dbc.Interface) *Runner {
	return &Runner{
		dbcInterface: dbcInterface,
	}
}

func (r *Runner) PrepareUSB(usbMountPath string) error {
	scriptsDir := filepath.Join(usbMountPath, "scripts")
	if err := os.MkdirAll(scriptsDir, 0755); err != nil {
		return fmt.Errorf("failed to create scripts directory: %w", err)
	}
	log.Println("Created scripts directory on USB drive")
	return nil
}

func (r *Runner) ProcessScripts(ctx context.Context, dbcTimeout time.Duration, logger *umslog.Logger, usbMountPath string) error {
	scriptsDir := filepath.Join(usbMountPath, "scripts")

	if _, err := os.Stat(scriptsDir); os.IsNotExist(err) {
		return nil
	}

	mdbErr := r.runMDBScript(scriptsDir)
	dbcErr := r.runDBCScript(ctx, dbcTimeout, logger, scriptsDir)
	return errors.Join(mdbErr, dbcErr)
}

func (r *Runner) runMDBScript(scriptsDir string) error {
	srcPath := filepath.Join(scriptsDir, "mdb.sh")
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return nil
	}

	log.Println("Running MDB script")

	tmpPath := "/tmp/ums-mdb.sh"
	input, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read mdb.sh: %w", err)
	}

	if err := os.WriteFile(tmpPath, input, 0755); err != nil {
		return fmt.Errorf("write temporary mdb.sh: %w", err)
	}
	defer os.Remove(tmpPath)

	output, err := exec.Command("bash", tmpPath).CombinedOutput()
	if err != nil {
		return scriptError("MDB script", err, output)
	}

	log.Printf("MDB script output: %s", string(output))
	return nil
}

const maxErrorOutput = 4096

func scriptError(name string, err error, output []byte) error {
	if len(output) > maxErrorOutput {
		output = output[len(output)-maxErrorOutput:]
	}
	return fmt.Errorf("%s failed: %w, output: %s", name, err, strings.TrimSpace(string(output)))
}

func (r *Runner) runDBCScript(ctx context.Context, timeout time.Duration, logger *umslog.Logger, scriptsDir string) error {
	srcPath := filepath.Join(scriptsDir, "dbc.sh")
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return nil
	}

	if !r.dbcInterface.IsEnabled() {
		return fmt.Errorf("DBC interface not enabled for dbc.sh")
	}

	log.Println("Running DBC script")

	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	remotePath := "/tmp/dbc.sh"
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(opCtx), 30*time.Second)
		defer cleanupCancel()
		if _, err := r.dbcInterface.RunCommand(cleanupCtx, "rm -f "+remotePath); err != nil {
			log.Printf("Failed to remove temporary DBC script: %v", err)
		}
	}()

	var progress dbc.ProgressFunc
	if logger != nil {
		progress = logger.ProgressCallback("dbc.sh")
		defer logger.ClearProgress()
	}
	if err := r.dbcInterface.TransferFile(opCtx, srcPath, remotePath, progress); err != nil {
		return fmt.Errorf("transfer dbc.sh: %w", err)
	}

	output, err := r.dbcInterface.RunCommand(opCtx, fmt.Sprintf("bash %s", remotePath))
	if err != nil {
		return scriptError("DBC script", err, []byte(output))
	}

	log.Printf("DBC script output: %s", output)
	return nil
}
