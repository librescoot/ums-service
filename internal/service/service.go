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
	"github.com/librescoot/ums-service/pkg/maps"
	"github.com/librescoot/ums-service/pkg/rpm"
	"github.com/librescoot/ums-service/pkg/scripts"
	"github.com/librescoot/ums-service/pkg/settings"
	"github.com/librescoot/ums-service/pkg/umslog"
	"github.com/librescoot/ums-service/pkg/update"
	"github.com/librescoot/ums-service/pkg/usb"
	"github.com/librescoot/ums-service/pkg/wireguard"
)

// operation is a single mode transition. target is the desired mode.
// ctx is cancelled when the transition should be abandoned. done is
// closed when the operation goroutine exits (success or cancellation
// teardown). A stable state is represented by an operation with done
// already closed.
type operation struct {
	target string
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

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
	diagnostics  *diagnostics.Collector
	rpmInstaller *rpm.Installer
	scriptRunner *scripts.Runner

	serviceCtx context.Context

	dispatchMu sync.Mutex // serializes requestMode so only one caller installs the next op

	mu          sync.Mutex // protects currentOp, detachCount
	currentOp   *operation
	detachCount int
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
		diagnostics:  diagnostics.New(),
		rpmInstaller: rpmInstaller,
		scriptRunner: scriptRunner,
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

	s.serviceCtx = ctx

	if err := s.diskMgr.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize disk manager: %w", err)
	}

	// Seed currentOp with a stable "normal" state so dispatcher logic
	// has a well-defined starting point.
	stableDone := make(chan struct{})
	close(stableDone)
	s.mu.Lock()
	s.currentOp = &operation{
		target: "normal",
		ctx:    ctx,
		cancel: func() {},
		done:   stableDone,
	}
	s.mu.Unlock()

	s.usbCtrl.StartMonitoring()

	go s.detachLoop(ctx)

	if err := s.startBrakeExitListener(); err != nil {
		return fmt.Errorf("failed to start brake exit listener: %w", err)
	}

	go func() {
		<-ctx.Done()
		s.usbCtrl.StopMonitoring()
	}()

	if err := s.watcher.StartWithSync(); err != nil {
		return fmt.Errorf("failed to start hash watcher: %w", err)
	}

	log.Println("UMS service running, waiting for mode changes...")
	<-ctx.Done()
	return nil
}

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
	switch mode {
	case "ums", "ums-by-dbc", "normal":
		s.requestMode(mode)
		return nil
	default:
		return fmt.Errorf("unknown mode: %s", mode)
	}
}

func isUMSTarget(t string) bool {
	return t == "ums" || t == "ums-by-dbc"
}

// opDone reports whether op has finished. Callers should hold s.mu.
func opDone(op *operation) bool {
	if op == nil {
		return true
	}
	select {
	case <-op.done:
		return true
	default:
		return false
	}
}

// requestMode is the single entry point for mode transitions. It
// cancels any in-flight op, waits for its teardown, then starts a new
// op for the requested target. Callers include the Redis hash watcher,
// the USB detach loop, and the brake-hold handler.
func (s *Service) requestMode(target string) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()

	s.mu.Lock()
	cur := s.currentOp
	curDone := opDone(cur)

	// Already at or heading to the same target.
	if cur != nil && cur.target == target {
		s.mu.Unlock()
		return
	}

	// ums <-> ums-by-dbc swap while stable: just change detach
	// semantics, no need to re-prep the drive.
	if cur != nil && curDone && isUMSTarget(cur.target) && isUMSTarget(target) {
		cur.target = target
		log.Printf("UMS variant changed to %s", target)
		s.mu.Unlock()
		return
	}

	var oldDone chan struct{}
	publishIdle := false
	if cur != nil && !curDone {
		cur.cancel()
		oldDone = cur.done
		publishIdle = (target == "normal")
	}
	s.mu.Unlock()

	if publishIdle {
		s.publishIdle()
	}

	if oldDone != nil {
		<-oldDone
	}

	opCtx, cancel := context.WithCancel(s.serviceCtx)
	newOp := &operation{
		target: target,
		ctx:    opCtx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	s.mu.Lock()
	s.currentOp = newOp
	s.mu.Unlock()

	go s.runOperation(newOp)
}

func (s *Service) runOperation(op *operation) {
	defer close(op.done)
	switch op.target {
	case "ums", "ums-by-dbc":
		s.runUMSOp(op)
	case "normal":
		s.runNormalOp(op)
	default:
		log.Printf("Unknown op target: %s", op.target)
	}
}

