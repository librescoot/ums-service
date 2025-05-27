package maps

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/librescoot/ums-service/pkg/dbc"
)

type Updater struct {
	dbcMapsDir     string
	dbcValhallaDir string
	dbcInterface   *dbc.Interface
}

func New(dbcInterface *dbc.Interface) *Updater {
	return &Updater{
		dbcMapsDir:     "/data/maps",
		dbcValhallaDir: "/data/valhalla",
		dbcInterface:   dbcInterface,
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

func (u *Updater) ProcessMaps(usbMountPath string) error {
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
		} else if strings.HasSuffix(filename, "tiles.tar") {
			tilesFile = filepath.Join(mapsDir, filename)
		}
	}

	// Process mbtiles file
	if mbtilesFile != "" {
		if err := u.processMBTiles(mbtilesFile); err != nil {
			return fmt.Errorf("failed to process mbtiles: %w", err)
		}
	}

	// Process tiles.tar file
	if tilesFile != "" {
		if err := u.processTilesTar(tilesFile); err != nil {
			return fmt.Errorf("failed to process tiles.tar: %w", err)
		}
	}

	if mbtilesFile == "" && tilesFile == "" {
		log.Println("No map files found to process")
	}

	return nil
}

func (u *Updater) processMBTiles(localPath string) error {
	// Create remote maps directory
	if _, err := u.dbcInterface.RunCommand(fmt.Sprintf("mkdir -p %s", u.dbcMapsDir)); err != nil {
		return fmt.Errorf("failed to create remote maps directory: %w", err)
	}

	remotePath := filepath.Join(u.dbcMapsDir, "map.mbtiles")

	// Copy mbtiles file to DBC
	if err := u.dbcInterface.CopyFile(localPath, remotePath); err != nil {
		return fmt.Errorf("failed to copy mbtiles to DBC: %w", err)
	}

	log.Printf("Successfully copied mbtiles to DBC at %s", remotePath)
	return nil
}

func (u *Updater) processTilesTar(localPath string) error {
	// Create remote valhalla directory
	if _, err := u.dbcInterface.RunCommand(fmt.Sprintf("mkdir -p %s", u.dbcValhallaDir)); err != nil {
		return fmt.Errorf("failed to create remote valhalla directory: %w", err)
	}

	remotePath := filepath.Join(u.dbcValhallaDir, "tiles.tar")

	// Copy tiles.tar file to DBC
	if err := u.dbcInterface.CopyFile(localPath, remotePath); err != nil {
		return fmt.Errorf("failed to copy tiles.tar to DBC: %w", err)
	}

	log.Printf("Successfully copied tiles.tar to DBC at %s", remotePath)
	return nil
}