package maps

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelectMapFilesPreservesSingletonInputs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"map.mbtiles", "tiles_bremen.mbtiles", "valhalla_tiles_bremen.tar.zst"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := selectMapFiles(entries, dir)
	if filepath.Base(files.legacyMap) != "tiles_bremen.mbtiles" ||
		filepath.Base(files.legacyRouting) != "valhalla_tiles_bremen.tar.zst" {
		t.Fatalf("singleton selection: map=%q routing=%q", files.legacyMap, files.legacyRouting)
	}
	if len(files.regionalMaps) != 1 || files.explicitRegional {
		t.Fatalf("single named pair must remain a singleton by default: %+v", files)
	}
}

func TestSelectMapFilesExplicitRegional(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"regional-packs", "tiles_bremen.mbtiles", "valhalla_tiles_bremen.tar",
		"valhalla_tiles_bremen.tar.zst", "tiles_luxembourg.mbtiles",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := selectMapFiles(entries, dir)
	if !files.explicitRegional || len(files.regionalMaps) != 2 ||
		filepath.Base(files.regionalRouting["bremen"]) != "valhalla_tiles_bremen.tar.zst" {
		t.Fatalf("regional selection: %+v", files)
	}
}
