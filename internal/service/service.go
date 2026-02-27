package service

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/ums-service/pkg/config"
	"github.com/librescoot/ums-service/pkg/dbc"
	"github.com/librescoot/ums-service/pkg/disk"
	"github.com/librescoot/ums-service/pkg/maps"
	"github.com/librescoot/ums-service/pkg/settings"
	"github.com/librescoot/ums-service/pkg/update"
	"github.com/librescoot/ums-service/pkg/usb"
	"github.com/librescoot/ums-service/pkg/wireguard"
)

type Service struct {
	config       *config.Config
	watcher      *ipc.HashWatcher
	usbCtrl      *usb.Controller
	diskMgr      *disk.Manager
	dbcInterface *dbc.Interface
	settingsLdr  *settings.Loader
	updateLdr    *update.Loader
	mapsUpdater  *maps.Updater
	wgManager    *wireguard.Manager
	mu           sync.Mutex
	detachCount  int
	umsModeType  string
}

func New(cfg *config.Config) (*Service, error) {
	redisHost, redisPort, err := parseRedisAddr(cfg.RedisAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_ADDR %q: %w", cfg.RedisAddr, err)
	}

	client, err := ipc.New(
		ipc.WithAddress(redisHost),
		ipc.WithPort(redisPort),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Redis client: %w", err)
	}

	usbCtrl := usb.NewController(cfg.USBDriveFile)
	diskMgr := disk.NewManager(cfg.USBDriveFile, cfg.USBDriveSize)

	// Initialize components
	dbcInterface := dbc.New("/data/dbc")
	settingsLdr := settings.New()
	mapsUpdater := maps.New(dbcInterface)
	wgManager := wireguard.New()

	updateLdr := update.New(client, dbcInterface)

	svc := &Service{
		config:       cfg,
		watcher:      client.NewHashWatcher("usb"),
		usbCtrl:      usbCtrl,
		diskMgr:      diskMgr,
		dbcInterface: dbcInterface,
		settingsLdr:  settingsLdr,
		updateLdr:    updateLdr,
		mapsUpdater:  mapsUpdater,
		wgManager:    wgManager,
	}

	svc.watcher.OnField("mode", svc.handleModeChange)

	return svc, nil
}

func parseRedisAddr(addr string) (string, int, error) {
	const defaultPort = 6379

	// REDIS_ADDR can be set as "host" or "host:port".
	host, portStr, err := net.SplitHostPort(addr)
	if err == nil {
		port, convErr := strconv.Atoi(portStr)
		if convErr != nil {
			return "", 0, fmt.Errorf("invalid port %q", portStr)
		}
		return host, port, nil
	}

	// When no port is provided, use default Redis port.
	if strings.Contains(err.Error(), "missing port in address") {
		return addr, defaultPort, nil
	}

	return "", 0, err
}

func (s *Service) Run(ctx context.Context) error {
	log.Println("Starting UMS service...")

	if err := s.diskMgr.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize disk manager: %w", err)
	}

	s.usbCtrl.SetModeChangeCallback(s.onDeviceDetached)
	s.usbCtrl.StartMonitoring()

	go func() {
		<-ctx.Done()
		s.usbCtrl.StopMonitoring()
	}()

	return s.watcher.StartWithSync()
}

func (s *Service) handleModeChange(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	prevMode := s.usbCtrl.GetCurrentMode()
	if prevMode == mode {
		return nil
	}

	switch mode {
	case "ums", "ums-by-dbc":
		return s.switchToUMS(mode)
	case "normal":
		return s.switchToNormal(prevMode)
	default:
		return fmt.Errorf("unknown mode: %s", mode)
	}
}

func (s *Service) switchToUMS(mode string) error {
	// Mount the drive first to prepare files
	if err := s.diskMgr.Mount(); err != nil {
		return fmt.Errorf("failed to mount drive: %w", err)
	}

	mountPoint := s.diskMgr.GetMountPoint()

	// Copy settings to USB
	if err := s.settingsLdr.CopyToUSB(mountPoint); err != nil {
		log.Printf("Error copying settings to USB: %v", err)
	}

	// Prepare update directory
	if err := s.updateLdr.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing update directory: %v", err)
	}

	// Prepare maps directory
	if err := s.mapsUpdater.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing maps directory: %v", err)
	}

	// Prepare WireGuard directory and copy configs
	if err := s.wgManager.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing wireguard directory: %v", err)
	}
	if err := s.wgManager.CopyToUSB(mountPoint); err != nil {
		log.Printf("Error copying wireguard configs to USB: %v", err)
	}

	// Unmount before switching to UMS mode
	if err := s.diskMgr.Unmount(); err != nil {
		return fmt.Errorf("failed to unmount drive: %w", err)
	}

	// Switch USB mode
	if err := s.usbCtrl.SwitchMode("ums"); err != nil {
		return fmt.Errorf("failed to switch to UMS mode: %w", err)
	}

	s.umsModeType = mode
	log.Println("Switched to UMS mode")
	return nil
}

