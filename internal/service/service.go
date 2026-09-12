package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/ums-service/pkg/config"
	"github.com/librescoot/ums-service/pkg/dbc"
	"github.com/librescoot/ums-service/pkg/diagnostics"
	"github.com/librescoot/ums-service/pkg/disk"
	"github.com/librescoot/ums-service/pkg/logbundles"
	"github.com/librescoot/ums-service/pkg/maps"
	"github.com/librescoot/ums-service/pkg/onboot"
	"github.com/librescoot/ums-service/pkg/radiogaga"
	"github.com/librescoot/ums-service/pkg/scripts"
	"github.com/librescoot/ums-service/pkg/settings"
	"github.com/librescoot/ums-service/pkg/umslog"
	"github.com/librescoot/ums-service/pkg/update"
	"github.com/librescoot/ums-service/pkg/uplink"
	"github.com/librescoot/ums-service/pkg/usb"
	"github.com/librescoot/ums-service/pkg/wireguard"
)

const logBundleKeepCount = 10

const (
	// installAwaitTimeout is the per-window liveness timeout for the
	// install awaiter. The window resets whenever an install is
	// genuinely still progressing (see update.WaitForCompletion), so a
	// slow install is not abandoned mid-flight; only a window that
	// expires with no install activity ends the wait early.
	installAwaitTimeout = 10 * time.Minute
	// installOverallCap bounds the total time the awaiter keeps the
	// MDB reboot-ownership claim while waiting. On final expiry the
	// claim is intentionally retained as a fail-safe (as before), but
	// the cap must be generous enough to cover slow delta installs
	// (observed ~12 min on the bench for 063513) with large margin.
	installOverallCap  = 2 * time.Hour
	mdbRebootOwnerPath = "/run/librescoot/ums-mdb-reboot-owner"
)

var rebootAllowedVehicleStates = map[string]bool{
	"stand-by":      true,
	"parked":        true,
	"shutting-down": true,
}

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
	scriptRunner  *scripts.Runner
	logBundlesMgr *logbundles.Manager
	radioGagaMgr  *radiogaga.Manager
	uplinkMgr     *uplink.Manager
	onbootMgr     *onboot.Manager
	mu            sync.Mutex
	detachCount   int
	umsModeType   string
	// cancelPending is set by the brake exit listener without taking mu,
	// which switchToUMS holds for the whole preparing phase. It lets a
	// left brake hold during preparing abandon the entry before the USB
	// gadget is ever switched. Cleared at the start of every entry.
	cancelPending atomic.Bool
	serviceCtx    context.Context    // set in Run; parent for reboot goroutine
	rebootWatcher context.CancelFunc // cancel pending reboot goroutine; nil if none
	rebootGen     int                // increments per startRebootWatcher; lets a stale goroutine know it's been superseded
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
	mapsUpdater := maps.New(dbcInterface, client)
	wgManager := wireguard.New()

	updateLdr := update.New(client, dbcInterface)
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
		diagnostics:   diagnostics.New(client),
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
	s.serviceCtx = ctx

	if err := s.diskMgr.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize disk manager: %w", err)
	}

	s.runStartupCleanup()
	// Reconcile the MDB reboot-owner claim with what actually happened
	// before this process started. Invariant: the claim is held by a
	// live awaiter; a restarted ums-service adopts an install that
	// already completed (MDB at pending-reboot) when no other component
	// is mid-flight, because update-service defers to the claim and
	// would otherwise wait forever for a reboot nobody owns. Any other
	// active install keeps the claim with its (dead) owner — that is
	// the existing fail-safe — and pending/error/idle states clear or
	// keep it as before.
	if ota, err := s.client.HGetAll("ota"); err == nil {
		s.reconcileRebootOwner(ota)
	}

	s.usbCtrl.StartMonitoring()

	go s.detachLoop(ctx)

	if err := s.startBrakeExitListener(); err != nil {
		return fmt.Errorf("failed to start brake exit listener: %w", err)
	}

	go func() {
		<-ctx.Done()
		s.usbCtrl.StopMonitoring()
	}()

	// Seed the usb hash with the baseline state so readers (e.g. `lsc usb
	// status`) see a real value instead of an empty hash on a boot where no
	// mode change has happened yet. Writing mode=normal also reconciles any
	// stale mode=ums left in Redis if the scooter rebooted mid-session, since
	// the controller always starts in normal mode. NoPublish keeps boot from
	// emitting a spurious change notification; the watcher's StartWithSync
	// below reads the hash directly regardless.
	if err := s.publisher.SetMany(map[string]any{
		"mode":   s.usbCtrl.GetCurrentMode(),
		"status": "idle",
	}, ipc.Sync(), ipc.NoPublish()); err != nil {
		return fmt.Errorf("failed to seed usb hash: %w", err)
	}

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
		s.setResult(resultError, "rejected unsupported USB mode %q; remaining in %s", mode, prevMode)
		if err := s.publisher.Set("mode", prevMode, ipc.Sync()); err != nil {
			return fmt.Errorf("unknown mode %q and failed to restore mode %q: %w", mode, prevMode, err)
		}
		return fmt.Errorf("unknown mode: %s", mode)
	}
}

