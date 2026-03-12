package rpm

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/librescoot/ums-service/pkg/dbc"
)

type Installer struct {
	dbcInterface *dbc.Interface
}

func New(dbcInterface *dbc.Interface) *Installer {
	return &Installer{
		dbcInterface: dbcInterface,
	}
}

func (i *Installer) PrepareUSB(usbMountPath string) error {
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

func (i *Installer) ProcessRPMs(usbMountPath string) error {
	if err := i.processMDBRPMs(usbMountPath); err != nil {
		return fmt.Errorf("failed to process MDB RPMs: %w", err)
	}

	if err := i.processDBCRPMs(usbMountPath); err != nil {
		return fmt.Errorf("failed to process DBC RPMs: %w", err)
	}

	return nil
}

func (i *Installer) processMDBRPMs(usbMountPath string) error {
	mdbDir := filepath.Join(usbMountPath, "rpms", "mdb")

	entries, err := os.ReadDir(mdbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read rpms/mdb directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".rpm") {
			continue
		}

		rpmPath := filepath.Join(mdbDir, entry.Name())
		log.Printf("Installing MDB RPM: %s", entry.Name())

		cmd := exec.Command("rpm", "-Uvh", rpmPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("Failed to install %s: %v, output: %s", entry.Name(), err, string(output))
			continue
		}

		log.Printf("Successfully installed MDB RPM: %s", entry.Name())
	}

	return nil
}

func (i *Installer) processDBCRPMs(usbMountPath string) error {
	dbcDir := filepath.Join(usbMountPath, "rpms", "dbc")

	entries, err := os.ReadDir(dbcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read rpms/dbc directory: %w", err)
	}

	if !i.dbcInterface.IsEnabled() {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".rpm") {
				return fmt.Errorf("DBC interface not enabled for RPM installation")
			}
		}
		return nil
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".rpm") {
			continue
		}

		localPath := filepath.Join(dbcDir, entry.Name())
		remotePath := filepath.Join("/tmp", entry.Name())
		log.Printf("Installing DBC RPM: %s", entry.Name())

		if err := i.dbcInterface.CopyFile(localPath, remotePath); err != nil {
			log.Printf("Failed to copy %s to DBC: %v", entry.Name(), err)
			continue
		}

		output, err := i.dbcInterface.RunCommand(fmt.Sprintf("rpm -Uvh %s", remotePath))
		if err != nil {
			log.Printf("Failed to install %s on DBC: %v", entry.Name(), err)
			continue
		}

		log.Printf("Successfully installed DBC RPM %s: %s", entry.Name(), output)
	}

	return nil
}
