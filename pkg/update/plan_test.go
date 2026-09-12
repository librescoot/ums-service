package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/librescoot/ums-service/pkg/dbc"
)

func TestPlanBoardUpdate(t *testing.T) {
	cases := []struct {
		name       string
		paths      []string
		wantPaths  []string
		wantReason bool
	}{
		{
			name:      "single full image",
			paths:     []string{"/usb/librescoot-unu-mdb-v1.2.0.mender"},
			wantPaths: []string{"/usb/librescoot-unu-mdb-v1.2.0.mender"},
		},
		{
			name:      "single delta",
			paths:     []string{"/usb/librescoot-unu-mdb-v1.2.0.delta"},
			wantPaths: []string{"/usb/librescoot-unu-mdb-v1.2.0.delta"},
		},
		{
			// Order is the caller's; update-service resolves the chain from
			// the deltas' metadata, not from the command.
			name: "delta chain passes through unchanged",
			paths: []string{
				"/usb/librescoot-unu-mdb-nightly-20260103T120000.delta",
				"/usb/librescoot-unu-mdb-nightly-20260101T120000.delta",
				"/usb/librescoot-unu-mdb-nightly-20260102T120000.delta",
			},
			wantPaths: []string{
				"/usb/librescoot-unu-mdb-nightly-20260103T120000.delta",
				"/usb/librescoot-unu-mdb-nightly-20260101T120000.delta",
				"/usb/librescoot-unu-mdb-nightly-20260102T120000.delta",
			},
		},
		{
			name: "two full images conflict",
			paths: []string{
				"/usb/librescoot-unu-mdb-v1.2.0.mender",
				"/usb/librescoot-unu-mdb-v1.3.0.mender",
			},
			wantReason: true,
		},
		{
			name: "full image plus delta conflict",
			paths: []string{
				"/usb/librescoot-unu-mdb-v1.2.0.mender",
				"/usb/librescoot-unu-mdb-v1.2.0.delta",
			},
			wantReason: true,
		},
		{
			name: "one full image plus two deltas conflict",
			paths: []string{
				"/usb/librescoot-unu-mdb-v1.2.0.mender",
				"/usb/librescoot-unu-mdb-v1.2.0.delta",
				"/usb/librescoot-unu-mdb-v1.3.0.delta",
			},
			wantReason: true,
		},
		{
			name: "deltas across channels conflict",
			paths: []string{
				"/usb/librescoot-unu-mdb-nightly-20260101T120000.delta",
				"/usb/librescoot-unu-mdb-v1.2.0.delta",
			},
			wantReason: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := planBoardUpdate("mdb", tc.paths, nil)
			if tc.wantReason {
				if reason == "" {
					t.Fatalf("planBoardUpdate reason is empty, want a conflict (paths=%v)", got)
				}
				if got != nil {
					t.Fatalf("conflicted board returned paths %v, want none", got)
				}
				return
			}
			if reason != "" {
				t.Fatalf("planBoardUpdate refused a valid drop: %s", reason)
			}
			if strings.Join(got, "\n") != strings.Join(tc.wantPaths, "\n") {
				t.Errorf("planBoardUpdate paths =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.wantPaths, "\n"))
			}
		})
	}
}

