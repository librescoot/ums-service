package disk

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

const tmpSuffix = ".tmp"

type Manager struct {
	driveFile   string
	driveSize   int64
	mountPoint  string
}

func NewManager(driveFile string, driveSize int64) *Manager {
	return &Manager{
		driveFile:  driveFile,
		driveSize:  driveSize,
		mountPoint: "/mnt/usb-drive-temp",
	}
}

func (m *Manager) Initialize() error {
	m.cleanupTempFile()

	if err := m.ensureDriveExists(); err != nil {
		return fmt.Errorf("failed to ensure drive exists: %w", err)
	}
	return nil
}

func (m *Manager) cleanupTempFile() {
	tmpFile := m.driveFile + tmpSuffix
	if _, err := os.Stat(tmpFile); err == nil {
		log.Printf("Removing leftover temp drive file %s", tmpFile)
		os.Remove(tmpFile)
	}
}

func (m *Manager) ensureDriveExists() error {
	if _, err := os.Stat(m.driveFile); os.IsNotExist(err) {
		return m.createAndFormatDrive()
	}
	return nil
}

func (m *Manager) createAndFormatDrive() error {
	log.Printf("Creating virtual USB drive at %s", m.driveFile)
	tmpFile := m.driveFile + tmpSuffix

	if err := os.MkdirAll(filepath.Dir(m.driveFile), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	if err := m.createDriveFile(tmpFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to create drive file: %w", err)
	}

	if err := m.formatDrive(tmpFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to format drive: %w", err)
	}

	if err := os.Rename(tmpFile, m.driveFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to move drive file into place: %w", err)
	}

	log.Printf("Virtual USB drive created successfully")
	return nil
}

func (m *Manager) createDriveFile(path string) error {
	cmd := exec.Command("dd", "if=/dev/zero", fmt.Sprintf("of=%s", path),
		"bs=1M", fmt.Sprintf("count=%d", m.driveSize/(1024*1024)))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dd failed: %v, output: %s", err, string(output))
	}
	return nil
}

func (m *Manager) formatDrive(path string) error {
	cmd := exec.Command("mkfs.fat", "-F", "32", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.fat failed: %v, output: %s", err, string(output))
	}
	return nil
}

func (m *Manager) checkFilesystem() error {
	cmd := exec.Command("fsck.fat", "-n", m.driveFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fsck.fat failed: %v, output: %s", err, string(output))
	}
	return nil
}

func (m *Manager) Mount() error {
	if err := m.checkFilesystem(); err != nil {
		log.Printf("Filesystem check failed: %v — recreating drive", err)
		os.Remove(m.driveFile)
		if err := m.createAndFormatDrive(); err != nil {
			return fmt.Errorf("failed to recreate drive after corruption: %w", err)
		}
	}

	if err := os.MkdirAll(m.mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	if err := m.mountDrive(m.mountPoint); err != nil {
		return fmt.Errorf("failed to mount drive: %w", err)
	}

	log.Printf("Mounted USB drive at %s", m.mountPoint)
	return nil
}

func (m *Manager) Unmount() error {
	if err := m.unmountDrive(m.mountPoint); err != nil {
		return fmt.Errorf("failed to unmount drive: %w", err)
	}

	os.RemoveAll(m.mountPoint)
	log.Println("Unmounted USB drive")
	return nil
}

func (m *Manager) GetMountPoint() string {
	return m.mountPoint
}

func (m *Manager) CleanDrive() error {
	log.Println("Cleaning USB drive")

	if err := m.cleanDrive(m.mountPoint); err != nil {
		return fmt.Errorf("failed to clean drive: %w", err)
	}

	log.Println("Successfully cleaned USB drive")
	return nil
}

func (m *Manager) mountDrive(mountPoint string) error {
	cmd := exec.Command("mount", "-t", "vfat", m.driveFile, mountPoint)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount failed: %v, output: %s", err, string(output))
	}
	return nil
}

func (m *Manager) unmountDrive(mountPoint string) error {
	cmd := exec.Command("umount", mountPoint)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount failed: %v, output: %s", err, string(output))
	}
	return nil
}

func (m *Manager) cleanDrive(mountPoint string) error {
	cmds := [][]string{
		{"find", mountPoint, "-mindepth", "1", "-type", "f", "-not", "-name", "ums_log.txt", "-delete"},
		{"find", mountPoint, "-mindepth", "1", "-type", "d", "-empty", "-delete"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("clean failed: %v, output: %s", err, string(output))
		}
	}
	return nil
}