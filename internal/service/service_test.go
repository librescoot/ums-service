package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/librescoot/ums-service/pkg/update"
)

// TestDecideRebootOwnerAction covers the startup reconciliation of a
// stale MDB reboot-owner claim. The adopted case is the bench deadlock:
// the awaiter died with the claim held while the MDB install had
// already completed to pending-reboot, deadlocking update-service,
// which defers to the claim.
func TestDecideRebootOwnerAction(t *testing.T) {
	tests := []struct {
		name      string
		mdb       string
		dbc       string
		owner     string
		ownerHeld bool
		want      ownerAction
	}{
		{
			name:      "no claim at all",
			mdb:       "idle",
			dbc:       "idle",
			owner:     "",
			ownerHeld: false,
			want:      ownerKeep,
		},
		{
			name:      "claim with nothing pending",
			mdb:       "idle",
			dbc:       "idle",
			owner:     "ums",
			ownerHeld: true,
			want:      ownerClear,
		},
		{
			name:      "claim with failed install",
			mdb:       "error",
			dbc:       "error",
			owner:     "ums",
			ownerHeld: true,
			want:      ownerClear,
		},
		{
			name:      "bench deadlock: MDB complete, DBC settled",
			mdb:       "pending-reboot",
			dbc:       "idle",
			owner:     "ums",
			ownerHeld: true,
			want:      ownerAdopt,
		},
		{
			name:      "MDB complete, DBC mid-flight",
			mdb:       "pending-reboot",
			dbc:       "installing",
			owner:     "ums",
			ownerHeld: true,
			want:      ownerKeep,
		},
		{
			name:      "MDB complete, DBC still activating",
			mdb:       "pending-reboot",
			dbc:       "pending-reboot",
			owner:     "ums",
			ownerHeld: true,
			want:      ownerKeep,
		},
		{
			name:      "MDB install active",
			mdb:       "installing",
			dbc:       "idle",
			owner:     "ums",
			ownerHeld: true,
			want:      ownerKeep,
		},
		{
			name:      "idle MDB, DBC still activating",
			mdb:       "idle",
			dbc:       "pending-reboot",
			owner:     "ums",
			ownerHeld: true,
			want:      ownerKeep,
		},
		{
			name:      "staged no-op is settled",
			mdb:       "staged-noop",
			dbc:       "idle",
			owner:     "ums",
			ownerHeld: true,
			want:      ownerClear,
		},
		{
			name:      "staged no-op MDB, DBC still activating",
			mdb:       "staged-noop",
			dbc:       "installing",
			owner:     "ums",
			ownerHeld: true,
			want:      ownerKeep,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideRebootOwnerAction(tt.mdb, tt.dbc, tt.owner, tt.ownerHeld)
			if got != tt.want {
				t.Fatalf("decideRebootOwnerAction() = %v, want %v", got, tt.want)
			}
		})
	}
}

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
			got, err := service.checkIfDBCNeeded(root)
			if err != nil {
				t.Fatalf("checkIfDBCNeeded() returned error: %v", err)
			}
			if got != tt.wantDBC {
				t.Fatalf("checkIfDBCNeeded() = %v, want %v", got, tt.wantDBC)
			}
		})
	}
}

