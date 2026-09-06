package maps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsValhallaTilesArchive(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"valhalla_tiles_bayern.tar", true},
		{"valhalla_tiles_bayern.tar.zst", true},
		{"tiles.tar", true},
		{"tiles.tar.zst", true},
		{"valhalla_tiles_berlin_brandenburg.tar.zst", true},
		{"map.mbtiles", false},
		{"valhalla_tiles_bayern.tar.gz", false},
		{"notes.txt", false},
		{"tiles.tar.zst.part", false},
	}
	for _, c := range cases {
		if got := IsValhallaTilesArchive(c.name); got != c.want {
			t.Errorf("IsValhallaTilesArchive(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestProcessMapsEmptyDirectoryDoesNotRequireDBC(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "maps"), 0o755); err != nil {
		t.Fatal(err)
	}

	updater := New(nil, nil)
	if err := updater.ProcessMaps(context.Background(), time.Second, nil, root); err != nil {
		t.Fatalf("ProcessMaps() = %v, want successful no-op", err)
	}
}

func TestIsCompressedTilesArchive(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"valhalla_tiles_bayern.tar.zst", true},
		{"tiles.tar.zst", true},
		{"valhalla_tiles_bayern.tar", false},
		{"map.mbtiles", false},
	}
	for _, c := range cases {
		if got := isCompressedTilesArchive(c.name); got != c.want {
			t.Errorf("isCompressedTilesArchive(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