func TestProcessUpdatesMDBDeltaChain(t *testing.T) {
	usb := t.TempDir()
	updateDir := filepath.Join(usb, "system-update")
	if err := os.MkdirAll(updateDir, 0755); err != nil {
		t.Fatal(err)
	}
	names := []string{
		"librescoot-unu-mdb-nightly-20260103T120000.delta",
		"librescoot-unu-mdb-nightly-20260101T120000.delta",
		"librescoot-unu-mdb-nightly-20260102T120000.delta",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(updateDir, n), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	ota := filepath.Join(t.TempDir(), "mdb")
	l := &Loader{otaRootDir: filepath.Join(ota, ".."), otaDir: ota, dbcOtaDir: filepath.Join(ota, "..", "dbc")}

	queued, err := l.ProcessUpdates(context.Background(), time.Minute, nil, usb)
	if err != nil {
		t.Fatalf("ProcessUpdates: %v", err)
	}
	if !queued.MDB || queued.DBC {
		t.Fatalf("queued = {MDB:%v DBC:%v}, want MDB only", queued.MDB, queued.DBC)
	}
	if len(queued.Refused) != 0 {
		t.Fatalf("unexpected refusals: %+v", queued.Refused)
	}
	if len(queued.PendingPushes) != 1 {
		t.Fatalf("PendingPushes = %d, want 1", len(queued.PendingPushes))
	}
	push := queued.PendingPushes[0]
	if push.Channel != "scooter:update:mdb" {
		t.Errorf("channel = %q, want scooter:update:mdb", push.Channel)
	}
	// The command is path-free: update-service discovers the staged chain.
	if push.Value != stagedUpdateCommand {
		t.Errorf("push value = %q, want %q", push.Value, stagedUpdateCommand)
	}
	// Every chain member must be staged before the command is pushed.
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(ota, n)); err != nil {
			t.Errorf("chain member %s was not staged: %v", n, err)
		}
	}
}

