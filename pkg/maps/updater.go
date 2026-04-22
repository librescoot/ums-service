package maps

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/librescoot/ums-service/pkg/dbc"
	"github.com/librescoot/ums-service/pkg/umslog"
)

type Updater struct {
	dbcMapsDir     string
	dbcValhallaDir string
	dbcInterface   *dbc.Interface
}

func isValhallaTilesArchive(filename string) bool {
	return strings.HasSuffix(filename, "tiles.tar") ||
		(strings.HasPrefix(filename, "valhalla_tiles_") && strings.HasSuffix(filename, ".tar"))
}

func New(dbcInterface *dbc.Interface) *Updater {
	return &Updater{
		dbcMapsDir:     "/data/maps",
		dbcValhallaDir: "/data/valhalla",
		dbcInterface:   dbcInterface,
	}
}

func (u *Updater) PrepareUSB(ctx context.Context, usbMountPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
		} else if isValhallaTilesArchive(filename) {
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
	return nil
}

func (u *Updater) processTilesTar(ctx context.Context, timeout time.Duration, logger *umslog.Logger, localPath string) error {
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := u.dbcInterface.RunCommand(opCtx, fmt.Sprintf("mkdir -p %s", u.dbcValhallaDir)); err != nil {
		return fmt.Errorf("failed to create remote valhalla directory: %w", err)
	}

	remotePath := filepath.Join(u.dbcValhallaDir, "tiles.tar")

	var progress dbc.ProgressFunc
	if logger != nil {
		progress = logger.ProgressCallback("tiles.tar")
		defer logger.ClearProgress()
	}
	if err := u.dbcInterface.TransferFile(opCtx, localPath, remotePath, progress); err != nil {
		return fmt.Errorf("failed to transfer tiles.tar to DBC: %w", err)
	}

	log.Printf("Successfully copied tiles.tar to DBC at %s", remotePath)
	return nil
}