func (s *Service) switchToNormal(prevMode string) error {
	// Switch USB mode first
	if err := s.usbCtrl.SwitchMode("normal"); err != nil {
		return fmt.Errorf("failed to switch to normal mode: %w", err)
	}

	if prevMode != "ums" {
		return nil
	}

	// Mount the drive to process files
	if err := s.diskMgr.Mount(); err != nil {
		return fmt.Errorf("failed to mount drive: %w", err)
	}
	defer s.diskMgr.Unmount()

	ctx := context.Background()
	mountPoint := s.diskMgr.GetMountPoint()

	// Check if we need DBC for any operations
	needDBC := s.checkIfDBCNeeded(mountPoint)

	if needDBC {
		// Enable DBC only if we need to transfer files
		if err := s.dbcInterface.Enable(ctx); err != nil {
			log.Printf("Warning: failed to enable DBC: %v", err)
			// Continue with other operations
		}
	}

	// Process settings
	settingsChanged := false
	if changed, err := s.settingsLdr.CopyFromUSB(mountPoint); err != nil {
		log.Printf("Error processing settings: %v", err)
	} else {
		settingsChanged = changed
	}

	// Process WireGuard configs
	wgChanged := false
	if changed, err := s.wgManager.SyncFromUSB(mountPoint); err != nil {
		log.Printf("Error processing wireguard configs: %v", err)
	} else {
		wgChanged = changed
	}

	// Process system updates
	if err := s.updateLdr.ProcessUpdates(mountPoint); err != nil {
		log.Printf("Error processing updates: %v", err)
	}
	if err := s.mapsUpdater.ProcessMaps(mountPoint); err != nil {
		log.Printf("Error processing maps: %v", err)
	}

	// Restart settings-service once if any config changed
	if settingsChanged || wgChanged {
		log.Println("Configuration changed, restarting settings-service")
		cmd := exec.Command("systemctl", "restart", "settings-service")
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("Failed to restart settings-service: %v, output: %s", err, string(output))
		} else {
			log.Println("Successfully restarted settings-service")
		}
	}

	// Clean the USB drive
	if err := s.diskMgr.CleanDrive(); err != nil {
		log.Printf("Error cleaning USB drive: %v", err)
	}

	// Disable DBC if it was enabled
	if needDBC {
		if err := s.dbcInterface.Disable(); err != nil {
			log.Printf("Warning: failed to disable DBC: %v", err)
		}
	}

	log.Println("Switched to normal mode and processed files")
	log.Println("Update files queued via Redis - update service will handle installation")

	return nil
}

func (s *Service) checkIfDBCNeeded(mountPoint string) bool {
	// Check for DBC updates
	updateDir := filepath.Join(mountPoint, "system-update")
	if entries, err := os.ReadDir(updateDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), "librescoot-dbc") && strings.HasSuffix(entry.Name(), ".mender") {
				log.Println("Found DBC update files, DBC needed")
				return true
			}
		}
	}

	// Check for map files
	mapsDir := filepath.Join(mountPoint, "maps")
	if entries, err := os.ReadDir(mapsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				filename := entry.Name()
				if strings.HasSuffix(filename, ".mbtiles") || strings.HasSuffix(filename, "tiles.tar") {
					log.Println("Found map files, DBC needed")
					return true
				}
			}
		}
	}

	log.Println("No DBC operations needed")
	return false
}

func (s *Service) onDeviceDetached(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	currentMode := s.usbCtrl.GetCurrentMode()
	if currentMode != "ums" {
		return
	}

	s.detachCount++

	if s.umsModeType == "ums" && s.detachCount == 1 {
		log.Println("ums mode: switching to normal after first disconnect")
		if err := s.handleModeChange("normal"); err != nil {
			log.Printf("Error handling device detachment: %v", err)
		}
		s.detachCount = 0
		return
	}

	if s.umsModeType == "ums-by-dbc" {
		if s.detachCount == 1 {
			log.Println("ums-by-dbc mode: first disconnect, staying in UMS")
			return
		}
		if s.detachCount == 2 {
			log.Println("ums-by-dbc mode: second disconnect, switching to normal")
			if err := s.handleModeChange("normal"); err != nil {
				log.Printf("Error handling device detachment: %v", err)
			}
			s.detachCount = 0
			return
		}
	}
}
