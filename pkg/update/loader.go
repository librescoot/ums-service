package update

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/ums-service/pkg/dbc"
	"github.com/librescoot/ums-service/pkg/umslog"
)

// dbcUpdater is the slice of the DBC interface the OTA loader uses. Production
// passes *dbc.Interface; tests substitute a fake so the multi-file transfer
// order and the single MarkDBCUpdateQueued handoff are exercised.
type dbcUpdater interface {
	IsEnabled() bool
	RunCommand(ctx context.Context, command string) (string, error)
	TransferFile(ctx context.Context, localPath, remotePath string, progressCb dbc.ProgressFunc) error
	MarkDBCUpdateQueued()
}

type Loader struct {
	otaRootDir   string
	otaDir       string
	dbcOtaDir    string
	managedDirs  []managedDir
	client       *ipc.Client
	dbcInterface dbcUpdater
}

// managedDir is a subdirectory under /data/ota that ums-service keeps update
// artifacts in. `keep` is the number of most-recent versions to retain per
// (channel) group during cleanup.
//
// The otaDir and dbcOtaDir entries are load-bearing even though their pruning
// is skipped: managedDirs is also the orphan-sweep allowlist
// (removeOrphanedUpdateFiles deletes any update file whose immediate parent is
// not listed here), and the staged update artifacts live directly in those
// two dirs. Removing them would make the next CleanupStaleFiles delete a
// staged chain out from under an in-flight install. Their `keep` value alone
// is inert.
type managedDir struct {
	path string
	keep int
}

// Refusal is one board's staged update that was refused before anything was
// copied or transferred. The caller surfaces it to the user (usb.last-result
// and the dashboard notification channel) as well as the ums log.
type Refusal struct {
	Board  string // "mdb" or "dbc"
	Reason string
}

// Queued summarizes what ProcessUpdates copied/transferred and what still
// needs to be pushed to Redis to trigger the install. The caller is
// responsible for subscribing to the ota hash BEFORE performing
// PendingPushes, so the awaiter doesn't miss the install→pending-reboot
// transition.
//
// MDB and DBC indicate whether the respective artifact was staged
// (file copied or transferred to its target); they do not reflect
// whether the LPush in PendingPushes completed. Refused lists boards that
// were skipped without staging anything.
type Queued struct {
	MDB           bool
	DBC           bool
	PendingPushes []PendingPush
	Refused       []Refusal
}

// PendingPush is an LPush operation deferred so the caller can subscribe
// to status changes first.
type PendingPush struct {
	Channel string
	Value   string
}

func New(client *ipc.Client, dbcInterface dbcUpdater) *Loader {
	otaDir := "/data/ota/mdb"
	dbcOtaDir := "/data/ota/dbc"
	return &Loader{
		otaRootDir: "/data/ota",
		otaDir:     otaDir,
		dbcOtaDir:  dbcOtaDir,
		managedDirs: []managedDir{
			{otaDir, 1},
			{dbcOtaDir, 1},
			{"/data/ota/mdb-boot", 5},
			{"/data/ota/dbc-boot", 5},
		},
		client:       client,
		dbcInterface: dbcInterface,
	}
}

// CleanupStaleFiles removes orphaned update artifacts under /data/ota:
//   - Any *.mender or *.delta file NOT inside one of the managed subdirs is removed.
//   - Inside the boot subdirs (mdb-boot, dbc-boot) only the newest N versions
//     per channel group are kept.
//
// /data/ota/mdb and /data/ota/dbc are owned by update-service: they hold the
// previously-installed .mender (delta base) and any in-flight download.
// ums-service must not prune them — update-service has its own retention
// policy and a concurrent prune here would race with downloads/installs and
// can delete the delta base out from under it.
//
// Version comparison is semver-aware for v-prefixed versions (e.g. v0.10.0 >
// v0.7.0); otherwise lexicographic, which works for ISO timestamps used by the
// nightly/testing channels.
func (l *Loader) CleanupStaleFiles() error {
	if _, err := os.Stat(l.otaRootDir); os.IsNotExist(err) {
		return nil
	}

	if err := l.removeOrphanedUpdateFiles(); err != nil {
		log.Printf("ota cleanup: orphan sweep failed: %v", err)
	}

	skipPrune := map[string]bool{
		filepath.Clean(l.otaDir):    true,
		filepath.Clean(l.dbcOtaDir): true,
	}
	for _, md := range l.managedDirs {
		if skipPrune[filepath.Clean(md.path)] {
			continue
		}
		if err := l.pruneOldVersions(md.path, md.keep); err != nil {
			log.Printf("ota cleanup: pruning %s failed: %v", md.path, err)
		}
	}
	return nil
}

