package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckIfDBCNeeded(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		dirs    []string
		wantDBC bool
	}{
		{
			name:    "DBC delta update",
			files:   []string{"system-update/librescoot-unu-dbc-nightly-20260911T063513.delta"},
			wantDBC: true,
		},
		{
			name:    "DBC full update",
			files:   []string{"system-update/librescoot-unu-dbc-nightly-20260911T063513.mender"},
			wantDBC: true,
		},
		{
			name:    "MDB update only",
			files:   []string{"system-update/librescoot-unu-mdb-nightly-20260911T063513.delta"},
			wantDBC: false,
		},
		{
			name: "mixed MDB and DBC updates",
			files: []string{
				"system-update/librescoot-unu-mdb-nightly-20260911T063513.delta",
				"system-update/librescoot-unu-dbc-nightly-20260911T063513.delta",
			},
			wantDBC: true,
		},
		{
			name: "irrelevant files and directories",
			files: []string{
				"system-update/notes.txt",
				"system-update/not-librescoot-dbc.delta",
				"system-update/librescoot-unu-dbc-nightly-20260911T063513.delta.part",
			},
			dirs: []string{
				"system-update/librescoot-unu-dbc-nightly-20260911T063513.delta",
			},
			wantDBC: false,
		},
		{
			name:    "MBTiles map",
			files:   []string{"maps/map.mbtiles"},
			wantDBC: true,
		},
		{
			name:    "Valhalla tiles archive",
			files:   []string{"maps/valhalla_tiles_bayern.tar.zst"},
			wantDBC: true,
		},
		{
			name:    "DBC script",
			files:   []string{"scripts/dbc.sh"},
			wantDBC: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range tt.dirs {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range tt.files {
				path := filepath.Join(root, file)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			service := &Service{}
			if got := service.checkIfDBCNeeded(root); got != tt.wantDBC {
				t.Fatalf("checkIfDBCNeeded() = %v, want %v", got, tt.wantDBC)
			}
		})
	}
}
