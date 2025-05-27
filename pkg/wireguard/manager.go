package wireguard

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

type Manager struct {
	configDir string
}

func New() *Manager {
	return &Manager{
		configDir: "/data/wireguard",
	}
}

func (m *Manager) PrepareUSB(usbMountPath string) error {
	wgDir := filepath.Join(usbMountPath, "wireguard")
	if err := os.MkdirAll(wgDir, 0755); err != nil {
		return fmt.Errorf("failed to create wireguard directory: %w", err)
	}
	log.Println("Created wireguard directory on USB drive")
	return nil
}

func (m *Manager) CopyToUSB(usbMountPath string) error {
	// Ensure config directory exists
	if _, err := os.Stat(m.configDir); os.IsNotExist(err) {
		log.Printf("WireGuard config directory %s does not exist, skipping", m.configDir)
		return nil
	}

	destDir := filepath.Join(usbMountPath, "wireguard")
	
	// Read all .conf files
	entries, err := os.ReadDir(m.configDir)
	if err != nil {
		return fmt.Errorf("failed to read wireguard directory: %w", err)
	}

	copiedCount := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}

		srcPath := filepath.Join(m.configDir, entry.Name())
		destPath := filepath.Join(destDir, entry.Name())

		input, err := os.ReadFile(srcPath)
		if err != nil {
			log.Printf("Failed to read %s: %v", srcPath, err)
			continue
		}

		if err := os.WriteFile(destPath, input, 0644); err != nil {
			log.Printf("Failed to write %s: %v", destPath, err)
			continue
		}

		copiedCount++
		log.Printf("Copied WireGuard config: %s", entry.Name())
	}

	if copiedCount > 0 {
		log.Printf("Copied %d WireGuard config file(s) to USB drive", copiedCount)
	} else {
		log.Println("No WireGuard config files found to copy")
	}

	return nil
}

func (m *Manager) SyncFromUSB(usbMountPath string) (bool, error) {
	srcDir := filepath.Join(usbMountPath, "wireguard")
	
	// Check if USB wireguard directory exists
	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		log.Printf("No wireguard directory found on USB drive")
		return false, nil
	}

	// Ensure local config directory exists
	if err := os.MkdirAll(m.configDir, 0755); err != nil {
		return false, fmt.Errorf("failed to create wireguard config directory: %w", err)
	}

	// Track changes
	changed := false

	// Get list of existing files
	existingFiles := make(map[string]bool)
	if entries, err := os.ReadDir(m.configDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".conf") {
				existingFiles[entry.Name()] = true
			}
		}
	}

	// Read files from USB
	usbEntries, err := os.ReadDir(srcDir)
	if err != nil {
		return false, fmt.Errorf("failed to read USB wireguard directory: %w", err)
	}

	// Process files from USB
	processedFiles := make(map[string]bool)
	for _, entry := range usbEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}

		filename := entry.Name()
		processedFiles[filename] = true

		srcPath := filepath.Join(srcDir, filename)
		destPath := filepath.Join(m.configDir, filename)

		// Read the file content
		input, err := os.ReadFile(srcPath)
		if err != nil {
			log.Printf("Failed to read %s: %v", srcPath, err)
			continue
		}

		// Check if file exists and has different content
		needUpdate := true
		if existing, err := os.ReadFile(destPath); err == nil {
			needUpdate = string(existing) != string(input)
		}

		if needUpdate {
			if err := os.WriteFile(destPath, input, 0644); err != nil {
				log.Printf("Failed to write %s: %v", destPath, err)
				continue
			}
			changed = true
			log.Printf("Updated WireGuard config: %s", filename)
		}
	}

	// Remove files that don't exist on USB
	for filename := range existingFiles {
		if !processedFiles[filename] {
			filePath := filepath.Join(m.configDir, filename)
			if err := os.Remove(filePath); err != nil {
				log.Printf("Failed to remove %s: %v", filePath, err)
			} else {
				changed = true
				log.Printf("Removed WireGuard config: %s", filename)
			}
		}
	}

	// If no files on USB, remove all local configs
	if len(processedFiles) == 0 && len(existingFiles) > 0 {
		for filename := range existingFiles {
			filePath := filepath.Join(m.configDir, filename)
			if err := os.Remove(filePath); err != nil {
				log.Printf("Failed to remove %s: %v", filePath, err)
			} else {
				changed = true
				log.Printf("Removed WireGuard config: %s", filename)
			}
		}
	}

	if changed {
		log.Println("WireGuard configs changed")
	} else {
		log.Println("No WireGuard config changes detected")
	}

	return changed, nil
}