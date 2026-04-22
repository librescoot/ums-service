package settings

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Loader struct {
	settingsFile string
}

func New() *Loader {
	return &Loader{
		settingsFile: "/data/settings.toml",
	}
}

func (l *Loader) CopyToUSB(ctx context.Context, usbMountPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(l.settingsFile); os.IsNotExist(err) {
		log.Printf("Settings file %s does not exist, skipping", l.settingsFile)
		return nil
	}

	destPath := filepath.Join(usbMountPath, "settings.toml")

	input, err := os.ReadFile(l.settingsFile)
	if err != nil {
		return fmt.Errorf("failed to read settings file: %w", err)
	}

	if err := os.WriteFile(destPath, input, 0644); err != nil {
		return fmt.Errorf("failed to write settings to USB: %w", err)
	}

	log.Printf("Copied settings.toml to USB drive")
	return nil
}

func (l *Loader) CopyFromUSB(ctx context.Context, usbMountPath string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	srcPath := filepath.Join(usbMountPath, "settings.toml")

	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		log.Printf("No settings.toml found on USB drive")
		return false, nil
	}

	input, err := os.ReadFile(srcPath)
	if err != nil {
		return false, fmt.Errorf("failed to read settings from USB: %w", err)
	}

	var dummy map[string]interface{}
	if err := toml.Unmarshal(input, &dummy); err != nil {
		log.Printf("Invalid TOML in settings.toml on USB drive: %v — skipping", err)
		return false, nil
	}

	// Check if content changed
	changed := true
	if existing, err := os.ReadFile(l.settingsFile); err == nil {
		changed = string(existing) != string(input)
	}

	if changed {
		if err := os.WriteFile(l.settingsFile, input, 0644); err != nil {
			return false, fmt.Errorf("failed to write settings file: %w", err)
		}
		log.Printf("Updated settings.toml from USB drive")
	} else {
		log.Printf("settings.toml unchanged")
	}

	return changed, nil
}
