package maps

import "testing"

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
		if got := isValhallaTilesArchive(c.name); got != c.want {
			t.Errorf("isValhallaTilesArchive(%q) = %v, want %v", c.name, got, c.want)
		}
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
