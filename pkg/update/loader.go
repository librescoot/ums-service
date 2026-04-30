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

type Loader struct {
	otaRootDir   string
	otaDir       string
	dbcOtaDir    string
	managedDirs  []managedDir
	client       *ipc.Client
	dbcInterface *dbc.Interface
}

// managedDir is a subdirectory under /data/ota that ums-service is allowed to
// keep update artifacts in. `keep` is the number of most-recent versions to
// retain per (channel) group during cleanup.
type managedDir struct {
	path string
	keep int
}

func New(client *ipc.Client, dbcInterface *dbc.Interface) *Loader {
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

func (l *Loader) PrepareUSB(usbMountPath string) error {
	updateDir := filepath.Join(usbMountPath, "system-update")
	if err := os.MkdirAll(updateDir, 0755); err != nil {
		return fmt.Errorf("failed to create system-update directory: %w", err)
	}
	log.Println("Created system-update directory on USB drive")
	return nil
}

func (l *Loader) ProcessUpdates(ctx context.Context, perFileTimeout time.Duration, logger *umslog.Logger, usbMountPath string) error {
	updateDir := filepath.Join(usbMountPath, "system-update")

	entries, err := os.ReadDir(updateDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("No system-update directory found")
			return nil
		}
		return fmt.Errorf("failed to read update directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()
		if !strings.HasPrefix(filename, "librescoot-") || (!strings.HasSuffix(filename, ".mender") && !strings.HasSuffix(filename, ".delta")) {
			continue
		}

		srcPath := filepath.Join(updateDir, filename)

		if strings.Contains(filename, "-mdb") {
			if err := l.processMDBUpdate(logger, srcPath); err != nil {
				return fmt.Errorf("failed to process MDB update: %w", err)
			}
		} else if strings.Contains(filename, "-dbc") {
			if err := l.processDBCUpdate(ctx, perFileTimeout, logger, srcPath); err != nil {
				return fmt.Errorf("failed to process DBC update: %w", err)
			}
		}
	}

	return nil
}

func (l *Loader) processMDBUpdate(logger *umslog.Logger, srcPath string) error {
	filename := filepath.Base(srcPath)
	log.Printf("Processing MDB update: %s", filename)
	if logger != nil {
		logger.Logf("updates", "copying MDB update %s", filename)
	}

	if err := os.MkdirAll(l.otaDir, 0755); err != nil {
		return fmt.Errorf("failed to create OTA directory: %w", err)
	}

	dstPath := filepath.Join(l.otaDir, filename)

	// Copy instead of rename — source is on vfat, destination on ext4
	if err := copyFile(srcPath, dstPath); err != nil {
		return fmt.Errorf("failed to copy update file: %w", err)
	}

	_, err := l.client.LPush("scooter:update:mdb", fmt.Sprintf("update-from-file:%s", dstPath))
	if err != nil {
		return fmt.Errorf("failed to notify update service: %w", err)
	}

	log.Printf("Successfully queued MDB update: %s", filename)
	if logger != nil {
		logger.Logf("updates", "queued MDB update %s -> %s", filename, dstPath)
	}
	return nil
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

func (l *Loader) processDBCUpdate(ctx context.Context, timeout time.Duration, logger *umslog.Logger, srcPath string) error {
	filename := filepath.Base(srcPath)
	log.Printf("Processing DBC update: %s", filename)

	if !l.dbcInterface.IsEnabled() {
		return fmt.Errorf("DBC interface not enabled for update")
	}

	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	remotePath := filepath.Join(l.dbcOtaDir, filename)

	if _, err := l.dbcInterface.RunCommand(opCtx, fmt.Sprintf("mkdir -p %s", l.dbcOtaDir)); err != nil {
		return fmt.Errorf("failed to create remote OTA directory: %w", err)
	}

	var progress dbc.ProgressFunc
	if logger != nil {
		progress = logger.ProgressCallback(filename)
		defer logger.ClearProgress()
	}
	if err := l.dbcInterface.TransferFile(opCtx, srcPath, remotePath, progress); err != nil {
		return fmt.Errorf("failed to transfer update to DBC: %w", err)
	}

	log.Printf("Copied DBC update to %s", remotePath)

	_, err := l.client.LPush("scooter:update:dbc", fmt.Sprintf("update-from-file:%s", remotePath))
	if err != nil {
		return fmt.Errorf("failed to notify update service: %w", err)
	}

	// Tell the dbc.Interface to leave the vehicle-service update lock
	// held after Disable(). update-service runs the actual mender
	// installation asynchronously from here and owns its own
	// start-dbc / complete-dbc cycle around that install. Releasing
	// now would drop the lock mid-handoff and let the FSM cut DBC
	// power before the installation finishes.
	l.dbcInterface.MarkDBCUpdateQueued()

	log.Printf("Successfully queued DBC update: %s", filename)
	if logger != nil {
		logger.Logf("updates", "queued DBC update %s -> %s", filename, remotePath)
	}
	return nil
}
