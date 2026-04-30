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
	"github.com/librescoot/ums-service/pkg/diagnostics"
	"github.com/librescoot/ums-service/pkg/disk"
	"github.com/librescoot/ums-service/pkg/logbundles"
	"github.com/librescoot/ums-service/pkg/maps"
	"github.com/librescoot/ums-service/pkg/onboot"
	"github.com/librescoot/ums-service/pkg/radiogaga"
	"github.com/librescoot/ums-service/pkg/rpm"
	"github.com/librescoot/ums-service/pkg/scripts"
	"github.com/librescoot/ums-service/pkg/settings"
	"github.com/librescoot/ums-service/pkg/umslog"
	"github.com/librescoot/ums-service/pkg/update"
	"github.com/librescoot/ums-service/pkg/uplink"
	"github.com/librescoot/ums-service/pkg/usb"
	"github.com/librescoot/ums-service/pkg/wireguard"
)

const logBundleKeepCount = 10

type Service struct {
	config        *config.Config
	client        *ipc.Client
	watcher       *ipc.HashWatcher
	publisher     *ipc.HashPublisher
	usbCtrl       *usb.Controller
	diskMgr       *disk.Manager
	dbcInterface  *dbc.Interface
	settingsLdr   *settings.Loader
	updateLdr     *update.Loader
	mapsUpdater   *maps.Updater
	wgManager     *wireguard.Manager
	diagnostics   *diagnostics.Collector
	rpmInstaller  *rpm.Installer
	scriptRunner  *scripts.Runner
	logBundlesMgr *logbundles.Manager
	radioGagaMgr  *radiogaga.Manager
	uplinkMgr     *uplink.Manager
	onbootMgr     *onboot.Manager
	mu            sync.Mutex
	detachCount   int
	umsModeType   string
}