// runUMSOp prepares the virtual drive and switches the USB gadget
// into mass-storage mode. It checks op.ctx between steps so a mid-prep
// cancel cleanly unwinds without finishing the switch.
func (s *Service) runUMSOp(op *operation) {
	mounted := false
	defer func() {
		if op.ctx.Err() != nil && mounted {
			// Always unmount even on cancel — a stale loopback mount
			// blocks the next session.
			if err := s.diskMgr.Unmount(context.Background()); err != nil {
				log.Printf("Error unmounting after cancel: %v", err)
			}
		}
	}()

	if op.ctx.Err() != nil {
		return
	}

	s.setStatus("preparing")

	if _, err := s.client.Del("usb:log"); err != nil {
		log.Printf("Warning: failed to clear usb:log: %v", err)
	}

	if err := s.diskMgr.Mount(op.ctx); err != nil {
		if op.ctx.Err() == nil {
			log.Printf("Failed to mount drive: %v", err)
			s.setStatus("idle")
		}
		return
	}
	mounted = true

	mountPoint := s.diskMgr.GetMountPoint()

	type prepStep struct {
		name string
		run  func() error
	}
	steps := []prepStep{
		{"settings", func() error { return s.settingsLdr.CopyToUSB(op.ctx, mountPoint) }},
		{"updates", func() error { return s.updateLdr.PrepareUSB(op.ctx, mountPoint) }},
		{"maps", func() error { return s.mapsUpdater.PrepareUSB(op.ctx, mountPoint) }},
		{"wireguard-dir", func() error { return s.wgManager.PrepareUSB(op.ctx, mountPoint) }},
		{"wireguard-copy", func() error { return s.wgManager.CopyToUSB(op.ctx, mountPoint) }},
		{"diagnostics", func() error {
			s.diagnostics.CollectToUSB(op.ctx, mountPoint)
			return nil
		}},
		{"rpms", func() error { return s.rpmInstaller.PrepareUSB(op.ctx, mountPoint) }},
		{"scripts", func() error { return s.scriptRunner.PrepareUSB(op.ctx, mountPoint) }},
	}
	for _, st := range steps {
		if op.ctx.Err() != nil {
			return
		}
		if err := st.run(); err != nil && op.ctx.Err() == nil {
			log.Printf("Error in prep step %q: %v", st.name, err)
		}
	}

	if op.ctx.Err() != nil {
		return
	}

	if err := s.diskMgr.Unmount(op.ctx); err != nil {
		log.Printf("Failed to unmount before UMS switch: %v", err)
		s.setStatus("idle")
		return
	}
	mounted = false

	if op.ctx.Err() != nil {
		return
	}

	// Publish status BEFORE switching USB — DBC can still read Redis via g_ether.
	s.setStatus("active")
	s.setLEDs(ledsUMSActive)

	if err := s.usbCtrl.SwitchMode("ums"); err != nil {
		log.Printf("Failed to switch to UMS mode: %v", err)
		s.setStatus("idle")
		s.setLEDs(ledsOff)
		return
	}

	s.mu.Lock()
	s.detachCount = 0
	s.mu.Unlock()

	log.Printf("Switched to UMS mode (type: %s)", op.target)
}