func (l *Loader) removeOrphanedUpdateFiles() error {
	allowedDirs := make(map[string]bool, len(l.managedDirs))
	for _, d := range l.managedDirs {
		allowedDirs[filepath.Clean(d.path)] = true
	}

	return filepath.Walk(l.otaRootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !isUpdateFile(info.Name()) {
			return nil
		}
		if allowedDirs[filepath.Clean(filepath.Dir(path))] {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil {
			log.Printf("ota cleanup: failed to remove orphaned %s: %v", path, rmErr)
			return nil
		}
		log.Printf("ota cleanup: removed orphaned update file %s", path)
		return nil
	})
}

func (l *Loader) pruneOldVersions(dir string, keep int) error {
	if keep < 1 {
		keep = 1
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	groups := make(map[string][]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isUpdateFile(name) {
			continue
		}
		key, _ := splitVersion(name)
		groups[key] = append(groups[key], name)
	}

	for _, files := range groups {
		if len(files) <= keep {
			continue
		}
		sort.Slice(files, func(i, j int) bool {
			_, vi := splitVersion(files[i])
			_, vj := splitVersion(files[j])
			return compareVersions(vi, vj) < 0
		})
		// Keep `keep` newest (tail of the sorted slice); remove the rest.
		for _, old := range files[:len(files)-keep] {
			path := filepath.Join(dir, old)
			if err := os.Remove(path); err != nil {
				log.Printf("ota cleanup: failed to remove old %s: %v", path, err)
				continue
			}
			log.Printf("ota cleanup: removed old update %s", path)
		}
	}
	return nil
}

func isUpdateFile(name string) bool {
	return strings.HasSuffix(name, ".mender") || strings.HasSuffix(name, ".delta")
}

func updateArtifactTarget(name string) string {
	if !strings.HasPrefix(name, "librescoot-") || !isUpdateFile(name) {
		return ""
	}
	if strings.Contains(name, "-mdb") {
		return "mdb"
	}
	if strings.Contains(name, "-dbc") {
		return "dbc"
	}
	return ""
}

// IsDBCUpdateArtifact reports whether name is a supported DBC update artifact.
// Exported so DBC preflight and update processing use the same filename rules.
func IsDBCUpdateArtifact(name string) bool {
	return updateArtifactTarget(name) == "dbc"
}

// splitVersion splits "librescoot-foo-mdb-nightly-20260429T102607.mender" into
// ("librescoot-foo-mdb-nightly", "20260429T102607"). The version token is the
// segment after the last '-' (extension stripped).
func splitVersion(filename string) (key, version string) {
	base := filename
	for _, ext := range []string{".mender", ".delta"} {
		if trimmed, ok := strings.CutSuffix(base, ext); ok {
			base = trimmed
			break
		}
	}
	idx := strings.LastIndex(base, "-")
	if idx < 0 {
		return base, ""
	}
	return base[:idx], base[idx+1:]
}

// compareVersions returns -1, 0, or 1. Treats v-prefixed dotted versions as
// semver, everything else as lexicographic (works for ISO timestamps).
func compareVersions(a, b string) int {
	aSemver, aParts := parseSemver(a)
	bSemver, bParts := parseSemver(b)
	if aSemver && bSemver {
		for i := range 3 {
			if aParts[i] != bParts[i] {
				if aParts[i] < bParts[i] {
					return -1
				}
				return 1
			}
		}
		return 0
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func parseSemver(v string) (bool, [3]int) {
	var out [3]int
	if !strings.HasPrefix(v, "v") {
		return false, out
	}
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return false, out
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return false, out
		}
		out[i] = n
	}
	return true, out
}

// PrepareUSB creates the system-update directory on the exported drive and,
// when discardStale is set, discards any update artifacts a previous cycle left
// there. The exit path's CleanDrive() wipes the drive after a completed cycle,
// so a file that survives to the next entry is the residue of a cycle that died
// before it could exit; leaving it would present it to the host and import it
// again on the next exit. discardStale is false when re-entering UMS from an
// already-exporting mode (ums -> ums-by-dbc): the drive was never handed back,
// so host-written files that have not been imported yet must not be swept.
// This only touches the on-drive directory: /data/ota staging and
// update-service's delta base are deliberately untouched.
func (l *Loader) PrepareUSB(ctx context.Context, usbMountPath string, discardStale bool, logger *umslog.Logger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	updateDir := filepath.Join(usbMountPath, "system-update")
	if err := os.MkdirAll(updateDir, 0755); err != nil {
		return fmt.Errorf("failed to create system-update directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if discardStale {
		discarded, err := discardStaleUpdateFiles(ctx, updateDir)
		if err != nil {
			return fmt.Errorf("failed to inspect system-update directory: %w", err)
		}
		if discarded > 0 {
			log.Printf("Discarded %d stale update file(s) left in %s by a previous cycle", discarded, updateDir)
			if logger != nil {
				logger.Logf("updates", "discarded stale update files left by a previous cycle (%d file(s))", discarded)
			}
		}
	}
	log.Println("Created system-update directory on USB drive")
	return nil
}

// discardStaleUpdateFiles removes the update artifacts left on the exported
// drive and reports how many it removed. Only files UMS would itself treat as
// artifacts (librescoot-*-{mdb,dbc}.mender/.delta, via updateArtifactTarget)
// are removed; a user's unrelated notes.delta or other file is left alone.
func discardStaleUpdateFiles(ctx context.Context, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	discarded := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return discarded, err
		}
		if entry.IsDir() || updateArtifactTarget(entry.Name()) == "" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := os.Remove(path); err != nil {
			log.Printf("Warning: failed to discard stale update file %s: %v", path, err)
			continue
		}
		discarded++
		log.Printf("Discarded stale update file %s", path)
	}
	return discarded, nil
}

