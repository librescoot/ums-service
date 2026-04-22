package scripts

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

func (r *Runner) PrepareUSB(ctx context.Context, usbMountPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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

	r.runMDBScript(scriptsDir)
	r.runDBCScript(ctx, dbcTimeout, logger, scriptsDir)

	return nil
}

func (r *Runner) runMDBScript(scriptsDir string) {
	srcPath := filepath.Join(scriptsDir, "mdb.sh")
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return
	}

	log.Println("Running MDB script")

	tmpPath := "/tmp/ums-mdb.sh"
	input, err := os.ReadFile(srcPath)
	if err != nil {
		log.Printf("Failed to read mdb.sh: %v", err)
		return
	}

	if err := os.WriteFile(tmpPath, input, 0755); err != nil {
		log.Printf("Failed to write mdb.sh to temp: %v", err)
		return
	}

	cmd := exec.Command("bash", tmpPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("MDB script failed: %v, output: %s", err, string(output))
		return
	}

	log.Printf("MDB script output: %s", string(output))
}

func (r *Runner) runDBCScript(ctx context.Context, timeout time.Duration, logger *umslog.Logger, scriptsDir string) {
	srcPath := filepath.Join(scriptsDir, "dbc.sh")
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return
	}

	if !r.dbcInterface.IsEnabled() {
		log.Println("DBC interface not enabled, skipping dbc.sh")
		return
	}

	log.Println("Running DBC script")

	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	remotePath := "/tmp/dbc.sh"
	var progress dbc.ProgressFunc
	if logger != nil {
		progress = logger.ProgressCallback("dbc.sh")
		defer logger.ClearProgress()
	}
	if err := r.dbcInterface.TransferFile(opCtx, srcPath, remotePath, progress); err != nil {
		log.Printf("Failed to transfer dbc.sh to DBC: %v", err)
		return
	}

	output, err := r.dbcInterface.RunCommand(opCtx, fmt.Sprintf("bash %s", remotePath))
	if err != nil {
		log.Printf("DBC script failed: %v", err)
		return
	}

	log.Printf("DBC script output: %s", output)
}