func (s *Service) switchToUMS(mode string) error {
	// Only a hold that lands from here on counts as cancelling this entry.
	s.cancelPending.Store(false)
	s.setStatus("preparing")

	if s.rebootWatcher != nil {
		log.Println("Cancelling pending reboot watcher (re-entering UMS)")
		s.rebootWatcher()
		// Don't nil rebootWatcher here — the goroutine's defer
		// handles cleanup under the generation check. Nilling
		// here would also be safe but is redundant.
	}

	if _, err := s.client.Del("usb:log"); err != nil {
		log.Printf("Warning: failed to clear usb:log: %v", err)
	}

	s.clearResult()

	if err := s.diskMgr.Mount(); err != nil {
		s.setResult(resultError, "could not prepare the USB drive: %v", err)
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

	if err := s.scriptRunner.PrepareUSB(mountPoint); err != nil {
		log.Printf("Error preparing scripts directory: %v", err)
	}

	if err := s.diskMgr.Unmount(); err != nil {
		s.setStatus("idle")
		return fmt.Errorf("failed to unmount drive: %w", err)
	}

	// A left brake hold during preparing abandons the entry here, with the
	// drive already unmounted and the gadget untouched. Bailing out before
	// SwitchMode avoids loading g_mass_storage only to unload it again on
	// the exit path, which would drop the DBC's g_ether link for no reason.
	if s.cancelPending.Load() {
		log.Println("UMS entry cancelled by left brake hold during preparing")
		s.setStep("")
		s.setStatus("idle")
		// The gadget never moved, but usb.mode still reads ums from the
		// request that got us here. Reconcile it the way doSwitchToNormal
		// does, so the hash keeps matching the controller.
		if err := s.publisher.Set("mode", "normal", ipc.Sync()); err != nil {
			log.Printf("Error updating Redis usb mode: %v", err)
		}
		return nil
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
		s.setResult(resultError, "could not read the USB drive: %v", err)
		s.setStep("")
		s.setStatus("idle")
		return fmt.Errorf("failed to mount drive: %w", err)
	}

	ctx := context.Background()
	mountPoint := s.diskMgr.GetMountPoint()
	logger := umslog.New(s.client)

	needDBC, err := s.checkIfDBCNeeded(mountPoint)
	if err != nil {
		// Abort before per-file processing and before the drive is
		// cleaned: the staged artifacts stay on the drive and are
		// imported by the next detach instead of being silently lost.
		logger.Error("dbc", "scan failed: %v", err)
		log.Printf("Error scanning the USB drive: %v", err)
		s.setResult(resultError, "could not scan the USB drive: %v", err)
		s.setStep("")
		s.setStatus("idle")
		if umountErr := s.diskMgr.Unmount(); umountErr != nil {
			log.Printf("Error unmounting USB drive after scan failure: %v", umountErr)
		}
		return fmt.Errorf("scan the USB drive: %w", err)
	}

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
	queued, err := s.updateLdr.ProcessUpdates(ctx, s.config.MenderTransferTimeout, logger, mountPoint)
	if err != nil {
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

	s.setStep("scripts")
	if err := s.scriptRunner.ProcessScripts(ctx, s.config.ScriptTransferTimeout, logger, mountPoint); err != nil {
		logger.Error("scripts", "%v", err)
		log.Printf("Error processing scripts: %v", err)
	}
	logger.ClearProgress()

	if settingsChanged || wgChanged {
		restartUnit(logger, "librescoot-settings.service")
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

	if err == nil && (queued.MDB || queued.DBC) {
		// Hand off to the awaiter goroutine. It owns setStatus
		// transitions from "awaiting-reboot" back to "idle".
		// On ProcessUpdates error we skip the watcher even if some
		// pushes were staged — the partial state would confuse a
		// user who only sees the error in usb:log.
		s.startRebootWatcher(queued)
	} else {
		s.setStatus("idle")
	}
	log.Println("Switched to normal mode and processed files")

	return nil
}

// startRebootWatcher launches a goroutine that subscribes to the ota
// hash, performs the queued install LPushes, waits for completion, and
// triggers a reboot. Must be called with s.mu held (so writes to
// s.rebootWatcher / s.rebootGen are race-free with switchToUMS).
func (s *Service) startRebootWatcher(queued update.Queued) {
	ctx, cancel := context.WithCancel(s.serviceCtx)
	s.rebootWatcher = cancel
	s.rebootGen++
	myGen := s.rebootGen
	s.setStatus("awaiting-reboot")
	go s.awaitInstallsAndReboot(ctx, queued, myGen)
}

func (s *Service) awaitInstallsAndReboot(ctx context.Context, queued update.Queued, myGen int) {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		// If a newer goroutine has been started, leave its state
		// alone — we're a zombie from a cancelled cycle.
		if s.rebootGen != myGen {
			return
		}
		s.rebootWatcher = nil
		// If we were cancelled externally, whoever cancelled us
		// (switchToUMS) already owns the status field; don't clobber.
		if ctx.Err() == nil {
			s.setStep("")
			s.setStatus("idle")
		}
	}()

	logger := umslog.New(s.client)

	source, err := update.NewIPCOTASource(s.client)
	if err != nil {
		logger.Error("reboot", "subscribe to ota hash: %v", err)
		log.Printf("awaiter: subscribe failed: %v", err)
		s.setResult(resultError, "could not watch the ota hash: %v", err)
		return
	}
	defer source.Stop()
	// Recording keeps the observed status history so a failed wait can
	// be re-checked against what actually happened (InstallRecovered).
	rec := update.NewRecordingSource(source)

	// update-service normally owns MDB reboot scheduling. Claim it before
	// publishing install requests so combined imports cannot reboot the MDB
	// while the DBC is still rebooting, verifying, or committing. The claim is
	// scoped to this awaiter and is cleared on every exit.
	releaseRebootOwner := false
	if queued.MDB {
		if err := os.MkdirAll(filepath.Dir(mdbRebootOwnerPath), 0o755); err != nil {
			s.setResult(resultError, "could not create MDB reboot owner directory: %v", err)
			return
		}
		if err := os.WriteFile(mdbRebootOwnerPath, []byte("ums\n"), 0o644); err != nil {
			s.setResult(resultError, "could not persist MDB reboot ownership: %v", err)
			return
		}
		if err := s.client.HSet("ota", "reboot-owner:mdb", "ums"); err != nil {
			_ = os.Remove(mdbRebootOwnerPath)
			s.setResult(resultError, "could not claim MDB reboot ownership: %v", err)
			return
		}
		ownerDone := make(chan struct{})
		ownerStopped := make(chan struct{})
		// Redis is volatile. Renew the claim while installs are being watched so
		// a flush cannot hand MDB reboot ownership back to update-service in the
		// middle of a combined DBC activation.
		go func() {
			defer close(ownerStopped)
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ownerDone:
					return
				case <-ticker.C:
					if err := s.client.HSet("ota", "reboot-owner:mdb", "ums"); err != nil {
						log.Printf("awaiter: failed to renew MDB reboot ownership: %v", err)
					}
				}
			}
		}()
		defer func() {
			close(ownerDone)
			<-ownerStopped
			if !releaseRebootOwner {
				return
			}
			if err := s.client.HSet("ota", "reboot-owner:mdb", ""); err != nil {
				log.Printf("awaiter: failed to clear MDB reboot ownership: %v", err)
				return
			}
			if err := os.Remove(mdbRebootOwnerPath); err != nil && !os.IsNotExist(err) {
				log.Printf("awaiter: failed to clear MDB reboot owner file: %v", err)
			}
		}()
	}

	for _, p := range queued.PendingPushes {
		if _, perr := s.client.LPush(p.Channel, p.Value); perr != nil {
			logger.Error("reboot", "LPush %s failed: %v", p.Channel, perr)
			log.Printf("awaiter: LPush %s failed: %v", p.Channel, perr)
			s.setResult(resultError, "could not queue install on %s: %v", p.Channel, perr)
			return
		}
		logger.Logf("reboot", "queued %s", p.Channel)
	}

	pending := update.RequiredComponents(queued)
	onPending := func(components []string) {
		pending = components
		s.setStep("waiting-" + strings.Join(components, "+"))
	}

	if err := update.WaitForCompletion(ctx, rec, queued, installAwaitTimeout, installOverallCap, onPending); err != nil {
		if !errors.Is(err, context.Canceled) && update.InstallRecovered(rec, queued) {
			// The wait gave up, but the recording shows every queued
			// install actually recovered to its completed state (bench:
			// a transient DBC error flip made the old code retain the
			// reboot-owner claim while the MDB sat at pending-reboot,
			// deadlocking update-service). Finish the cycle normally.
			logger.Logf("reboot", "wait ended with %v but installs recovered; completing the reboot", err)
			log.Printf("awaiter: %v, but installs recovered; completing reboot", err)
		} else {
			logger.Error("reboot", "skip: %v", err)
			log.Printf("awaiter: skip reboot: %v", err)
			switch {
			case errors.Is(err, context.Canceled):
				// A new UMS entry clears the superseded result.
			case errors.Is(err, context.DeadlineExceeded):
				s.setResult(resultTimeout, "install did not finish within %s (still waiting on %s)",
					installOverallCap, strings.Join(pending, ", "))
			default:
				s.setResult(resultInstallError, "%v", err)
			}
			return
		}
	}

	// All queued installers are now terminal — or the wait failed but
	// InstallRecovered verified they recovered to their completed
	// states. It is safe to release MDB reboot ownership on any
	// subsequent return; the retained-claim paths returned above.
	releaseRebootOwner = true

	if queued.DBC && !queued.MDB {
		s.setResult(resultRebootTriggered, "DBC reboot completed")
		logger.Logf("reboot", "DBC reboot completed by update-service")
		log.Println("awaiter: DBC reboot completed by update-service")
		return
	}

	s.setStep("waiting-vehicle-state")

	state, err := s.client.HGet("vehicle", "state")
	if err != nil {
		logger.Error("reboot", "skip: failed to read vehicle state: %v", err)
		log.Printf("awaiter: failed to read vehicle state: %v", err)
		s.setResult(resultError, "could not read vehicle state: %v", err)
		return
	}
	if !rebootAllowedVehicleStates[state] {
		logger.Logf("reboot", "skip: vehicle state %q not in allowed set", state)
		log.Printf("awaiter: skip reboot, vehicle state is %q", state)
		s.setResult(resultVehicleState,
			"install is staged but the reboot was skipped: vehicle state %q does not allow it", state)
		return
	}

	// Installation is complete and the safety gate has passed. Expose the
	// short final phase explicitly so the dashboard does not keep describing
	// an already-finished install as merely awaiting a reboot.
	s.setStatus("rebooting")
	// Give the DBC enough time to paint the final state before its power-off
	// command is consumed. This runs only in the background awaiter.
	time.Sleep(500 * time.Millisecond)

	if _, err := s.client.LPush("scooter:power", "reboot"); err != nil {
		logger.Error("reboot", "LPush scooter:power reboot failed: %v", err)
		log.Printf("awaiter: failed to trigger MDB reboot: %v", err)
		s.setResult(resultError, "could not trigger the MDB reboot: %v", err)
		return
	}
	s.setResult(resultRebootTriggered, "MDB reboot triggered")
	logger.Logf("reboot", "MDB reboot triggered")
	log.Println("awaiter: MDB reboot triggered")
}