func New(cfg *config.Config) (*Service, error) {
	redisHost, redisPort, err := parseRedisAddr(cfg.RedisAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_ADDR %q: %w", cfg.RedisAddr, err)
	}

	client, err := ipc.New(
		ipc.WithAddress(redisHost),
		ipc.WithPort(redisPort),
		ipc.WithCodec(ipc.StringCodec{}),
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
	rpmInstaller := rpm.New(dbcInterface)
	scriptRunner := scripts.New(dbcInterface)

	svc := &Service{
		config:        cfg,
		client:        client,
		watcher:       client.NewHashWatcher("usb"),
		publisher:     client.NewHashPublisher("usb"),
		usbCtrl:       usbCtrl,
		diskMgr:       diskMgr,
		dbcInterface:  dbcInterface,
		settingsLdr:   settingsLdr,
		updateLdr:     updateLdr,
		mapsUpdater:   mapsUpdater,
		wgManager:     wgManager,
		diagnostics:   diagnostics.New(),
		rpmInstaller:  rpmInstaller,
		scriptRunner:  scriptRunner,
		logBundlesMgr: logbundles.New(),
		radioGagaMgr:  radiogaga.New(),
		uplinkMgr:     uplink.New(),
		onbootMgr:     onboot.New(),
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

	s.runStartupCleanup()

	s.usbCtrl.StartMonitoring()

	go s.detachLoop(ctx)

	if err := s.startBrakeExitListener(); err != nil {
		return fmt.Errorf("failed to start brake exit listener: %w", err)
	}

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

	if _, err := s.client.Del("usb:log"); err != nil {
		log.Printf("Warning: failed to clear usb:log: %v", err)
	}

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

	if err := s.radioGagaMgr.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing radio-gaga directory: %v", err)
	}
	if err := s.radioGagaMgr.CopyToUSB(mountPoint); err != nil {
		log.Printf("Error copying radio-gaga config to USB: %v", err)
	}

	if err := s.uplinkMgr.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing uplink-service directory: %v", err)
	}
	if err := s.uplinkMgr.CopyToUSB(mountPoint); err != nil {
		log.Printf("Error copying uplink-service config to USB: %v", err)
	}

	if err := s.onbootMgr.CopyToUSB(mountPoint); err != nil {
		log.Printf("Error copying onboot.sh to USB: %v", err)
	}

	if err := s.logBundlesMgr.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing log-bundles directory: %v", err)
	}
	if err := s.logBundlesMgr.CopyToUSB(mountPoint); err != nil {
		log.Printf("Error copying log bundles to USB: %v", err)
	}

	s.diagnostics.CollectToUSB(mountPoint)

	if err := s.rpmInstaller.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing rpms directory: %v", err)
	}

	if err := s.scriptRunner.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing scripts directory: %v", err)
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
		s.setStep("")
		s.setStatus("idle")
		return nil
	}

	s.setStatus("processing")

	if err := s.diskMgr.Mount(); err != nil {
		s.setStep("")
		s.setStatus("idle")
		return fmt.Errorf("failed to mount drive: %w", err)
	}

	ctx := context.Background()
	mountPoint := s.diskMgr.GetMountPoint()
	logger := umslog.New(s.client)

	needDBC := s.checkIfDBCNeeded(mountPoint)

	if needDBC {
		if err := s.dbcInterface.Enable(ctx); err != nil {
			logger.Error("dbc", "Failed to enable: %v", err)
			log.Printf("Warning: failed to enable DBC: %v", err)
		} else {
			logger.Logf("dbc", "enabled")
		}
	}

	s.setStep("settings")
	settingsChanged := false
	if changed, err := s.settingsLdr.CopyFromUSB(mountPoint); err != nil {
		logger.Error("settings", "%v", err)
		log.Printf("Error processing settings: %v", err)
	} else {
		logger.Logf("settings", "done (changed=%v)", changed)
		settingsChanged = changed
	}

	s.setStep("wireguard")
	wgChanged := false
	if changed, err := s.wgManager.SyncFromUSB(mountPoint); err != nil {
		logger.Error("wireguard", "%v", err)
		log.Printf("Error processing wireguard configs: %v", err)
	} else {
		logger.Logf("wireguard", "done (changed=%v)", changed)
		wgChanged = changed
	}

	s.setStep("radio-gaga")
	radioGagaChanged := false
	if changed, err := s.radioGagaMgr.CopyFromUSB(mountPoint); err != nil {
		logger.Error("radio-gaga", "%v", err)
		log.Printf("Error processing radio-gaga config: %v", err)
	} else {
		logger.Logf("radio-gaga", "done (changed=%v)", changed)
		radioGagaChanged = changed
	}

	s.setStep("uplink-service")
	uplinkChanged := false
	if changed, err := s.uplinkMgr.CopyFromUSB(mountPoint); err != nil {
		logger.Error("uplink-service", "%v", err)
		log.Printf("Error processing uplink-service config: %v", err)
	} else {
		logger.Logf("uplink-service", "done (changed=%v)", changed)
		uplinkChanged = changed
	}

	s.setStep("onboot")
	if changed, err := s.onbootMgr.CopyFromUSB(mountPoint); err != nil {
		logger.Error("onboot", "%v", err)
		log.Printf("Error processing onboot.sh: %v", err)
	} else {
		logger.Logf("onboot", "done (changed=%v)", changed)
	}

	s.setStep("updates")
	if err := s.updateLdr.ProcessUpdates(ctx, s.config.MenderTransferTimeout, logger, mountPoint); err != nil {
		logger.Error("updates", "%v", err)
		log.Printf("Error processing updates: %v", err)
	} else {
		logger.Logf("updates", "done")
	}
	logger.ClearProgress()

	s.setStep("maps")
	if err := s.mapsUpdater.ProcessMaps(ctx, s.config.MapTransferTimeout, logger, mountPoint); err != nil {
		logger.Error("maps", "%v", err)
		log.Printf("Error processing maps: %v", err)
	} else {
		logger.Logf("maps", "done")
	}
	logger.ClearProgress()

	if err := s.rpmInstaller.ProcessRPMs(ctx, s.config.RPMTransferTimeout, logger, mountPoint); err != nil {
		logger.Error("rpms", "%v", err)
		log.Printf("Error processing RPMs: %v", err)
	} else {
		logger.Logf("rpms", "done")
	}
	logger.ClearProgress()

	if err := s.scriptRunner.ProcessScripts(ctx, s.config.ScriptTransferTimeout, logger, mountPoint); err != nil {
		logger.Error("scripts", "%v", err)
		log.Printf("Error processing scripts: %v", err)
	}
	logger.ClearProgress()

	if settingsChanged || wgChanged {
		restartUnit(logger, "settings-service")
	}
	if radioGagaChanged {
		restartUnit(logger, "radio-gaga.service")
	}
	if uplinkChanged {
		restartUnit(logger, "librescoot-uplink.service")
	}

	if err := logger.WriteToFile(filepath.Join(mountPoint, "ums_log.txt")); err != nil {
		log.Printf("Error writing log file: %v", err)
	}

	s.runPostCycleCleanup()

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
	s.setStep("")
	s.setStatus("idle")
	log.Println("Switched to normal mode and processed files")

	return nil
}