// stagedUpdateCommand is the path-free command UMS pushes for a staged board.
// update-service discovers the artifacts in its own component download dir and
// resolves what to install, so the command carries no paths and there is no
// separator to parse.
const stagedUpdateCommand = "apply-staged-updates"

func (l *Loader) ProcessUpdates(ctx context.Context, perFileTimeout time.Duration, logger *umslog.Logger, usbMountPath string) (Queued, error) {
	var queued Queued
	updateDir := filepath.Join(usbMountPath, "system-update")

	entries, err := os.ReadDir(updateDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("No system-update directory found")
			return queued, nil
		}
		return queued, fmt.Errorf("failed to read update directory: %w", err)
	}

	byBoard := make(map[string][]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		target := updateArtifactTarget(entry.Name())
		if target == "" {
			continue
		}
		byBoard[target] = append(byBoard[target], filepath.Join(updateDir, entry.Name()))
	}

	// MDB before DBC, so the order is deterministic regardless of how the
	// directory happens to iterate.
	for _, target := range []string{"mdb", "dbc"} {
		paths := byBoard[target]
		if len(paths) == 0 {
			continue
		}

		plan, reason := planBoardUpdate(target, paths, logger)
		if reason != "" {
			queued.Refused = append(queued.Refused, Refusal{Board: target, Reason: reason})
			continue
		}

		var push PendingPush
		var procErr error
		switch target {
		case "mdb":
			push, procErr = l.processMDBUpdate(logger, plan)
		case "dbc":
			push, procErr = l.processDBCUpdate(ctx, perFileTimeout, logger, plan)
		}
		if procErr != nil {
			return queued, fmt.Errorf("failed to process %s update: %w", strings.ToUpper(target), procErr)
		}
		if target == "mdb" {
			queued.MDB = true
		} else {
			queued.DBC = true
		}
		queued.PendingPushes = append(queued.PendingPushes, push)
	}

	return queued, nil
}