// ownerAction is what reconcileRebootOwner should do with a stale
// reboot-owner claim found at startup.
type ownerAction int

const (
	// ownerKeep leaves the claim in place: an install is genuinely
	// still active, or a completed one cannot safely be adopted yet.
	ownerKeep ownerAction = iota
	// ownerClear removes the claim: nothing is pending any more.
	ownerClear
	// ownerAdopt means the MDB install completed while the previous
	// process was dying; the restarted service must clear the claim
	// and finish the reboot itself.
	ownerAdopt
)

// decideRebootOwnerAction classifies a startup ota snapshot. owner is
// the Redis reboot-owner value; ownerHeld reports whether the on-disk
// claim marker exists.
func decideRebootOwnerAction(mdb, dbc, owner string, ownerHeld bool) ownerAction {
	if !ownerHeld && owner != "ums" {
		return ownerKeep
	}
	switch {
	case mdb == "pending-reboot":
		switch dbc {
		case "downloading", "preparing", "installing", "pending-reboot":
			// The DBC is still mid-install; adopting now would reboot
			// the MDB out from under a live combined activation.
			return ownerKeep
		default:
			return ownerAdopt
		}
	case mdb == "downloading" || mdb == "preparing" || mdb == "installing":
		return ownerKeep
	case mdb == "" || mdb == "idle" || mdb == "error":
		if dbc == "downloading" || dbc == "preparing" || dbc == "installing" || dbc == "pending-reboot" {
			// A DBC install may still be activating; keep the fail-safe
			// claim until it settles.
			return ownerKeep
		}
		return ownerClear
	default:
		return ownerKeep
	}
}

