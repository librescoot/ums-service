package dbc

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

// heartbeatInterval controls how often we re-publish start-dbc to keep
// vehicle-service's watchdog fed. Must be comfortably less than the
// vehicle-service `dbcUpdateTimeout` (currently 15 min on production,
// 45 min in local source). 5 min gives three heartbeats per watchdog
// window and leaves slack for transient redis delays.
const heartbeatInterval = 5 * time.Minute

// uploadServerKind identifies which variant of HTTP PUT endpoint is
// running on the DBC for a given Enable() cycle.
type uploadServerKind int

const (
	// uploadServerNone means the HTTP PUT path is unavailable — callers
	// will fall through to SCP. Either startup failed or no python3 is
	// present.
	uploadServerNone uploadServerKind = iota
	// uploadServerBootstrapped is the ums-service's own ephemeral Python
	// server written to /tmp/upload_srv.py and launched over SSH. Accepts
	// PUT at absolute paths (PUT /data/maps/foo writes /data/maps/foo).
	uploadServerBootstrapped
	// uploadServerDataServer is librescoot-data-server running as a
	// long-lived service on the DBC (newer LibreScoot images). Accepts
	// PUT at paths relative to its -data dir (PUT /maps/foo writes
	// <dataDir>/maps/foo). Identified via the `Server:` response header.
	uploadServerDataServer
)

type Interface struct {
	ip               string
	port             int
	dataDir          string
	httpServer       *http.Server
	enabled          bool
	client           *ipc.Client
	uploadServerKind uploadServerKind
	heartbeatCancel  context.CancelFunc
	heartbeatDone    chan struct{}
}

func New(dataDir string, client *ipc.Client) *Interface {
	return &Interface{
		ip:      "192.168.7.2",
		port:    31337,
		dataDir: dataDir,
		client:  client,
		enabled: false,
	}
}

func (i *Interface) Enable(ctx context.Context) error {
	if i.enabled {
		return nil
	}

	log.Println("Enabling DBC interface...")

	// `start-dbc` tells vehicle-service to claim the DBC update lock:
	// set dbcUpdating=true, arm a safety watchdog, install the
	// suspend-only inhibitor, and force dashboard_power on. Any
	// dashboard:off request from the FSM (standby, lock, hibernate) is
	// then either rejected or deferred until we send complete-dbc. See
	// vehicle-service/internal/core/redis_handlers.go:167-209.
	if _, err := i.client.LPush("scooter:update", "start-dbc"); err != nil {
		return fmt.Errorf("failed to claim DBC update lock: %w", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	timeout := time.After(60 * time.Second)

	for {
		select {
		case <-ctx.Done():
			// Release the lock even on cancellation — we never got to
			// enabled=true, so our own Disable() won't be called.
			i.releaseUpdateLock()
			return ctx.Err()
		case <-timeout:
			i.releaseUpdateLock()
			return fmt.Errorf("timeout waiting for DBC to become reachable")
		case <-ticker.C:
			if i.isReachable() {
				i.enabled = true
				log.Println("DBC is now reachable")
				if err := i.startHTTPServer(); err != nil {
					i.releaseUpdateLock()
					i.enabled = false
					return err
				}
				if err := i.startUploadServer(ctx); err != nil {
					log.Printf("DBC upload server failed to start, uploads will fall back to SCP: %v", err)
				}
				i.startHeartbeat()
				return nil
			}
		}
	}
}

// startHeartbeat launches a goroutine that re-sends start-dbc every
// heartbeatInterval so vehicle-service's watchdog gets fed during long
// operations. Safe because start-dbc is idempotent — same handler just
// resets the timer and re-arms the inhibitor.
func (i *Interface) startHeartbeat() {
	hbCtx, cancel := context.WithCancel(context.Background())
	i.heartbeatCancel = cancel
	i.heartbeatDone = make(chan struct{})
	go func() {
		defer close(i.heartbeatDone)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if _, err := i.client.LPush("scooter:update", "start-dbc"); err != nil {
					log.Printf("heartbeat: failed to refresh DBC update lock: %v", err)
				} else {
					log.Printf("heartbeat: refreshed DBC update lock")
				}
			}
		}
	}()
}

