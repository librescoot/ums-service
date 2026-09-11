package update

import "testing"

func TestUpdateArtifactTarget(t *testing.T) {
	tests := []struct {
		name       string
		filename   string
		wantTarget string
	}{
		{"MDB full", "librescoot-unu-mdb-nightly-20260911T063513.mender", "mdb"},
		{"MDB delta", "librescoot-unu-mdb-nightly-20260911T063513.delta", "mdb"},
		{"DBC full", "librescoot-unu-dbc-nightly-20260911T063513.mender", "dbc"},
		{"DBC delta", "librescoot-unu-dbc-nightly-20260911T063513.delta", "dbc"},
		{"wrong prefix", "not-librescoot-dbc.delta", ""},
		{"partial download", "librescoot-unu-dbc-nightly-20260911T063513.delta.part", ""},
		{"unknown board", "librescoot-unu-nightly-20260911T063513.delta", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := updateArtifactTarget(tt.filename); got != tt.wantTarget {
				t.Fatalf("updateArtifactTarget(%q) = %q, want %q", tt.filename, got, tt.wantTarget)
			}
			if got := IsDBCUpdateArtifact(tt.filename); got != (tt.wantTarget == "dbc") {
				t.Fatalf("IsDBCUpdateArtifact(%q) = %v, want %v", tt.filename, got, tt.wantTarget == "dbc")
			}
		})
	}
}