// clearRebootOwner removes the claim from Redis and from disk.
func (s *Service) clearRebootOwner() {
	if err := s.client.HSet("ota", "reboot-owner:mdb", ""); err != nil {
		log.Printf("Failed to clear orphaned MDB reboot ownership: %v", err)
		return
	}
	if err := os.Remove(mdbRebootOwnerPath); err != nil && !os.IsNotExist(err) {
		log.Printf("Failed to clear orphaned MDB reboot owner file: %v", err)
	}
}

// reconcileRebootOwner applies the startup decision for a stale claim.
func (s *Service) reconcileRebootOwner(ota map[string]string) {
	mdb := ota["status:mdb"]
	dbc := ota["status:dbc"]
	owner := ota["reboot-owner:mdb"]
	_, ownerFileErr := os.Stat(mdbRebootOwnerPath)
	ownerHeld := ownerFileErr == nil

	switch decideRebootOwnerAction(mdb, dbc, owner, ownerHeld) {
	case ownerClear:
		s.clearRebootOwner()
	case ownerAdopt:
		// Gate on the vehicle state exactly like the live awaiter: the
		// reboot must only fire from stand-by, parked, or shutting-down.
		state, err := s.client.HGet("vehicle", "state")
		if err != nil {
			log.Printf("Keeping MDB reboot ownership: cannot read vehicle state: %v", err)
			return
		}
		if !rebootAllowedVehicleStates[state] {
			log.Printf("Keeping MDB reboot ownership: vehicle state %q does not allow a reboot; a later restart or UMS cycle can adopt the completed install", state)
			return
		}
		s.clearRebootOwner()
		if _, err := s.client.LPush("scooter:power", "reboot"); err != nil {
			log.Printf("Failed to trigger the adopted MDB reboot: %v", err)
			return
		}
		log.Printf("Adopted completed MDB install (pending-reboot) and triggered the MDB reboot")
	default:
		log.Printf("Keeping MDB reboot ownership: mdb=%q dbc=%q", mdb, dbc)
	}
}