// TestCheckIfDBCNeeded_UnreadableDirectoryFails covers the bench
// regression where a failed listing was silently treated as "no DBC
// operations needed": a scan failure must surface as an error so the
// import pass aborts with the staged artifacts intact.
func TestCheckIfDBCNeeded_UnreadableDirectoryFails(t *testing.T) {
	root := t.TempDir()
	// A regular file where the system-update directory belongs makes
	// every ReadDir attempt fail with ENOTDIR.
	if err := os.WriteFile(filepath.Join(root, "system-update"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	service := &Service{}
	if _, err := service.checkIfDBCNeeded(root); err == nil {
		t.Fatal("checkIfDBCNeeded() succeeded on an unreadable system-update directory")
	}
}

// TestTruncateUTF16 pins the notification ingress limits: title <= 120 and
// body <= 512 UTF-16 code units, counted so an astral character is two.
func TestTruncateUTF16(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than cap", "abc", 5, "abc"},
		{"exactly at cap", "abcde", 5, "abcde"},
		{"cut at cap", "abcdef", 5, "abcde"},
		{"astral character counts as two", "a\U0001F600b", 2, "a"},
		{"astral character fits", "a\U0001F600b", 3, "a\U0001F600"},
		{"zero cap", "abc", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateUTF16(tc.in, tc.max); got != tc.want {
				t.Errorf("truncateUTF16(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// TestWithRefusalDetail pins the mixed-cycle result contract: a refusal from a
// board that was skipped must be folded into the terminal result detail, since
// the awaiter's later write for the installing board would otherwise be the
// only thing `lsc usb status` shows.
func TestWithRefusalDetail(t *testing.T) {
	refusals := []update.Refusal{{Board: "dbc", Reason: "two full images staged"}}

	if got, want := withRefusalDetail("MDB reboot triggered", refusals),
		"MDB reboot triggered; refused: DBC: two full images staged"; got != want {
		t.Errorf("withRefusalDetail = %q, want %q", got, want)
	}
	if got, want := withRefusalDetail("", refusals), "DBC: two full images staged"; got != want {
		t.Errorf("withRefusalDetail(empty) = %q, want %q", got, want)
	}
	if got, want := withRefusalDetail("MDB reboot triggered", nil), "MDB reboot triggered"; got != want {
		t.Errorf("withRefusalDetail(no refusals) = %q, want %q", got, want)
	}

	both := withRefusalDetail("x", []update.Refusal{
		{Board: "mdb", Reason: "ambiguous"},
		{Board: "dbc", Reason: "cross-channel"},
	})
	if !strings.Contains(both, "MDB: ambiguous") || !strings.Contains(both, "DBC: cross-channel") {
		t.Errorf("withRefusalDetail = %q, want both boards", both)
	}
}

// TestNotificationPayload pins the scootui:notification ingress contract: the
// exact JSON field names and values, the refusal identity/severity, and the TTL
// bounds. A tag typo or a dropped field would otherwise only surface as a
// qWarning in the dashboard log.
func TestNotificationPayload(t *testing.T) {
	const title = "Update refused"
	const body = "DBC: two full images staged"

	data, err := json.Marshal(newNotification(title, body))
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal notification: %v", err)
	}

	want := map[string]any{
		"id":       "ums-update-refused",
		"source":   "ums",
		"action":   "show",
		"title":    title,
		"body":     body,
		"severity": "error",
		"ttl_ms":   float64(notificationTTL),
	}
	if len(got) != len(want) {
		t.Errorf("payload has %d fields, want %d: %v", len(got), len(want), got)
	}
	for field, value := range want {
		if got[field] != value {
			t.Errorf("payload[%q] = %v, want %v", field, got[field], value)
		}
	}
	if notificationTTL < 1000 || notificationTTL > 60000 {
		t.Errorf("ttl_ms = %d outside the ingress 1000-60000 range", notificationTTL)
	}

	// The builder applies the ingress title/body caps (120/512 UTF-16 units).
	long := newNotification(strings.Repeat("x", 200), strings.Repeat("y", 600))
	if len(long.Title) != 120 || len(long.Body) != 512 {
		t.Errorf("caps not applied: title=%d body=%d, want 120/512", len(long.Title), len(long.Body))
	}
}

// TestEnteringUMSFromNormal pins the re-entry policy: the stale-artifact sweep
// runs only when the drive is handed over fresh from normal mode. Re-entering
// UMS while a UMS variant is already exporting (ums -> ums-by-dbc) must leave
// host-written, not-yet-imported files alone.
func TestEnteringUMSFromNormal(t *testing.T) {
	if !enteringUMSFromNormal("normal") {
		t.Error("entering from normal must sweep the stale artifacts")
	}
	if enteringUMSFromNormal("ums") {
		t.Error("re-entering from ums must not sweep")
	}
	if enteringUMSFromNormal("ums-by-dbc") {
		t.Error("re-entering from ums-by-dbc must not sweep")
	}
}
