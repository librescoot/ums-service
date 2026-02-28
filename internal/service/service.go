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
	client       *ipc.Client
	watcher      *ipc.HashWatcher
	publisher    *ipc.HashPublisher
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

	dbcInterface := dbc.New("/data/dbc", client)
	settingsLdr := settings.New()
	mapsUpdater := maps.New(dbcInterface)
	wgManager := wireguard.New()

	updateLdr := update.New(client, dbcInterface)

	svc := &Service{
		config:       cfg,
		client:       client,
		watcher:      client.NewHashWatcher("usb"),
		publisher:    client.NewHashPublisher("usb"),
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

	host, portStr, err := net.SplitHostPort(addr)
	if err == nil {
		port, convErr := strconv.Atoi(portStr)
		if convErr != nil {
			return "", 0, fmt.Errorf("invalid port %q", portStr)
		}
		return host, port, nil
	}

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

	s.usbCtrl.StartMonitoring()

	go s.detachLoop(ctx)

	go func() {
		<-ctx.Done()
		s.usbCtrl.StopMonitoring()
	}()

	// StartWithSync is non-blocking: it subscribes to the Redis channel,
	// syncs current hash state, then processes messages in a goroutine.
	if err := s.watcher.StartWithSync(); err != nil {
		return fmt.Errorf("failed to start hash watcher: %w", err)
	}

	log.Println("UMS service running, waiting for mode changes...")
	<-ctx.Done()
	return nil
}

// detachLoop reads USB detach signals from the controller and handles
// the mode transition back to normal. Running in its own goroutine
// ensures the service mutex is acquired cleanly without reentrancy.
func (s *Service) detachLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.usbCtrl.DetachCh():
			s.onDeviceDetached()
		}
	}
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
	s.setStatus("preparing")

	if err := s.diskMgr.Mount(); err != nil {
		s.setStatus("idle")
		return fmt.Errorf("failed to mount drive: %w", err)
	}

	mountPoint := s.diskMgr.GetMountPoint()

	if err := s.settingsLdr.CopyToUSB(mountPoint); err != nil {
		log.Printf("Error copying settings to USB: %v", err)
	}

	if err := s.updateLdr.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing update directory: %v", err)
	}

	if err := s.mapsUpdater.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing maps directory: %v", err)
	}

	if err := s.wgManager.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing wireguard directory: %v", err)
	}
	if err := s.wgManager.CopyToUSB(mountPoint); err != nil {
		log.Printf("Error copying wireguard configs to USB: %v", err)
	}

	if err := s.diskMgr.Unmount(); err != nil {
		s.setStatus("idle")
		return fmt.Errorf("failed to unmount drive: %w", err)
	}

	// Publish status BEFORE switching USB — DBC can still read Redis via g_ether
	s.setStatus("active")
	s.setLEDs(ledsUMSActive)

	if err := s.usbCtrl.SwitchMode("ums"); err != nil {
		s.setStatus("idle")
		s.setLEDs(ledsOff)
		return fmt.Errorf("failed to switch to UMS mode: %w", err)
	}

	s.umsModeType = mode
	s.detachCount = 0
	log.Printf("Switched to UMS mode (type: %s)", mode)
	return nil
}

func (s *Service) switchToNormal(prevMode string) error {
	s.setLEDs(ledsOff)

	if err := s.usbCtrl.SwitchMode("normal"); err != nil {
		return fmt.Errorf("failed to switch to normal mode: %w", err)
	}

	if prevMode != "ums" {
		s.setStatus("idle")
		return nil
	}

	s.setStatus("processing")

	if err := s.diskMgr.Mount(); err != nil {
		s.setStatus("idle")
		return fmt.Errorf("failed to mount drive: %w", err)
	}

	ctx := context.Background()
	mountPoint := s.diskMgr.GetMountPoint()

	needDBC := s.checkIfDBCNeeded(mountPoint)

	if needDBC {
		if err := s.dbcInterface.Enable(ctx); err != nil {
			log.Printf("Warning: failed to enable DBC: %v", err)
		}
	}

	settingsChanged := false
	if changed, err := s.settingsLdr.CopyFromUSB(mountPoint); err != nil {
		log.Printf("Error processing settings: %v", err)
	} else {
		settingsChanged = changed
	}

	wgChanged := false
	if changed, err := s.wgManager.SyncFromUSB(mountPoint); err != nil {
		log.Printf("Error processing wireguard configs: %v", err)
	} else {
		wgChanged = changed
	}

	if err := s.updateLdr.ProcessUpdates(mountPoint); err != nil {
		log.Printf("Error processing updates: %v", err)
	}
	if err := s.mapsUpdater.ProcessMaps(mountPoint); err != nil {
		log.Printf("Error processing maps: %v", err)
	}

	if settingsChanged || wgChanged {
		log.Println("Configuration changed, restarting settings-service")
		cmd := exec.Command("systemctl", "restart", "settings-service")
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("Failed to restart settings-service: %v, output: %s", err, string(output))
		} else {
			log.Println("Successfully restarted settings-service")
		}
	}

	if err := s.diskMgr.CleanDrive(); err != nil {
		log.Printf("Error cleaning USB drive: %v", err)
	}

	if err := s.diskMgr.Unmount(); err != nil {
		log.Printf("Error unmounting USB drive: %v", err)
	}

	if needDBC {
		if err := s.dbcInterface.Disable(); err != nil {
			log.Printf("Warning: failed to disable DBC: %v", err)
		}
	}

	s.umsModeType = ""
	s.setStatus("idle")
	log.Println("Switched to normal mode and processed files")

	return nil
}