func (s *Service) checkIfDBCNeeded(mountPoint string) (bool, error) {
	updateDir := filepath.Join(mountPoint, "system-update")
	entries, err := readDirWithRetry(updateDir)
	if err != nil {
		return false, fmt.Errorf("scan %s: %w", updateDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && update.IsDBCUpdateArtifact(entry.Name()) {
			log.Println("Found DBC update files, DBC needed")
			return true, nil
		}
	}

	mapsDir := filepath.Join(mountPoint, "maps")
	entries, err = readDirWithRetry(mapsDir)
	if err != nil {
		return false, fmt.Errorf("scan %s: %w", mapsDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			filename := entry.Name()
			if strings.HasSuffix(filename, ".mbtiles") || maps.IsValhallaTilesArchive(filename) {
				log.Println("Found map files, DBC needed")
				return true, nil
			}
		}
	}

	dbcScript := filepath.Join(mountPoint, "scripts", "dbc.sh")
	if _, err := os.Stat(dbcScript); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("stat %s: %w", dbcScript, err)
	} else if err == nil {
		log.Println("Found DBC script, DBC needed")
		return true, nil
	}

	log.Println("No DBC operations needed")
	return false, nil
}

// readDirWithRetry retries a directory listing: the FAT remount right
// after a UMS detach can briefly serve I/O errors, and a false "no DBC
// operations needed" here skips the DBC interface enable and later
// aborts the whole import pass. A directory that genuinely does not
// exist is an empty listing, not an error; any other error that
// survives the retries fails the scan so the caller aborts with the
// staged artifacts intact instead of silently importing nothing.
func readDirWithRetry(path string) ([]os.DirEntry, error) {
	var entries []os.DirEntry
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		entries, err = os.ReadDir(path)
		if err == nil {
			return entries, nil
		}
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	return nil, err
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
	fadeSmoothOff = 1 // fade1-smooth-off, ends at duty 6
	fadeDimOn     = 4 // fade4-brake-dim-on, ramps to duty 156
)

