package disk

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

const tmpSuffix = ".tmp"

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

	if err := m.verifyOrRecreate(); err != nil {
		return fmt.Errorf("failed to verify drive: %w", err)
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

// verifyOrRecreate ensures a healthy FAT32 drive exists at driveFile.
// Recreates on missing file, smaller-than-configured size, or fsck
// failure. A larger existing drive is kept to avoid destroying user
// data after a configured-size decrease.
func (m *Manager) verifyOrRecreate() error {
	info, err := os.Stat(m.driveFile)
	if os.IsNotExist(err) {
		log.Printf("Drive file missing, creating %s", m.driveFile)
		return m.createAndFormatDrive()
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", m.driveFile, err)
	}

	if info.Size() < m.driveSize {
		log.Printf("Drive file %s is smaller than configured (%d < %d), recreating",
			m.driveFile, info.Size(), m.driveSize)
		os.Remove(m.driveFile)
		return m.createAndFormatDrive()
	}
	if info.Size() > m.driveSize {
		log.Printf("Drive file %s is larger than configured (%d > %d), keeping",
			m.driveFile, info.Size(), m.driveSize)
	}

	if err := m.checkFilesystem(); err != nil {
		log.Printf("Filesystem check failed: %v — recreating drive", err)
		os.Remove(m.driveFile)
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

// createDriveFile allocates a sparse file of driveSize bytes. mkfs.fat
// writes only FAT metadata so the file stays sparse until real writes
// come through g_mass_storage, making drive creation near-instant.
func (m *Manager) createDriveFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := f.Truncate(m.driveSize); err != nil {
		return fmt.Errorf("truncate %s to %d: %w", path, m.driveSize, err)
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

func (m *Manager) Mount(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(m.mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	if err := m.mountDrive(ctx, m.mountPoint); err != nil {
		return fmt.Errorf("failed to mount drive: %w", err)
	}

	log.Printf("Mounted USB drive at %s", m.mountPoint)
	return nil
}

// Unmount detaches the loopback mount. It always attempts the unmount,
// even if ctx is cancelled, because leaving the mount behind prevents
// the next UMS switch from working. Callers that need to unwind on
// cancellation should still call Unmount to clean up.
func (m *Manager) Unmount(ctx context.Context) error {
	if err := m.unmountDrive(ctx, m.mountPoint); err != nil {
		return fmt.Errorf("failed to unmount drive: %w", err)
	}

	os.RemoveAll(m.mountPoint)
	log.Println("Unmounted USB drive")
	return nil
}

func (m *Manager) GetMountPoint() string {
	return m.mountPoint
}

func (m *Manager) CleanDrive(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	log.Println("Cleaning USB drive")

	if err := m.cleanDrive(ctx, m.mountPoint); err != nil {
		return fmt.Errorf("failed to clean drive: %w", err)
	}

	log.Println("Successfully cleaned USB drive")
	return nil
}

func (m *Manager) mountDrive(ctx context.Context, mountPoint string) error {
	cmd := exec.CommandContext(ctx, "mount", "-t", "vfat", m.driveFile, mountPoint)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount failed: %v, output: %s", err, string(output))
	}
	return nil
}

// unmountDrive runs umount detached from the caller's context. A
// cancelled context must not abort the unmount itself — a stale mount
// blocks the next session.
func (m *Manager) unmountDrive(_ context.Context, mountPoint string) error {
	cmd := exec.Command("umount", mountPoint)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount failed: %v, output: %s", err, string(output))
	}
	return nil
}

func (m *Manager) cleanDrive(ctx context.Context, mountPoint string) error {
	cmds := [][]string{
		{"find", mountPoint, "-mindepth", "1", "-type", "f", "-not", "-name", "ums_log.txt", "-delete"},
		{"find", mountPoint, "-mindepth", "1", "-type", "d", "-empty", "-delete"},
	}
	for _, args := range cmds {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("clean failed: %v, output: %s", err, string(output))
		}
	}
	return nil
}