// stopHeartbeat cancels the heartbeat goroutine and waits for it to
// exit. Must run before releaseUpdateLock so a racing heartbeat can't
// re-claim the lock after we've already released it.
func (i *Interface) stopHeartbeat() {
	if i.heartbeatCancel == nil {
		return
	}
	i.heartbeatCancel()
	<-i.heartbeatDone
	i.heartbeatCancel = nil
	i.heartbeatDone = nil
}

// releaseUpdateLock sends complete-dbc so vehicle-service clears
// dbcUpdating, removes the suspend-only inhibitor, and applies any
// deferred dashboard power changes. Safe to call multiple times.
func (i *Interface) releaseUpdateLock() {
	if _, err := i.client.LPush("scooter:update", "complete-dbc"); err != nil {
		log.Printf("Failed to release DBC update lock: %v", err)
	}
}

func (i *Interface) Disable() error {
	if !i.enabled {
		return nil
	}

	log.Println("Disabling DBC interface...")

	// Stop the heartbeat FIRST, then release the lock. Reversing the
	// order would race: a heartbeat tick between releaseUpdateLock and
	// stopHeartbeat could re-claim the DBC update lock after we told
	// vehicle-service we were done.
	i.stopHeartbeat()

	// Release the update lock so any follow-up dashboard:off (or
	// deferred FSM transitions) are allowed to proceed. Deferred
	// behavior matters: if the user locked the scooter mid-update,
	// vehicle-service parked deferredDashboardPower and complete-dbc
	// is what finally cuts power via the FSM standby path.
	defer i.releaseUpdateLock()

	i.stopUploadServer()

	if i.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := i.httpServer.Shutdown(ctx); err != nil {
			log.Printf("Error shutting down HTTP server: %v", err)
		}
		i.httpServer = nil
	}

	i.enabled = false
	return nil
}

func (i *Interface) isReachable() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:22", i.ip), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (i *Interface) startHTTPServer() error {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(i.dataDir)))

	i.httpServer = &http.Server{
		Addr:    fmt.Sprintf("192.168.7.1:%d", i.port),
		Handler: mux,
	}

	go func() {
		log.Printf("Starting HTTP server on port %d serving %s", i.port, i.dataDir)
		if err := i.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	return nil
}

func (i *Interface) DownloadFile(ctx context.Context, localPath, remotePath string) error {
	if !i.enabled {
		return fmt.Errorf("DBC interface not enabled")
	}

	filename := filepath.Base(localPath)
	url := fmt.Sprintf("http://192.168.7.1:%d/%s", i.port, filename)

	// `-y` on dbclient auto-accepts unknown host keys. The scooter's
	// ssh is dropbear, which doesn't understand OpenSSH's `-o Strict...`
	// options and prints a warning for each one in journald.
	cmd := exec.CommandContext(ctx, "ssh",
		"-y",
		fmt.Sprintf("root@%s", i.ip),
		fmt.Sprintf("wget -O %s %s", remotePath, url))

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to download file via SSH: %v, output: %s", err, string(output))
	}

	log.Printf("Downloaded %s to DBC at %s", filename, remotePath)
	return nil
}

func (i *Interface) CopyFile(ctx context.Context, localPath, remotePath string) error {
	if !i.enabled {
		return fmt.Errorf("DBC interface not enabled")
	}

	cmd := exec.CommandContext(ctx, "scp",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		localPath,
		fmt.Sprintf("root@%s:%s", i.ip, remotePath))

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to copy file: %v, output: %s", err, string(output))
	}

	log.Printf("Copied %s to DBC at %s", localPath, remotePath)
	return nil
}

func (i *Interface) RunCommand(ctx context.Context, command string) (string, error) {
	if !i.enabled {
		return "", fmt.Errorf("DBC interface not enabled")
	}

	cmd := exec.CommandContext(ctx, "ssh",
		"-y",
		fmt.Sprintf("root@%s", i.ip),
		command)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run command: %v, output: %s", err, string(output))
	}

	return strings.TrimSpace(string(output)), nil
}

func (i *Interface) IsEnabled() bool {
	return i.enabled
}