// Blinker LED channels (3,4,6,7) used as UMS indicators.
// Continuous on = distinguishable from normal parked state.
type ledPattern struct {
	channels []int
	fade     int
}

var (
	ledsUMSActive = ledPattern{channels: []int{3, 4, 6, 7}, fade: fadeDimOn}
	ledsWaitingPC = ledPattern{channels: []int{3, 4}, fade: fadeDimOn}
	ledsOff       = ledPattern{channels: nil, fade: fadeSmoothOff}
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

const (
	resultRebootTriggered = "reboot-triggered"
	resultTimeout         = "timeout"
	resultInstallError    = "install-error"
	resultVehicleState    = "vehicle-state"
	resultError           = "error"
)

func (s *Service) setResult(result, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	if err := s.publisher.SetMany(map[string]any{
		"last-result":        result,
		"last-result-detail": detail,
		"last-result-time":   time.Now().Format(time.RFC3339),
	}, ipc.Sync()); err != nil {
		log.Printf("Error publishing usb result %q: %v", result, err)
	}
	log.Printf("UMS cycle result: %s (%s)", result, detail)
}

func (s *Service) clearResult() {
	if err := s.publisher.SetMany(map[string]any{
		"last-result":        "",
		"last-result-detail": "",
		"last-result-time":   "",
	}, ipc.Sync()); err != nil {
		log.Printf("Error clearing usb result: %v", err)
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
func (s *Service) runPostCycleCleanup() {
	if err := s.logBundlesMgr.PruneOldBundles(logBundleKeepCount); err != nil {
		log.Printf("Warning: failed to prune old log bundles: %v", err)
	}
	if err := s.updateLdr.CleanupStaleFiles(); err != nil {
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