// planBoardUpdate decides whether one board's files form a drop UMS may stage,
// returning the files to copy/transfer. reason is non-empty when the board is
// conflicted and must be skipped without acting on any of its files:
//
//   - a full image together with one or more deltas — the board cannot be both
//     full-updated and delta-updated in one drop;
//   - more than one full image — it is ambiguous which one to install;
//   - deltas that span more than one channel/key — a chain cannot cross
//     channels, and ordering across them is meaningless.
//
// The order of the returned paths does not matter: update-service resolves a
// delta chain from the deltas' own metadata, not from the command.
func planBoardUpdate(target string, paths []string, logger *umslog.Logger) (plan []string, reason string) {
	var menders, deltas []string
	for _, p := range paths {
		if strings.HasSuffix(p, ".mender") {
			menders = append(menders, p)
		} else {
			deltas = append(deltas, p)
		}
	}

	refuse := func(format string, args ...any) ([]string, string) {
		msg := fmt.Sprintf(format, args...)
		log.Printf("update: refusing %s update: %s - skipping this board", target, msg)
		if logger != nil {
			logger.Error("updates", "refusing %s update: %s - skipping this board", target, msg)
		}
		return nil, msg
	}

	switch {
	case len(menders) > 0 && len(deltas) > 0:
		return refuse("%d full image(s) and %d delta(s) staged together; it is ambiguous whether to full-update or delta-update",
			len(menders), len(deltas))
	case len(menders) > 1:
		return refuse("%d full images staged for one board; it is ambiguous which one to install", len(menders))
	case len(menders) == 1:
		return menders, ""
	case len(deltas) == 1:
		return deltas, ""
	}

	// Deltas only, and at least two: they must be one chain, so one channel/key.
	keys := make(map[string]bool, len(deltas))
	for _, d := range deltas {
		key, _ := splitVersion(filepath.Base(d))
		keys[key] = true
	}
	if len(keys) > 1 {
		return refuse("%d deltas span %d channels/keys; a chain must stay on one", len(deltas), len(keys))
	}
	return deltas, ""
}

func (l *Loader) processMDBUpdate(logger *umslog.Logger, srcPaths []string) (PendingPush, error) {
	if err := os.MkdirAll(l.otaDir, 0755); err != nil {
		return PendingPush{}, fmt.Errorf("failed to create OTA directory: %w", err)
	}

	for _, srcPath := range srcPaths {
		filename := filepath.Base(srcPath)
		log.Printf("Processing MDB update: %s", filename)
		if logger != nil {
			logger.Logf("updates", "copying MDB update %s", filename)
		}

		dstPath := filepath.Join(l.otaDir, filename)

		// Copy instead of rename — source is on vfat, destination on ext4
		if err := copyFile(srcPath, dstPath); err != nil {
			return PendingPush{}, fmt.Errorf("failed to copy update file: %w", err)
		}

		log.Printf("Successfully staged MDB update: %s", filename)
		if logger != nil {
			logger.Logf("updates", "staged MDB update %s -> %s", filename, dstPath)
		}
	}

	return PendingPush{
		Channel: "scooter:update:mdb",
		Value:   stagedUpdateCommand,
	}, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	return out.Sync()
}

func (l *Loader) processDBCUpdate(ctx context.Context, timeout time.Duration, logger *umslog.Logger, srcPaths []string) (PendingPush, error) {
	if !l.dbcInterface.IsEnabled() {
		return PendingPush{}, fmt.Errorf("DBC interface not enabled for update")
	}

	mkdirCtx, cancelMkdir := context.WithTimeout(ctx, timeout)
	_, mkdirErr := l.dbcInterface.RunCommand(mkdirCtx, fmt.Sprintf("mkdir -p %s", l.dbcOtaDir))
	cancelMkdir()
	if mkdirErr != nil {
		return PendingPush{}, fmt.Errorf("failed to create remote OTA directory: %w", mkdirErr)
	}

	for _, srcPath := range srcPaths {
		filename := filepath.Base(srcPath)
		log.Printf("Processing DBC update: %s", filename)

		// One timeout per file: a chain is several transfers, not one long one.
		fileCtx, cancel := context.WithTimeout(ctx, timeout)
		var progress dbc.ProgressFunc
		if logger != nil {
			progress = logger.ProgressCallback(filename)
		}
		remotePath := filepath.Join(l.dbcOtaDir, filename)
		transferErr := l.dbcInterface.TransferFile(fileCtx, srcPath, remotePath, progress)
		cancel()
		if logger != nil {
			logger.ClearProgress()
		}
		if transferErr != nil {
			return PendingPush{}, fmt.Errorf("failed to transfer update to DBC: %w", transferErr)
		}

		log.Printf("Copied DBC update to %s", remotePath)
		log.Printf("Successfully staged DBC update: %s", filename)
		if logger != nil {
			logger.Logf("updates", "staged DBC update %s -> %s", filename, remotePath)
		}
	}

	// Tell the dbc.Interface to leave the vehicle-service update lock
	// held after Disable(). update-service runs the actual mender
	// installation asynchronously from here and owns its own
	// start-dbc / complete-dbc cycle around that install. Releasing
	// now would drop the lock mid-handoff and let the FSM cut DBC
	// power before the installation finishes.
	l.dbcInterface.MarkDBCUpdateQueued()

	return PendingPush{
		Channel: "scooter:update:dbc",
		Value:   stagedUpdateCommand,
	}, nil
}
