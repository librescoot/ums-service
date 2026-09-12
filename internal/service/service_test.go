package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/ums-service/pkg/update"
)

type testPublisher struct {
	setMany chan map[string]any
}

func (p *testPublisher) Set(string, any, ...ipc.SetOption) error {
	return nil
}

func (p *testPublisher) SetMany(fields map[string]any, _ ...ipc.SetOption) error {
	copy := make(map[string]any, len(fields))
	for key, value := range fields {
		copy[key] = value
	}
	p.setMany <- copy
	return nil
}

func TestRequestModeCancelsAndSupersedesInFlightPrep(t *testing.T) {
	serviceCtx, stopService := context.WithCancel(context.Background())
	stopService()
	prepCtx, cancelPrep := context.WithCancel(context.Background())
	prep := &operation{target: "ums", ctx: prepCtx, cancel: cancelPrep, done: make(chan struct{})}
	service := &Service{serviceCtx: serviceCtx, currentOp: prep}

	returned := make(chan struct{})
	go func() {
		service.requestMode("ums-by-dbc")
		close(returned)
	}()

	select {
	case <-prepCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("replacement request did not cancel the in-flight prep")
	}
	select {
	case <-returned:
		t.Fatal("replacement request did not wait for prep teardown")
	default:
	}

	close(prep.done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("replacement request did not install the superseding operation")
	}

	service.mu.Lock()
	current := service.currentOp
	service.mu.Unlock()
	if current == prep || current.target != "ums-by-dbc" {
		t.Fatalf("current operation = %#v, want superseding ums-by-dbc operation", current)
	}
	select {
	case <-current.done:
	case <-time.After(time.Second):
		t.Fatal("superseding operation did not finish")
	}
}

func TestRequestModeRetriesCompletedFailedUMSEntry(t *testing.T) {
	for _, target := range []string{"ums", "ums-by-dbc"} {
		t.Run(target, func(t *testing.T) {
			done := make(chan struct{})
			close(done)
			failed := &operation{
				target: "ums",
				ctx:    context.Background(),
				cancel: func() {},
				done:   done,
			}
			serviceCtx, stopService := context.WithCancel(context.Background())
			stopService()
			service := &Service{serviceCtx: serviceCtx, currentOp: failed}

			service.requestMode(target)

			service.mu.Lock()
			current := service.currentOp
			service.mu.Unlock()
			if current == failed || current.target != target {
				t.Fatalf("current operation = %#v, want fresh %s retry", current, target)
			}
			select {
			case <-current.done:
			case <-time.After(time.Second):
				t.Fatal("fresh retry operation did not run")
			}
		})
	}
}

func TestRequestModeLeavesStableStateAlone(t *testing.T) {
	done := make(chan struct{})
	close(done)
	cancelled := false
	stable := &operation{
		target: "normal",
		ctx:    context.Background(),
		cancel: func() { cancelled = true },
		done:   done,
	}
	service := &Service{serviceCtx: context.Background(), currentOp: stable}

	service.requestMode("normal")

	if cancelled {
		t.Fatal("stable operation was cancelled")
	}
	service.mu.Lock()
	current := service.currentOp
	service.mu.Unlock()
	if current != stable {
		t.Fatal("stable operation was replaced")
	}
}

func TestRequestModeWaitsForUnmountBeforeReplacement(t *testing.T) {
	serviceCtx, stopService := context.WithCancel(context.Background())
	stopService()
	prepCtx, cancelPrep := context.WithCancel(context.Background())
	prep := &operation{target: "ums", ctx: prepCtx, cancel: cancelPrep, done: make(chan struct{})}
	service := &Service{
		serviceCtx: serviceCtx,
		currentOp:  prep,
		publisher:  &testPublisher{setMany: make(chan map[string]any, 1)},
	}
	unmounted := make(chan struct{})

	go func() {
		<-prepCtx.Done()
		// This models runUMSOp's deferred unmount, which completes before
		// runOperation closes done.
		close(unmounted)
		close(prep.done)
	}()

	service.requestMode("normal")
	select {
	case <-unmounted:
	default:
		t.Fatal("replacement started before cancelled prep unmounted")
	}
	service.mu.Lock()
	current := service.currentOp
	service.mu.Unlock()
	if current == prep || current.target != "normal" {
		t.Fatalf("current operation = %#v, want normal replacement", current)
	}
}

