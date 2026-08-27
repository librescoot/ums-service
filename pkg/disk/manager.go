package disk

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const tmpSuffix = ".tmp"

// volumeLabel is what the host shows the drive as. FAT labels are 11
// characters, uppercase.
const volumeLabel = "LIBRESCOOT"

type Manager struct {
	driveFile  string
	driveSize  int64
	mountPoint string
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
		if err := os.Remove(tmpFile); err != nil {
			log.Printf("Failed to remove leftover temp drive file %s: %v", tmpFile, err)
		}
	}
}

func (m *Manager) ensureDriveExists() error {
	if _, err := os.Stat(m.driveFile); os.IsNotExist(err) {
		return m.createAndFormatDrive()
	}
	m.ensureLabel()
	return nil
}

// ensureLabel relabels a drive that predates the label in place. Drives already
// in the field are never reformatted, so without this they would show up as
// "NO NAME" forever. Failure is not fatal: an unlabelled drive still works.
func (m *Manager) ensureLabel() {
	current, err := m.readLabel()
	if err != nil {
		log.Printf("Could not read volume label of %s: %v", m.driveFile, err)
		return
	}
	if !shouldSetLabel(current) {
		return
	}
	if err := m.setLabel(volumeLabel); err != nil {
		log.Printf("Could not set volume label on %s: %v", m.driveFile, err)
		return
	}
	log.Printf("Set volume label of %s to %s", m.driveFile, volumeLabel)
}

// shouldSetLabel reports whether an existing label is ours to overwrite. An
// empty or default label means nobody picked it; anything else was chosen from
// the host and is left alone.
func shouldSetLabel(current string) bool {
	current = strings.TrimSpace(current)
	return current == "" || strings.EqualFold(current, "NO NAME")
}

func (m *Manager) readLabel() (string, error) {
	// Read from stdout only: fatlabel puts warnings on stderr.
	output, err := exec.Command("fatlabel", m.driveFile).Output()
	if err != nil {
		return "", fmt.Errorf("fatlabel failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func (m *Manager) setLabel(label string) error {
	output, err := exec.Command("fatlabel", m.driveFile, label).CombinedOutput()
	if err != nil {
		return fmt.Errorf("fatlabel failed: %v, output: %s", err, string(output))
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
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to create drive file: %w", err)
	}

	if err := m.formatDrive(tmpFile); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to format drive: %w", err)
	}

	if err := os.Rename(tmpFile, m.driveFile); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to move drive file into place: %w", err)
	}

	log.Printf("Virtual USB drive created successfully")
	return nil
}

// createDriveFile allocates the backing store sparsely.
//
// This runs before the service answers anything, so its cost is time the
// vehicle spends unresponsive on first boot after an install. Writing a
// gigabyte of zeros took most of a minute on eMMC and wore the flash for a
// file whose contents are then immediately replaced by mkfs; truncate takes
// no measurable time and allocates blocks as they are written.
//
// The gadget's file-backed storage handles a sparse image, and /data has room
// for the drive to fill out. A write that outruns free space fails at the
// point of writing rather than here.
func (m *Manager) createDriveFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := f.Truncate(m.driveSize); err != nil {
		return fmt.Errorf("size %s to %d bytes: %w", path, m.driveSize, err)
	}
	return nil
}

func (m *Manager) formatDrive(path string) error {
	cmd := exec.Command("mkfs.fat", "-F", "32", "-n", volumeLabel, path)
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
		if err := os.Remove(m.driveFile); err != nil {
			log.Printf("Failed to remove corrupted drive file %s: %v", m.driveFile, err)
		}
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

	if err := os.RemoveAll(m.mountPoint); err != nil {
		log.Printf("Failed to remove mount point %s: %v", m.mountPoint, err)
	}
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
