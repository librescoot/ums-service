package usb

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Controller struct {
	currentMode     string
	mu              sync.Mutex
	driveFile       string
	stopMonitor     chan struct{}
	monitorRunning  bool
	modeChangeCb    func(mode string)
	monitorInterval time.Duration
}

func NewController(driveFile string) *Controller {
	return &Controller{
		currentMode:     "normal",
		driveFile:       driveFile,
		stopMonitor:     make(chan struct{}),
		monitorInterval: 2 * time.Second,
	}
}

func (c *Controller) SwitchMode(mode string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.currentMode == mode {
		log.Printf("Already in %s mode", mode)
		return nil
	}

	log.Printf("Switching from %s to %s mode", c.currentMode, mode)

	switch mode {
	case "ums":
		if err := c.switchToUMS(); err != nil {
			return fmt.Errorf("failed to switch to UMS mode: %w", err)
		}
	case "normal":
		if err := c.switchToNormal(); err != nil {
			return fmt.Errorf("failed to switch to normal mode: %w", err)
		}
	default:
		return fmt.Errorf("unknown mode: %s", mode)
	}

	c.currentMode = mode
	return nil
}

func (c *Controller) switchToUMS() error {
	if err := c.unloadModule("g_ether"); err != nil {
		log.Printf("Warning: failed to unload g_ether: %v", err)
	}

	if err := c.loadModule("g_mass_storage",
		fmt.Sprintf("file=%s", c.driveFile),
		"removable=1",
		"ro=0",
		"stall=0",
		"iSerialNumber=1234567890"); err != nil {
		return fmt.Errorf("failed to load g_mass_storage: %w", err)
	}

	log.Println("Switched to UMS mode")
	return nil
}

func (c *Controller) switchToNormal() error {
	if err := c.unloadModule("g_mass_storage"); err != nil {
		log.Printf("Warning: failed to unload g_mass_storage: %v", err)
	}

	if err := c.loadModule("g_ether"); err != nil {
		return fmt.Errorf("failed to load g_ether: %w", err)
	}

	log.Println("Switched to normal mode")
	return nil
}

func (c *Controller) loadModule(module string, params ...string) error {
	args := []string{module}
	args = append(args, params...)

	cmd := exec.Command("modprobe", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe %s failed: %v, output: %s", module, err, string(output))
	}
	return nil
}

func (c *Controller) unloadModule(module string) error {
	cmd := exec.Command("rmmod", module)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "not currently loaded") {
			return nil
		}
		return fmt.Errorf("rmmod %s failed: %v, output: %s", module, err, string(output))
	}
	return nil
}

func (c *Controller) GetCurrentMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentMode
}

func (c *Controller) SetModeChangeCallback(cb func(mode string)) {
	c.modeChangeCb = cb
}

func (c *Controller) StartMonitoring() {
	c.mu.Lock()
	if c.monitorRunning {
		c.mu.Unlock()
		return
	}
	c.monitorRunning = true
	c.mu.Unlock()

	go c.monitorLoop()
}

func (c *Controller) StopMonitoring() {
	close(c.stopMonitor)
	c.mu.Lock()
	c.monitorRunning = false
	c.mu.Unlock()
}

func (c *Controller) monitorLoop() {
	ticker := time.NewTicker(c.monitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopMonitor:
			log.Println("USB monitoring stopped")
			return
		case <-ticker.C:
			if !c.isDeviceAttached() && c.GetCurrentMode() == "ums" {
				log.Println("Device detached detected, switching to normal mode")
				if c.modeChangeCb != nil {
					c.modeChangeCb("normal")
				}
				return
			}
		}
	}
}

func (c *Controller) isDeviceAttached() bool {
	usbPath := "/sys/kernel/config/usb_gadget"
	if _, err := os.Stat(usbPath); os.IsNotExist(err) {
		return false
	}

	entries, err := os.ReadDir(usbPath)
	if err != nil {
		return false
	}

	for _, entry := range entries {
		if entry.IsDir() {
			statePath := filepath.Join(usbPath, entry.Name(), "UDC")
			if data, err := os.ReadFile(statePath); err == nil {
				if strings.TrimSpace(string(data)) != "" {
					return true
				}
			}
		}
	}

	return false
}
