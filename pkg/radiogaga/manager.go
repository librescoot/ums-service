package radiogaga

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const (
	configDir  = "/data/radio-gaga"
	configFile = "config.yaml"
	usbSubdir  = "radio-gaga"
)

type Manager struct {
	srcPath string
	dirName string
}

func New() *Manager {
	return &Manager{
		srcPath: filepath.Join(configDir, configFile),
		dirName: usbSubdir,
	}
}

func (m *Manager) PrepareUSB(usbMountPath string) error {
	dest := filepath.Join(usbMountPath, m.dirName)
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("failed to create radio-gaga directory: %w", err)
	}
	return nil
}

func (m *Manager) CopyToUSB(usbMountPath string) error {
	if _, err := os.Stat(m.srcPath); os.IsNotExist(err) {
		log.Printf("radio-gaga: %s does not exist, skipping", m.srcPath)
		return nil
	}

	dest := filepath.Join(usbMountPath, m.dirName, configFile)
	input, err := os.ReadFile(m.srcPath)
	if err != nil {
		return fmt.Errorf("failed to read radio-gaga config: %w", err)
	}
	if err := os.WriteFile(dest, input, 0644); err != nil {
		return fmt.Errorf("failed to write radio-gaga config to USB: %w", err)
	}
	log.Printf("radio-gaga: copied config.yaml to USB drive")
	return nil
}

// CopyFromUSB returns true if the on-device config changed.
func (m *Manager) CopyFromUSB(usbMountPath string) (bool, error) {
	src := filepath.Join(usbMountPath, m.dirName, configFile)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return false, nil
	}

	input, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("failed to read radio-gaga config from USB: %w", err)
	}

	if existing, err := os.ReadFile(m.srcPath); err == nil {
		if string(existing) == string(input) {
			log.Printf("radio-gaga: config.yaml unchanged")
			return false, nil
		}
	}

	if err := os.MkdirAll(configDir, 0755); err != nil {
		return false, fmt.Errorf("failed to create radio-gaga directory: %w", err)
	}
	if err := os.WriteFile(m.srcPath, input, 0644); err != nil {
		return false, fmt.Errorf("failed to write radio-gaga config: %w", err)
	}
	log.Printf("radio-gaga: updated config.yaml from USB drive")
	return true, nil
}