func (s *Service) checkIfDBCNeeded(mountPoint string) bool {
	updateDir := filepath.Join(mountPoint, "system-update")
	if entries, err := os.ReadDir(updateDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), "librescoot-") && strings.Contains(entry.Name(), "-dbc") && strings.HasSuffix(entry.Name(), ".mender") {
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
				if strings.HasSuffix(filename, ".mbtiles") ||
					strings.HasSuffix(filename, "tiles.tar") ||
					(strings.HasPrefix(filename, "valhalla_tiles_") && strings.HasSuffix(filename, ".tar")) {
					log.Println("Found map files, DBC needed")
					return true
				}
			}
		}
	}

	dbcRPMDir := filepath.Join(mountPoint, "rpms", "dbc")
	if entries, err := os.ReadDir(dbcRPMDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".rpm") {
				log.Println("Found DBC RPM files, DBC needed")
				return true
			}
		}
	}

	dbcScript := filepath.Join(mountPoint, "scripts", "dbc.sh")
	if _, err := os.Stat(dbcScript); err == nil {
		log.Println("Found DBC script, DBC needed")
		return true
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

func (s *Service) setStep(step string) {
	if err := s.publisher.Set("step", step, ipc.Sync()); err != nil {
		log.Printf("Error publishing usb step %q: %v", step, err)
	}
}

func (s *Service) runStartupCleanup() {
	if err := s.logBundlesMgr.PruneOldBundles(logBundleKeepCount); err != nil {
		log.Printf("Warning: failed to prune old log bundles: %v", err)
	}
	if err := s.updateLdr.CleanupStaleFiles(); err != nil {
		log.Printf("Warning: failed to clean up stale OTA files: %v", err)
	}
}

// runPostCycleCleanup runs after a UMS cycle has finished applying USB content.
// It skips pruning of /data/ota/{mdb,dbc} because update-service may still be
// installing the .mender we just queued there; the next boot's full cleanup
// will sweep those.
func (s *Service) runPostCycleCleanup() {
	if err := s.logBundlesMgr.PruneOldBundles(logBundleKeepCount); err != nil {
		log.Printf("Warning: failed to prune old log bundles: %v", err)
	}
	if err := s.updateLdr.CleanupStaleFilesPostCycle(); err != nil {
		log.Printf("Warning: failed to clean up stale OTA files: %v", err)
	}
}

func restartUnit(logger *umslog.Logger, unit string) {
	log.Printf("Restarting %s", unit)
	cmd := exec.Command("systemctl", "restart", unit)
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error(unit, "restart failed: %v", err)
		log.Printf("Failed to restart %s: %v, output: %s", unit, err, string(output))
		return
	}
	logger.Logf(unit, "restarted")
	log.Printf("Successfully restarted %s", unit)
}