// TestProcessUpdatesConflictSkipsBoardOnly pins the agreed rule: a conflicted
// board is skipped entirely, while the other board in the same drop proceeds.
// The conflicted board here is the DBC, so it is never handed to the DBC
// interface.
func TestProcessUpdatesConflictSkipsBoardOnly(t *testing.T) {
	usb := t.TempDir()
	updateDir := filepath.Join(usb, "system-update")
	if err := os.MkdirAll(updateDir, 0755); err != nil {
		t.Fatal(err)
	}
	dbcMender := "librescoot-unu-dbc-nightly-20260102T120000.mender"
	dbcDelta := "librescoot-unu-dbc-nightly-20260103T120000.delta"
	mdbDelta := "librescoot-unu-mdb-nightly-20260102T120000.delta"
	for _, n := range []string{dbcMender, dbcDelta, mdbDelta} {
		if err := os.WriteFile(filepath.Join(updateDir, n), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	ota := filepath.Join(t.TempDir(), "mdb")
	dbcOta := filepath.Join(ota, "..", "dbc")
	l := &Loader{otaRootDir: filepath.Join(ota, ".."), otaDir: ota, dbcOtaDir: dbcOta}

	queued, err := l.ProcessUpdates(context.Background(), time.Minute, nil, usb)
	if err != nil {
		t.Fatalf("ProcessUpdates: %v", err)
	}
	if !queued.MDB || queued.DBC {
		t.Fatalf("queued = {MDB:%v DBC:%v}, want the MDB board only", queued.MDB, queued.DBC)
	}
	if len(queued.Refused) != 1 || queued.Refused[0].Board != "dbc" {
		t.Fatalf("Refused = %+v, want the DBC board", queued.Refused)
	}
	if len(queued.PendingPushes) != 1 || queued.PendingPushes[0].Channel != "scooter:update:mdb" {
		t.Fatalf("PendingPushes = %+v, want one MDB push", queued.PendingPushes)
	}
	// The MDB file was staged; the refused board's files were not.
	if _, err := os.Stat(filepath.Join(ota, mdbDelta)); err != nil {
		t.Errorf("MDB delta was not staged: %v", err)
	}
	for _, n := range []string{dbcMender, dbcDelta} {
		if _, err := os.Stat(filepath.Join(dbcOta, n)); err == nil {
			t.Errorf("refused DBC file %s was staged", n)
		}
	}
}

// TestProcessUpdatesConflictMDBOnly pins the refused-board-not-staged half of
// the rule for a same-board conflict.
func TestProcessUpdatesConflictMDBOnly(t *testing.T) {
	usb := t.TempDir()
	updateDir := filepath.Join(usb, "system-update")
	if err := os.MkdirAll(updateDir, 0755); err != nil {
		t.Fatal(err)
	}
	mender := "librescoot-unu-mdb-nightly-20260102T120000.mender"
	delta := "librescoot-unu-mdb-nightly-20260103T120000.delta"
	for _, n := range []string{mender, delta} {
		if err := os.WriteFile(filepath.Join(updateDir, n), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	ota := filepath.Join(t.TempDir(), "mdb")
	l := &Loader{otaRootDir: filepath.Join(ota, ".."), otaDir: ota, dbcOtaDir: filepath.Join(ota, "..", "dbc")}

	queued, err := l.ProcessUpdates(context.Background(), time.Minute, nil, usb)
	if err != nil {
		t.Fatalf("ProcessUpdates: %v", err)
	}
	if queued.MDB || queued.DBC || len(queued.PendingPushes) != 0 {
		t.Fatalf("conflicted board was acted on: %+v", queued)
	}
	if len(queued.Refused) != 1 || queued.Refused[0].Board != "mdb" {
		t.Fatalf("Refused = %+v, want the MDB board", queued.Refused)
	}
	// A refused board must not be staged either: an implementation that copied
	// the files and only skipped the push would still pass the checks above.
	for _, n := range []string{mender, delta} {
		if _, err := os.Stat(filepath.Join(ota, n)); err == nil {
			t.Errorf("refused MDB file %s was staged", n)
		}
	}
}

func TestProcessUpdatesSingleDeltaStaged(t *testing.T) {
	usb := t.TempDir()
	updateDir := filepath.Join(usb, "system-update")
	if err := os.MkdirAll(updateDir, 0755); err != nil {
		t.Fatal(err)
	}
	name := "librescoot-unu-mdb-nightly-20260102T120000.delta"
	if err := os.WriteFile(filepath.Join(updateDir, name), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	ota := filepath.Join(t.TempDir(), "mdb")
	l := &Loader{otaRootDir: filepath.Join(ota, ".."), otaDir: ota, dbcOtaDir: filepath.Join(ota, "..", "dbc")}

	queued, err := l.ProcessUpdates(context.Background(), time.Minute, nil, usb)
	if err != nil {
		t.Fatalf("ProcessUpdates: %v", err)
	}
	if len(queued.PendingPushes) != 1 || queued.PendingPushes[0].Value != stagedUpdateCommand {
		t.Fatalf("PendingPushes = %+v, want a single %q", queued.PendingPushes, stagedUpdateCommand)
	}
	if _, err := os.Stat(filepath.Join(ota, name)); err != nil {
		t.Errorf("staged delta missing: %v", err)
	}
}

// fakeDBC records what the loader asks the DBC interface to do. It satisfies
// dbcUpdater without an ssh/redis connection.
type fakeDBC struct {
	enabled   bool
	commands  []string
	transfers []string
	queued    int
}

func (f *fakeDBC) IsEnabled() bool { return f.enabled }

func (f *fakeDBC) RunCommand(_ context.Context, command string) (string, error) {
	f.commands = append(f.commands, command)
	return "", nil
}

func (f *fakeDBC) TransferFile(_ context.Context, _, remotePath string, _ dbc.ProgressFunc) error {
	f.transfers = append(f.transfers, remotePath)
	return nil
}

func (f *fakeDBC) MarkDBCUpdateQueued() { f.queued++ }

// TestProcessUpdatesDBCMultiFileTransferAndHandoff covers the DBC half of a
// multi-file drop: every member is transferred in order, then exactly one
// MarkDBCUpdateQueued handoff, then one path-free command.
func TestProcessUpdatesDBCMultiFileTransferAndHandoff(t *testing.T) {
	usb := t.TempDir()
	updateDir := filepath.Join(usb, "system-update")
	if err := os.MkdirAll(updateDir, 0755); err != nil {
		t.Fatal(err)
	}
	names := []string{
		"librescoot-unu-dbc-nightly-20260101T120000.delta",
		"librescoot-unu-dbc-nightly-20260102T120000.delta",
		"librescoot-unu-dbc-nightly-20260103T120000.delta",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(updateDir, n), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	ota := filepath.Join(root, "mdb")
	dbcOta := filepath.Join(root, "dbc")
	fake := &fakeDBC{enabled: true}
	l := &Loader{
		otaRootDir:   root,
		otaDir:       ota,
		dbcOtaDir:    dbcOta,
		managedDirs:  []managedDir{{ota, 1}, {dbcOta, 1}},
		dbcInterface: fake,
	}

	queued, err := l.ProcessUpdates(context.Background(), time.Minute, nil, usb)
	if err != nil {
		t.Fatalf("ProcessUpdates: %v", err)
	}
	if queued.MDB || !queued.DBC {
		t.Fatalf("queued = {MDB:%v DBC:%v}, want DBC only", queued.MDB, queued.DBC)
	}
	if fake.queued != 1 {
		t.Fatalf("MarkDBCUpdateQueued called %d times, want exactly 1", fake.queued)
	}
	wantTransfers := []string{
		filepath.Join(dbcOta, names[0]),
		filepath.Join(dbcOta, names[1]),
		filepath.Join(dbcOta, names[2]),
	}
	if strings.Join(fake.transfers, "\n") != strings.Join(wantTransfers, "\n") {
		t.Errorf("transfers =\n%s\nwant\n%s", strings.Join(fake.transfers, "\n"), strings.Join(wantTransfers, "\n"))
	}
	if len(fake.commands) != 1 || fake.commands[0] != "mkdir -p "+dbcOta {
		t.Errorf("commands = %v, want one mkdir -p %s", fake.commands, dbcOta)
	}
	if len(queued.PendingPushes) != 1 ||
		queued.PendingPushes[0].Channel != "scooter:update:dbc" ||
		queued.PendingPushes[0].Value != stagedUpdateCommand {
		t.Fatalf("PendingPushes = %+v, want one path-free DBC command", queued.PendingPushes)
	}
}

// TestNewManagedDirsCoverOtaDirs pins that the staging dirs are part of the
// orphan-sweep allowlist: they are load-bearing there, not retention config.
func TestNewManagedDirsCoverOtaDirs(t *testing.T) {
	l := New(nil, nil)
	got := make(map[string]bool, len(l.managedDirs))
	for _, md := range l.managedDirs {
		got[filepath.Clean(md.path)] = true
	}
	for _, want := range []string{filepath.Clean(l.otaDir), filepath.Clean(l.dbcOtaDir)} {
		if !got[want] {
			t.Errorf("managedDirs is missing %s; the orphan sweep would delete staged updates there", want)
		}
	}
}

// TestCleanupStaleFilesKeepsStagedDeltaChain guards the interaction between
// the post-cycle cleanup and a staged chain: CleanupStaleFiles runs after
// ProcessUpdates has copied the chain into /data/ota/mdb and before
// update-service consumes it, so pruning that directory would delete chain
// members out from under an in-flight install.
func TestCleanupStaleFilesKeepsStagedDeltaChain(t *testing.T) {
	root := t.TempDir()
	mdb := filepath.Join(root, "mdb")
	dbc := filepath.Join(root, "dbc")
	for _, d := range []string{mdb, dbc} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	chain := []string{
		"librescoot-unu-mdb-nightly-20260101T120000.delta",
		"librescoot-unu-mdb-nightly-20260102T120000.delta",
		"librescoot-unu-mdb-nightly-20260103T120000.delta",
	}
	for _, name := range chain {
		if err := os.WriteFile(filepath.Join(mdb, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	l := &Loader{
		otaRootDir: root,
		otaDir:     mdb,
		dbcOtaDir:  dbc,
		managedDirs: []managedDir{
			{mdb, 1},
			{dbc, 1},
		},
	}

	if err := l.CleanupStaleFiles(); err != nil {
		t.Fatalf("CleanupStaleFiles: %v", err)
	}

	for _, name := range chain {
		if _, err := os.Stat(filepath.Join(mdb, name)); err != nil {
			t.Errorf("staged chain member %s was removed: %v", name, err)
		}
	}
}
