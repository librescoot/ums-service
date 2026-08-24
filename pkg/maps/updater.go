package maps

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/ums-service/pkg/dbc"
	"github.com/librescoot/ums-service/pkg/umslog"
)

type Updater struct {
	dbcMapsDir     string
	dbcValhallaDir string
	dbcInterface   *dbc.Interface
	// Used to mirror what was installed into the `maps` hash. May be nil, in
	// which case only the DBC's metadata.json is updated.
	client *ipc.Client
}

// isCompressedTilesArchive reports whether the file is the zstd-compressed
// form. The DBC does the decompression: Go's zstd decoder has no armv7
// assembly and manages about 5.8 MB/s here, against roughly 20 MB/s for C
// libzstd on the DBC, which also keeps the transfer three times smaller.
func isCompressedTilesArchive(filename string) bool {
	return strings.HasSuffix(filename, ".tar.zst")
}

// IsValhallaTilesArchive reports whether a file on the USB drive is a routing
// tile archive this service knows how to install, in either the plain or the
// zstd-compressed form.
//
// Exported because the service also has to answer "does this stick need the
// DBC powered up?" before it gets anywhere near ProcessMaps. That check used to
// carry its own copy of the predicate, drifted, and stopped matching .tar.zst,
// which left the DBC off and made the whole update a silent no-op.
func IsValhallaTilesArchive(filename string) bool {
	base := strings.TrimSuffix(filename, ".zst")
	return strings.HasSuffix(base, "tiles.tar") ||
		(strings.HasPrefix(base, "valhalla_tiles_") && strings.HasSuffix(base, ".tar"))
}

func New(dbcInterface *dbc.Interface, client *ipc.Client) *Updater {
	return &Updater{
		dbcMapsDir:     "/data/maps",
		dbcValhallaDir: "/data/valhalla",
		dbcInterface:   dbcInterface,
		client:         client,
	}
}

func (u *Updater) PrepareUSB(usbMountPath string) error {
	mapsDir := filepath.Join(usbMountPath, "maps")
	if err := os.MkdirAll(mapsDir, 0755); err != nil {
		return fmt.Errorf("failed to create maps directory: %w", err)
	}
	log.Println("Created maps directory on USB drive")
	return nil
}

// ProcessMaps scans the USB drive for map files and uploads them to the
// DBC. The supplied context bounds the **entire** map processing phase;
// per-file transfers run under child contexts derived from perFileTimeout
// so one slow file can't starve later ones. If logger is non-nil, upload
// progress is published to the `usb` hash for the UI.
func (u *Updater) ProcessMaps(ctx context.Context, perFileTimeout time.Duration, logger *umslog.Logger, usbMountPath string) error {
	mapsDir := filepath.Join(usbMountPath, "maps")

	entries, err := os.ReadDir(mapsDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("No maps directory found")
			return nil
		}
		return fmt.Errorf("failed to read maps directory: %w", err)
	}

	if !u.dbcInterface.IsEnabled() {
		return fmt.Errorf("DBC interface not enabled for map updates")
	}

	var mbtilesFile, tilesFile string

	// Find map files
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()
		if strings.HasSuffix(filename, ".mbtiles") {
			mbtilesFile = filepath.Join(mapsDir, filename)
		} else if IsValhallaTilesArchive(filename) {
			tilesFile = filepath.Join(mapsDir, filename)
		}
	}

	if mbtilesFile != "" {
		if err := u.processMBTiles(ctx, perFileTimeout, logger, mbtilesFile); err != nil {
			return fmt.Errorf("failed to process mbtiles: %w", err)
		}
	}

	if tilesFile != "" {
		if err := u.processTilesTar(ctx, perFileTimeout, logger, tilesFile); err != nil {
			return fmt.Errorf("failed to process tiles.tar: %w", err)
		}
	}

	if mbtilesFile == "" && tilesFile == "" {
		log.Println("No map files found to process")
	}

	return nil
}

func (u *Updater) processMBTiles(ctx context.Context, timeout time.Duration, logger *umslog.Logger, localPath string) error {
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := u.dbcInterface.RunCommand(opCtx, fmt.Sprintf("mkdir -p %s", u.dbcMapsDir)); err != nil {
		return fmt.Errorf("failed to create remote maps directory: %w", err)
	}

	remotePath := filepath.Join(u.dbcMapsDir, "map.mbtiles")

	var progress dbc.ProgressFunc
	if logger != nil {
		progress = logger.ProgressCallback("map.mbtiles")
		defer logger.ClearProgress()
	}
	if err := u.dbcInterface.TransferFile(opCtx, localPath, remotePath, progress); err != nil {
		return fmt.Errorf("failed to transfer mbtiles to DBC: %w", err)
	}

	log.Printf("Successfully copied mbtiles to DBC at %s", remotePath)

	// Bookkeeping runs on the parent context, not opCtx: the transfer may have
	// used most of the per-file budget, and recording what landed is worth a
	// few more seconds even when it did.
	u.recordInstall(ctx, true, filepath.Base(localPath), remotePath)
	return nil
}

