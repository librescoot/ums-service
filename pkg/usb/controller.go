package usb

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	udcStatePath = "/sys/class/udc/ci_hdrc.0/state"

	// UDC states
	udcStateConfigured = "configured"
)

type Controller struct {
	currentMode     string
	mu              sync.Mutex
	driveFile       string
	stopMonitor     chan struct{}
	monitorRunning  bool
	detachCh        chan struct{}
	monitorInterval time.Duration
}

func NewController(driveFile string) *Controller {
	return &Controller{
		currentMode:     "normal",
		driveFile:       driveFile,
		stopMonitor:     make(chan struct{}),
		detachCh:        make(chan struct{}, 1),
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

// DetachCh returns a channel that receives a signal when a USB detach is
// detected while in UMS mode. The service layer reads from this channel
// to avoid the deadlock that occurred when the old callback called
// handleModeChange directly while holding the service mutex.
func (c *Controller) DetachCh() <-chan struct{} {
	return c.detachCh
}

func (c *Controller) StartMonitoring() {
	c.mu.Lock()
	if c.monitorRunning {
		c.mu.Unlock()
		return
	}
	c.monitorRunning = true
	c.stopMonitor = make(chan struct{})
	c.mu.Unlock()

	go c.monitorLoop()
}

func (c *Controller) StopMonitoring() {
	c.mu.Lock()
	if !c.monitorRunning {
		c.mu.Unlock()
		return
	}
	c.monitorRunning = false
	close(c.stopMonitor)
	c.mu.Unlock()
}

// monitorLoop polls UDC state to detect when the USB host disconnects.
// It watches for transitions from "configured" (host present) to any other
// state while in UMS mode, and signals via detachCh.
func (c *Controller) monitorLoop() {
	ticker := time.NewTicker(c.monitorInterval)
	defer ticker.Stop()

	wasConfigured := false

	for {
		select {
		case <-c.stopMonitor:
			log.Println("USB monitoring stopped")
			return
		case <-ticker.C:
			if c.GetCurrentMode() != "ums" {
				wasConfigured = false
				continue
			}

			configured := c.isHostConnected()

			if configured {
				wasConfigured = true
				continue
			}

			// Transition: configured → not configured = detach event
			if wasConfigured {
				wasConfigured = false
				log.Println("USB host disconnected (UDC state left configured)")
				select {
				case c.detachCh <- struct{}{}:
				default:
				}
			}
		}
	}
}

// isHostConnected checks whether a USB host is actively connected by
// reading the UDC state from sysfs. The "configured" state means the
// host has completed enumeration and is using the gadget.
func (c *Controller) isHostConnected() bool {
	data, err := os.ReadFile(udcStatePath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == udcStateConfigured
}