func (s *Service) checkIfDBCNeeded(mountPoint string) bool {
	updateDir := filepath.Join(mountPoint, "system-update")
	if entries, err := os.ReadDir(updateDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), "librescoot-dbc") && strings.HasSuffix(entry.Name(), ".mender") {
				log.Println("Found DBC update files, DBC needed")
				return true
			}
		}
	}

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

// onDeviceDetached is called from detachLoop when the USB monitor detects
// that the host has disconnected. It tracks the detach count to support
// the ums-by-dbc mode which requires two disconnects before switching back.
func (s *Service) onDeviceDetached() {
	s.mu.Lock()
	defer s.mu.Unlock()

	currentMode := s.usbCtrl.GetCurrentMode()
	if currentMode != "ums" {
		return
	}

	s.detachCount++
	log.Printf("USB detach #%d detected (mode type: %s)", s.detachCount, s.umsModeType)

	switch s.umsModeType {
	case "ums":
		if s.detachCount >= 1 {
			log.Println("ums mode: switching to normal after disconnect")
			s.doSwitchToNormal()
		}
	case "ums-by-dbc":
		if s.detachCount == 1 {
			log.Println("ums-by-dbc mode: first disconnect, waiting for PC")
			s.setLEDs(ledsWaitingPC)
			return
		}
		if s.detachCount >= 2 {
			log.Println("ums-by-dbc mode: second disconnect, switching to normal")
			s.doSwitchToNormal()
		}
	default:
		log.Printf("Unknown UMS mode type %q, switching to normal", s.umsModeType)
		s.doSwitchToNormal()
	}
}

// doSwitchToNormal performs the switch without re-acquiring the mutex.
// Must be called with s.mu held.
func (s *Service) doSwitchToNormal() {
	prevMode := s.usbCtrl.GetCurrentMode()
	if err := s.switchToNormal(prevMode); err != nil {
		log.Printf("Error switching to normal mode: %v", err)
	}
	s.detachCount = 0

	if err := s.publisher.Set("mode", "normal", ipc.Sync()); err != nil {
		log.Printf("Error updating Redis usb mode: %v", err)
	}
}

// LED fade indices (from /usr/share/led-curves/fades/)
const (
	fadeSmoothOn  = 0 // fade0-parking-smooth-on
	fadeSmoothOff = 1 // fade1-smooth-off
)

// Blinker LED channels (3,4,6,7) used as UMS indicators.
// Continuous on = distinguishable from normal parked state.
type ledPattern struct {
	channels []int
	fade     int
}

var (
	ledsUMSActive = ledPattern{channels: []int{3, 4, 6, 7}, fade: fadeSmoothOn}
	ledsWaitingPC = ledPattern{channels: []int{3, 4}, fade: fadeSmoothOn}
	ledsOff       = ledPattern{channels: []int{3, 4, 6, 7}, fade: fadeSmoothOff}
)

var allBlinkerChannels = []int{3, 4, 6, 7}

func (s *Service) setLEDs(p ledPattern) {
	onSet := make(map[int]bool)
	for _, ch := range p.channels {
		onSet[ch] = true
	}

	for _, ch := range allBlinkerChannels {
		fade := p.fade
		if !onSet[ch] {
			fade = fadeSmoothOff
		}
		if _, err := s.client.LPush("scooter:led:fade", fmt.Sprintf("%d:%d", ch, fade)); err != nil {
			log.Printf("Error setting LED channel %d: %v", ch, err)
		}
	}
}

func (s *Service) setStatus(status string) {
	if err := s.publisher.Set("status", status, ipc.Sync()); err != nil {
		log.Printf("Error publishing usb status %q: %v", status, err)
	}
}