// cleanupRemoteTimeout bounds the best-effort tidy-up after a failed map
// transfer. The paths being removed are leftovers, so giving up on them is
// cheap; blocking the whole UMS cycle on a DBC that has stopped answering is
// not. RunCommand shells out to ssh without a ConnectTimeout of its own, so
// without a deadline here the cleanup can hang indefinitely.
const cleanupRemoteTimeout = 30 * time.Second

// cleanupRemote removes leftover files from the DBC on a failure path. It
// deliberately detaches from opCtx's cancellation, because the usual reason to
// be here is that opCtx just expired, but keeps a short deadline of its own.
func (u *Updater) cleanupRemote(opCtx context.Context, paths ...string) {
	if len(paths) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(opCtx), cleanupRemoteTimeout)
	defer cancel()

	cmd := "rm -f " + strings.Join(paths, " ")
	if _, err := u.dbcInterface.RunCommand(cleanupCtx, cmd); err != nil {
		log.Printf("Could not remove %s from DBC after a failed map transfer: %v",
			strings.Join(paths, ", "), err)
	}
}

func (u *Updater) processTilesTar(ctx context.Context, timeout time.Duration, logger *umslog.Logger, localPath string) error {
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := u.dbcInterface.RunCommand(opCtx, fmt.Sprintf("mkdir -p %s", u.dbcValhallaDir)); err != nil {
		return fmt.Errorf("failed to create remote valhalla directory: %w", err)
	}

	remotePath := filepath.Join(u.dbcValhallaDir, "tiles.tar")
	uploadPath := remotePath
	compressed := isCompressedTilesArchive(localPath)
	if compressed {
		uploadPath = remotePath + ".zst"
	}

	// Before the upload, not after: the transfer is the slow part, and finding
	// out that /data was never going to hold the result is worth nothing once
	// the user has already waited it out.
	if err := u.checkDBCSpace(opCtx, localPath, compressed); err != nil {
		return err
	}

	var progress dbc.ProgressFunc
	if logger != nil {
		progress = logger.ProgressCallback(filepath.Base(uploadPath))
		defer logger.ClearProgress()
	}
	if err := u.dbcInterface.TransferFile(opCtx, localPath, uploadPath, progress); err != nil {
		// Whatever made it across is still sitting on the DBC, up to several
		// hundred MB of /data holding a file nothing will ever read. Clear it
		// the same way the decompress failure below does.
		u.cleanupRemote(opCtx, uploadPath)
		return fmt.Errorf("failed to transfer tiles archive to DBC: %w", err)
	}

	if compressed {
		// valhalla mmaps tiles.tar as its tile_extract, so the installed file
		// has to be the plain seekable tar. Decompress into a temp file and
		// only move it over remotePath once zstd has confirmed the whole
		// stream decoded cleanly: `zstd -f` writes destructively, so
		// decompressing straight into remotePath would let a failure partway
		// through (disk full, or a frame checksum mismatch that only
		// surfaces at end-of-stream) clobber a tiles.tar that was already
		// installed and working.
		tmpPath := remotePath + ".tmp"
		installCmd := fmt.Sprintf("zstd -d -f -o %s %s && mv -f %s %s", tmpPath, uploadPath, tmpPath, remotePath)
		if _, err := u.dbcInterface.RunCommand(opCtx, installCmd); err != nil {
			// Best effort: whichever stage failed, remotePath was never
			// touched, so just clear the temp decompress target and the
			// upload rather than leaving them occupying /data.
			u.cleanupRemote(opCtx, tmpPath, uploadPath)
			return fmt.Errorf("failed to decompress tiles archive on DBC: %w", err)
		}
		if _, err := u.dbcInterface.RunCommand(opCtx, "rm -f "+uploadPath); err != nil {
			log.Printf("Could not remove %s from DBC after decompress: %v", uploadPath, err)
		}
	}

	log.Printf("Successfully installed tiles archive on DBC at %s", remotePath)

	u.recordInstall(ctx, false, filepath.Base(localPath), remotePath)
	return nil
}
