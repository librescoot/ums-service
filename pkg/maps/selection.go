package maps

import (
	"os"
	"path/filepath"
	"strings"
)

type mapFiles struct {
	legacyMap        string
	legacyRouting    string
	regionalMaps     []string
	regionalRouting  map[string]string
	explicitRegional bool
}

func selectMapFiles(entries []os.DirEntry, mapsDir string) mapFiles {
	files := mapFiles{regionalRouting: make(map[string]string)}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(mapsDir, name)
		if name == "regional-packs" {
			files.explicitRegional = true
		}
		if strings.HasSuffix(name, ".mbtiles") {
			files.legacyMap = path
			if strings.HasPrefix(name, "tiles_") &&
				validRegionalName(strings.TrimSuffix(strings.TrimPrefix(name, "tiles_"), ".mbtiles")) {
				files.regionalMaps = append(files.regionalMaps, path)
			}
		}
		if !IsValhallaTilesArchive(name) {
			continue
		}
		files.legacyRouting = path
		if !strings.HasPrefix(name, "valhalla_tiles_") {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, "valhalla_tiles_"), ".zst"), ".tar")
		if validRegionalName(slug) {
			if _, found := files.regionalRouting[slug]; !found || strings.HasSuffix(name, ".zst") {
				files.regionalRouting[slug] = path
			}
		}
	}
	return files
}
