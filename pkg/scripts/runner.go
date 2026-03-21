package scripts

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/librescoot/ums-service/pkg/dbc"
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

func (r *Runner) ProcessScripts(usbMountPath string) error {
	scriptsDir := filepath.Join(usbMountPath, "scripts")

	if _, err := os.Stat(scriptsDir); os.IsNotExist(err) {
		return nil
	}

	r.runMDBScript(scriptsDir)
	r.runDBCScript(scriptsDir)

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

func (r *Runner) runDBCScript(scriptsDir string) {
	srcPath := filepath.Join(scriptsDir, "dbc.sh")
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return
	}

	if !r.dbcInterface.IsEnabled() {
		log.Println("DBC interface not enabled, skipping dbc.sh")
		return
	}

	log.Println("Running DBC script")

	remotePath := "/tmp/dbc.sh"
	if err := r.dbcInterface.CopyFile(srcPath, remotePath); err != nil {
		log.Printf("Failed to copy dbc.sh to DBC: %v", err)
		return
	}

	output, err := r.dbcInterface.RunCommand(fmt.Sprintf("bash %s", remotePath))
	if err != nil {
		log.Printf("DBC script failed: %v", err)
		return
	}

	log.Printf("DBC script output: %s", output)
}