// runNormalOp handles both the plain "return to idle" case and the
// post-UMS processing case. The processing phase (when we were in UMS
// gadget mode) is intentionally non-cancellable: Mender installs and
// file transfers to DBC can't safely abort mid-flight.
func (s *Service) runNormalOp(op *operation) {
	gadgetMode := s.usbCtrl.GetCurrentMode()

	s.setLEDs(ledsOff)

	if err := s.usbCtrl.SwitchMode("normal"); err != nil {
		log.Printf("Failed to switch to normal mode: %v", err)
	}

	if gadgetMode != "ums" {
		s.publishIdle()
		return
	}

	// Post-UMS processing. Use a non-cancellable context so an
	// incoming mode change can't abort a Mender install.
	procCtx := context.Background()

	s.setStatus("processing")

	if err := s.diskMgr.Mount(procCtx); err != nil {
		log.Printf("Failed to mount drive for processing: %v", err)
		s.publishIdle()
		return
	}
	mountPoint := s.diskMgr.GetMountPoint()
	logger := umslog.New(s.client)

	needDBC := s.checkIfDBCNeeded(mountPoint)

	if needDBC {
		if err := s.dbcInterface.Enable(procCtx); err != nil {
			logger.Error("dbc", "Failed to enable: %v", err)
			log.Printf("Warning: failed to enable DBC: %v", err)
		} else {
			logger.Logf("dbc", "enabled")
		}
	}

	s.setStep("settings")
	settingsChanged := false
	if changed, err := s.settingsLdr.CopyFromUSB(procCtx, mountPoint); err != nil {
		logger.Error("settings", "%v", err)
		log.Printf("Error processing settings: %v", err)
	} else {
		logger.Logf("settings", "done (changed=%v)", changed)
		settingsChanged = changed
	}

	s.setStep("wireguard")
	wgChanged := false
	if changed, err := s.wgManager.SyncFromUSB(procCtx, mountPoint); err != nil {
		logger.Error("wireguard", "%v", err)
		log.Printf("Error processing wireguard configs: %v", err)
	} else {
		logger.Logf("wireguard", "done (changed=%v)", changed)
		wgChanged = changed
	}

	s.setStep("updates")
	if err := s.updateLdr.ProcessUpdates(procCtx, s.config.MenderTransferTimeout, logger, mountPoint); err != nil {
		logger.Error("updates", "%v", err)
		log.Printf("Error processing updates: %v", err)
	} else {
		logger.Logf("updates", "done")
	}
	logger.ClearProgress()

	s.setStep("maps")
	if err := s.mapsUpdater.ProcessMaps(procCtx, s.config.MapTransferTimeout, logger, mountPoint); err != nil {
		logger.Error("maps", "%v", err)
		log.Printf("Error processing maps: %v", err)
	} else {
		logger.Logf("maps", "done")
	}
	logger.ClearProgress()

	if err := s.rpmInstaller.ProcessRPMs(procCtx, s.config.RPMTransferTimeout, logger, mountPoint); err != nil {
		logger.Error("rpms", "%v", err)
		log.Printf("Error processing RPMs: %v", err)
	} else {
		logger.Logf("rpms", "done")
	}
	logger.ClearProgress()

	if err := s.scriptRunner.ProcessScripts(procCtx, s.config.ScriptTransferTimeout, logger, mountPoint); err != nil {
		logger.Error("scripts", "%v", err)
		log.Printf("Error processing scripts: %v", err)
	}
	logger.ClearProgress()

	if settingsChanged || wgChanged {
		log.Println("Configuration changed, restarting settings-service")
		cmd := exec.Command("systemctl", "restart", "settings-service")
		if output, err := cmd.CombinedOutput(); err != nil {
			logger.Error("settings-service", "restart failed: %v", err)
			log.Printf("Failed to restart settings-service: %v, output: %s", err, string(output))
		} else {
			logger.Logf("settings-service", "restarted")
			log.Println("Successfully restarted settings-service")
		}
	}

	if err := logger.WriteToFile(filepath.Join(mountPoint, "ums_log.txt")); err != nil {
		log.Printf("Error writing log file: %v", err)
	}

	if err := s.diskMgr.CleanDrive(procCtx); err != nil {
		log.Printf("Error cleaning USB drive: %v", err)
	}

	if err := s.diskMgr.Unmount(procCtx); err != nil {
		log.Printf("Error unmounting USB drive: %v", err)
	}

	if needDBC {
		if err := s.dbcInterface.Disable(); err != nil {
			log.Printf("Warning: failed to disable DBC: %v", err)
		}
	}

	s.publishIdle()
	log.Println("Switched to normal mode and processed files")
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

// onDeviceDetached runs when the USB host disconnects while the
// gadget is in UMS mode. For plain ums, one detach exits. For
// ums-by-dbc, the first detach is the DBC's reboot cycle and we
// wait for the second (from the PC) before exiting.
func (s *Service) onDeviceDetached() {
	s.mu.Lock()
	cur := s.currentOp
	if cur == nil || !opDone(cur) || !isUMSTarget(cur.target) {
		s.mu.Unlock()
		return
	}
	s.detachCount++
	target := cur.target
	count := s.detachCount
	s.mu.Unlock()

	log.Printf("USB detach #%d detected (mode type: %s)", count, target)

	switch target {
	case "ums":
		log.Println("ums mode: switching to normal after disconnect")
		s.exitToNormal()
	case "ums-by-dbc":
		if count == 1 {
			log.Println("ums-by-dbc mode: first disconnect, waiting for PC")
			s.setLEDs(ledsWaitingPC)
			return
		}
		log.Println("ums-by-dbc mode: second disconnect, switching to normal")
		s.exitToNormal()
	default:
		log.Printf("Unknown UMS target %q, switching to normal", target)
		s.exitToNormal()
	}
}

// exitToNormal triggers a normal transition and updates Redis so
// external observers see the new mode.
func (s *Service) exitToNormal() {
	if err := s.publisher.Set("mode", "normal", ipc.Sync()); err != nil {
		log.Printf("Error updating Redis usb mode: %v", err)
	}
	s.requestMode("normal")
}

// LED fade indices (from /usr/share/led-curves/fades/)
const (
	fadeSmoothOn  = 0 // fade0-parking-smooth-on
	fadeSmoothOff = 1 // fade1-smooth-off
)

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

// publishIdle sets status=idle and step="" atomically so the UI sees
// a clean return-to-idle instead of a transient mixed state.
func (s *Service) publishIdle() {
	if err := s.publisher.SetMany(map[string]any{
		"status": "idle",
		"step":   "",
	}, ipc.Sync()); err != nil {
		log.Printf("Error publishing idle: %v", err)
	}
}