func TestRequestModeCancellationPublishesIdle(t *testing.T) {
	serviceCtx, stopService := context.WithCancel(context.Background())
	stopService()
	prepCtx, cancelPrep := context.WithCancel(context.Background())
	prep := &operation{target: "ums", ctx: prepCtx, cancel: cancelPrep, done: make(chan struct{})}
	publisher := &testPublisher{setMany: make(chan map[string]any, 1)}
	service := &Service{serviceCtx: serviceCtx, currentOp: prep, publisher: publisher}

	returned := make(chan struct{})
	go func() {
		service.requestMode("normal")
		close(returned)
	}()

	select {
	case fields := <-publisher.setMany:
		if fields["status"] != "idle" || fields["step"] != "" || len(fields) != 2 {
			t.Fatalf("idle publication = %#v, want only status=idle and step=empty", fields)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelling prep did not publish idle")
	}
	select {
	case <-returned:
		t.Fatal("normal request returned before the prep tore down")
	default:
	}

	close(prep.done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("normal request did not complete after teardown")
	}
}

func TestCancelledOperationTerminalWritesLeaveIdlePublished(t *testing.T) {
	serviceCtx, stopService := context.WithCancel(context.Background())
	stopService()
	prepCtx, cancelPrep := context.WithCancel(context.Background())
	prep := &operation{target: "ums", ctx: prepCtx, cancel: cancelPrep, done: make(chan struct{})}
	publisher := &testPublisher{setMany: make(chan map[string]any, 2)}
	service := &Service{serviceCtx: serviceCtx, currentOp: prep, publisher: publisher}

	returned := make(chan struct{})
	go func() {
		service.requestMode("normal")
		close(returned)
	}()
	select {
	case <-prepCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("normal request did not cancel UMS preparation")
	}
	select {
	case fields := <-publisher.setMany:
		if fields["status"] != "idle" || fields["step"] != "" {
			t.Fatalf("idle publication = %#v, want idle with cleared step", fields)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelling prep did not publish idle")
	}
	if service.publishUMSFailureIfActive(prep, "could not prepare the USB drive: %v", os.ErrPermission) {
		t.Fatal("cancelled operation published a terminal failure")
	}
	select {
	case fields := <-publisher.setMany:
		t.Fatalf("cancelled operation clobbered idle publication: %#v", fields)
	default:
	}
	close(prep.done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("normal request did not complete after teardown")
	}
}

func TestUMSActiveOrPreparingIncludesPrepAndFailedExit(t *testing.T) {
	prepCtx, cancelPrep := context.WithCancel(context.Background())
	preparing := &Service{currentOp: &operation{
		target: "ums-by-dbc",
		ctx:    prepCtx,
		cancel: cancelPrep,
		done:   make(chan struct{}),
	}}
	if target, active := preparing.umsActiveOrPreparing(); !active || target != "ums-by-dbc" {
		t.Fatalf("in-flight prep active,target = %v,%q, want true,ums-by-dbc", active, target)
	}

	done := make(chan struct{})
	close(done)
	failedExit := &Service{umsModeType: "ums", currentOp: &operation{
		target: "normal",
		ctx:    context.Background(),
		cancel: func() {},
		done:   done,
	}}
	if target, active := failedExit.umsActiveOrPreparing(); !active || target != "ums" {
		t.Fatalf("failed switch-back active,target = %v,%q, want true,ums", active, target)
	}
}

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
