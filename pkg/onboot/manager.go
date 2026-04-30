package onboot

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	scriptPath = "/data/onboot.sh"
	usbName    = "onboot.sh"
)

type Manager struct {
	srcPath string
}

func New() *Manager {
	return &Manager{srcPath: scriptPath}
}

func (m *Manager) CopyToUSB(usbMountPath string) error {
	if _, err := os.Stat(m.srcPath); os.IsNotExist(err) {
		log.Printf("onboot: %s does not exist, skipping", m.srcPath)
		return nil
	}

	dest := filepath.Join(usbMountPath, usbName)
	input, err := os.ReadFile(m.srcPath)
	if err != nil {
		return fmt.Errorf("failed to read onboot.sh: %w", err)
	}
	if err := os.WriteFile(dest, input, 0644); err != nil {
		return fmt.Errorf("failed to write onboot.sh to USB: %w", err)
	}
	log.Printf("onboot: copied onboot.sh to USB drive")
	return nil
}

// CopyFromUSB validates a USB-supplied onboot.sh and installs it. Returns true
// if the on-device script changed.
//
// Validation rules: must start with a #! shebang, and must pass `<interp> -n`
// using the interpreter named in the shebang (falling back to /bin/sh -n if
// that interpreter isn't installed). On any validation failure, the existing
// script is left untouched.
func (m *Manager) CopyFromUSB(usbMountPath string) (bool, error) {
	src := filepath.Join(usbMountPath, usbName)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return false, nil
	}

	input, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("failed to read onboot.sh from USB: %w", err)
	}

	if existing, err := os.ReadFile(m.srcPath); err == nil {
		if bytes.Equal(existing, input) {
			log.Printf("onboot: onboot.sh unchanged")
			// Make sure permissions are still correct even if content is unchanged.
			if err := os.Chmod(m.srcPath, 0755); err != nil {
				log.Printf("onboot: failed to chmod existing script: %v", err)
			}
			return false, nil
		}
	}

	if err := validate(input); err != nil {
		log.Printf("onboot: validation failed, leaving existing script untouched: %v", err)
		return false, nil
	}

	if err := os.WriteFile(m.srcPath, input, 0755); err != nil {
		return false, fmt.Errorf("failed to write onboot.sh: %w", err)
	}
	if err := os.Chmod(m.srcPath, 0755); err != nil {
		log.Printf("onboot: chmod failed: %v", err)
	}
	log.Printf("onboot: installed onboot.sh from USB drive")
	return true, nil
}

func validate(content []byte) error {
	if len(content) < 2 || content[0] != '#' || content[1] != '!' {
		return fmt.Errorf("missing shebang")
	}

	interp := parseShebangInterp(content)
	if interp == "" {
		return fmt.Errorf("could not parse shebang interpreter")
	}

	if err := runSyntaxCheck(interp, content); err == nil {
		return nil
	} else {
		log.Printf("onboot: shebang interpreter %q syntax check failed: %v", interp, err)
	}

	if interp != "/bin/sh" {
		if err := runSyntaxCheck("/bin/sh", content); err == nil {
			return nil
		} else {
			return fmt.Errorf("syntax check failed (interp=%s, fallback=/bin/sh): %w", interp, err)
		}
	}
	return fmt.Errorf("syntax check failed")
}

// parseShebangInterp extracts the interpreter path from the first line, e.g.
// "#!/bin/sh -e" → "/bin/sh", "#!/usr/bin/env bash" → "bash" (resolved by exec).
func parseShebangInterp(content []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	if !scanner.Scan() {
		return ""
	}
	line := strings.TrimPrefix(scanner.Text(), "#!")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	// "/usr/bin/env bash" → use bash directly so we can pass -n.
	if filepath.Base(fields[0]) == "env" && len(fields) >= 2 {
		return fields[1]
	}
	return fields[0]
}

func runSyntaxCheck(interp string, content []byte) error {
	cmd := exec.Command(interp, "-n", "/dev/stdin")
	cmd.Stdin = bytes.NewReader(content)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
