package maps

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRegionFromFilename(t *testing.T) {
	tests := []struct {
		filename string
		want     string
	}{
		// The published artifact names, which are what a user downloads from
		// the tile repos and drops on the stick.
		{"tiles_berlin_brandenburg.mbtiles", "berlin_brandenburg"},
		{"tiles_baden-wuerttemberg.mbtiles", "baden-wuerttemberg"},
		{"tiles_italy-nord-ovest.mbtiles", "italy-nord-ovest"},
		{"valhalla_tiles_berlin_brandenburg.tar", "berlin_brandenburg"},
		{"valhalla_tiles_netherlands.tar.zst", "netherlands"},
		{"/media/usb/maps/valhalla_tiles_bayern.tar.zst", "bayern"},

		// The generic names ProcessMaps also accepts. No region can be read
		// out of these, and guessing one would be worse than admitting it.
		{"map.mbtiles", ""},
		{"tiles.tar", ""},
		{"tiles.tar.zst", ""},

		// Near misses that must not be parsed as a region.
		{"valhalla_tiles_bayern.mbtiles", ""},
		{"tiles_bayern.tar", ""},
		{"something-else.mbtiles", ""},
		{"", ""},
	}

	for _, tc := range tests {
		if got := RegionFromFilename(tc.filename); got != tc.want {
			t.Errorf("RegionFromFilename(%q) = %q, want %q", tc.filename, got, tc.want)
		}
	}
}

// Every artifact IsValhallaTilesArchive accepts has to be something
// RegionFromFilename can either name or explicitly decline. A name that parses
// as a region under one and not the other is how the two drift apart.
func TestRegionFromFilenameCoversAcceptedArchives(t *testing.T) {
	accepted := []string{
		"tiles.tar",
		"tiles.tar.zst",
		"valhalla_tiles_bremen.tar",
		"valhalla_tiles_bremen.tar.zst",
	}
	for _, name := range accepted {
		if !IsValhallaTilesArchive(name) {
			t.Fatalf("test premise wrong: %q is not an accepted archive", name)
		}
		// Only asserts it does not panic or return junk with a separator in
		// it; the exact values are covered above.
		if got := RegionFromFilename(name); got != "" && got != "bremen" {
			t.Errorf("RegionFromFilename(%q) = %q, want \"\" or \"bremen\"", name, got)
		}
	}
}

// The three install paths all write /data/maps/metadata.json and scootui-qt
// reads it. This pins the shape against what MapMetadata::fromJson expects
// (src/models/MapMetadata.h) and against what the installer's shell composes.
func TestMetadataJSONShape(t *testing.T) {
	meta := &Metadata{
		Region:        "berlin_brandenburg",
		DisplayTiles:  &TileInfo{Digest: "aa11", Size: 208076800},
		ValhallaTiles: &TileInfo{Digest: "bb22", Size: 202055680},
	}

	got, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}

	const want = `{"region":"berlin_brandenburg",` +
		`"displayTiles":{"digest":"aa11","size":208076800},` +
		`"valhallaTiles":{"digest":"bb22","size":202055680}}`
	if string(got) != want {
		t.Errorf("metadata.json shape drifted\n got: %s\nwant: %s", got, want)
	}

	// No apostrophe can appear, which is what makes single-quoting the payload
	// for the remote shell safe. writeRemoteMetadata refuses otherwise.
	if bytes.ContainsRune(got, '\'') {
		t.Error("encoded metadata contains an apostrophe")
	}
}

// An install that could not name the region records an empty one rather than
// keeping the previous value. The key has to disappear, not be present and
// empty, or the dashboard reads "" as a region it should not re-derive.
func TestMetadataOmitsUnknownRegion(t *testing.T) {
	meta := &Metadata{DisplayTiles: &TileInfo{Digest: "aa11", Size: 1}}

	got, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(`"region"`)) {
		t.Errorf("empty region should be omitted, got %s", got)
	}
}
