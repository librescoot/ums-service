package update

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOTASource is a deterministic OTAStatusSource for testing.
type fakeOTASource struct {
	mu sync.Mutex
	// current mirrors what a live ipcOTASource would report from
	// Current: the initial status, updated by every pushed update.
	current map[string]string
	updates chan StatusUpdate
}

func newFakeOTASource(initial map[string]string) *fakeOTASource {
	current := make(map[string]string, len(initial))
	for k, v := range initial {
		current[k] = v
	}
	return &fakeOTASource{
		current: current,
		updates: make(chan StatusUpdate, 32),
	}
}

func (f *fakeOTASource) Current(component string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current[component], nil
}

func (f *fakeOTASource) Changes() <-chan StatusUpdate {
	return f.updates
}

func (f *fakeOTASource) Stop() {}

func (f *fakeOTASource) push(component, status string) {
	f.mu.Lock()
	f.current[component] = status
	f.mu.Unlock()
	f.updates <- StatusUpdate{Component: component, Status: status}
}

func (f *fakeOTASource) close() {
	close(f.updates)
}

func TestWaitForCompletion_MDBOnly_HappyPath(t *testing.T) {
	src := newFakeOTASource(map[string]string{"mdb": "idle"})
	q := Queued{MDB: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second, 10*time.Second, nil)
	}()

	src.push("mdb", "installing")
	src.push("mdb", "pending-reboot")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for awaiter to return")
	}
}

func TestWaitForCompletion_DBCOnly_WaitsForPostRebootIdle(t *testing.T) {
	src := newFakeOTASource(map[string]string{"dbc": "idle"})
	q := Queued{DBC: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second, 10*time.Second, nil)
	}()

	src.push("dbc", "installing")
	src.push("dbc", "pending-reboot")

	select {
	case err := <-done:
		t.Fatalf("returned before the post-reboot outcome: err=%v", err)
	case <-time.After(100 * time.Millisecond):
	}

	src.push("dbc", "idle")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-reboot idle")
	}
}

func TestWaitForCompletion_DBCOnly_PostRebootError(t *testing.T) {
	src := newFakeOTASource(map[string]string{"dbc": "idle"})
	q := Queued{DBC: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second, 10*time.Second, nil)
	}()

	src.push("dbc", "installing")
	src.push("dbc", "pending-reboot")
	src.push("dbc", "error")

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "dbc") || !strings.Contains(err.Error(), "error") {
			t.Errorf("expected error to mention dbc and error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-reboot error")
	}
}

func TestWaitForCompletion_Both_BothMustComplete(t *testing.T) {
	src := newFakeOTASource(map[string]string{"mdb": "idle", "dbc": "idle"})
	q := Queued{MDB: true, DBC: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second, 10*time.Second, nil)
	}()

	src.push("mdb", "installing")
	src.push("mdb", "pending-reboot")

	// Should NOT return yet — DBC still pending.
	select {
	case err := <-done:
		t.Fatalf("returned too early: err=%v", err)
	case <-time.After(100 * time.Millisecond):
	}

	src.push("dbc", "installing")
	src.push("dbc", "pending-reboot")

	select {
	case err := <-done:
		t.Fatalf("returned before DBC verified commit: err=%v", err)
	case <-time.After(100 * time.Millisecond):
	}

	src.push("dbc", "idle")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DBC final idle")
	}
}

func TestWaitForCompletion_Both_DBCFinalDoesNotFinishMDB(t *testing.T) {
	src := newFakeOTASource(map[string]string{"mdb": "idle", "dbc": "idle"})
	q := Queued{MDB: true, DBC: true}
	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second, 10*time.Second, nil)
	}()

	src.push("dbc", "installing")
	src.push("dbc", "pending-reboot")
	src.push("dbc", "idle")
	select {
	case err := <-done:
		t.Fatalf("returned while MDB was unfinished: err=%v", err)
	case <-time.After(100 * time.Millisecond):
	}

	src.push("mdb", "installing")
	src.push("mdb", "pending-reboot")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out after both components finished")
	}
}

func TestWaitForCompletion_InitialPendingRebootIsStale(t *testing.T) {
	// Stale pending-reboot from a prior install. update-service will
	// attempt the new install, which transitions status through
	// downloading/installing. If mender accepts it, we end up at
	// pending-reboot again — this transition should complete.
	src := newFakeOTASource(map[string]string{"mdb": "pending-reboot"})
	q := Queued{MDB: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second, 10*time.Second, nil)
	}()

	src.push("mdb", "downloading")
	src.push("mdb", "installing")
	src.push("mdb", "pending-reboot")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestWaitForCompletion_InitialPendingRebootThenError(t *testing.T) {
	// Realistic stale path: mender refuses the new install because a
	// previous one is staged.
	src := newFakeOTASource(map[string]string{"mdb": "pending-reboot"})
	q := Queued{MDB: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second, 10*time.Second, nil)
	}()

	src.push("mdb", "downloading")
	src.push("mdb", "installing")
	src.push("mdb", "error")

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "mdb") || !strings.Contains(err.Error(), "error") {
			t.Errorf("expected error to mention mdb and error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestWaitForCompletion_InitialErrorThenSuccess(t *testing.T) {
	// Pre-existing error from a prior failed install. update-service
	// clears it before the install runs, so we shouldn't bail just
	// because the initial state was error.
	src := newFakeOTASource(map[string]string{"mdb": "error"})
	q := Queued{MDB: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second, 10*time.Second, nil)
	}()

	src.push("mdb", "installing")
	src.push("mdb", "pending-reboot")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestWaitForCompletion_Timeout(t *testing.T) {
	src := newFakeOTASource(map[string]string{"mdb": "idle"})
	q := Queued{MDB: true}

	start := time.Now()
	err := WaitForCompletion(context.Background(), src, q, 100*time.Millisecond, time.Minute, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
	if elapsed < 100*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("expected ~100ms, got %v", elapsed)
	}
}

func TestWaitForCompletion_ContextCancel(t *testing.T) {
	src := newFakeOTASource(map[string]string{"mdb": "idle"})
	q := Queued{MDB: true}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(ctx, src, q, 5*time.Second, 10*time.Second, nil)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected Canceled, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for awaiter to honor cancellation")
	}
}

func TestWaitForCompletion_NothingQueued(t *testing.T) {
	// Defensive: should return immediately with no error if neither
	// MDB nor DBC was queued.
	src := newFakeOTASource(map[string]string{})
	q := Queued{}

	err := WaitForCompletion(context.Background(), src, q, 1*time.Second, 10*time.Second, nil)
	if err != nil {
		t.Errorf("expected nil error for empty Queued, got %v", err)
	}
}

func TestWaitForCompletion_SourceClosed(t *testing.T) {
	src := newFakeOTASource(map[string]string{"mdb": "idle"})
	q := Queued{MDB: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 5*time.Second, 10*time.Second, nil)
	}()

	src.close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error on source close, got nil")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out")
	}
}

func TestWaitForCompletion_OnPendingNarrowsAsComponentsFinish(t *testing.T) {
	src := newFakeOTASource(map[string]string{"mdb": "idle", "dbc": "idle"})
	q := Queued{MDB: true, DBC: true}

	var mu sync.Mutex
	var seen [][]string
	onPending := func(pending []string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, pending)
	}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second, 10*time.Second, onPending)
	}()

	src.push("mdb", "installing")
	src.push("mdb", "pending-reboot")
	src.push("dbc", "installing")
	src.push("dbc", "pending-reboot")
	src.push("dbc", "idle")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for awaiter to return")
	}

	mu.Lock()
	defer mu.Unlock()
	want := [][]string{{"dbc", "mdb"}, {"dbc"}}
	if len(seen) != len(want) {
		t.Fatalf("expected %d onPending calls, got %d: %v", len(want), len(seen), seen)
	}
	for i := range want {
		if strings.Join(seen[i], ",") != strings.Join(want[i], ",") {
			t.Errorf("call %d: expected %v, got %v", i, want[i], seen[i])
		}
	}
}

func TestWaitForCompletion_OnPendingNotCalledWhenNothingQueued(t *testing.T) {
	src := newFakeOTASource(nil)
	called := false
	err := WaitForCompletion(context.Background(), src, Queued{}, time.Second, 10*time.Second, func([]string) {
		called = true
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if called {
		t.Error("onPending should not be called when nothing is queued")
	}
}

// Regression tests for the 063513 bench deadlock: a slow MDB delta
// install (~12 min) outlived the awaiter's fixed 10-minute wait, so the
// awaiter gave up without rebooting and intentionally retained the
// reboot-owner:mdb claim. update-service triggers the MDB reboot only
// once at install completion, so the completed install sat at
// pending-reboot forever. The awaiter must keep waiting while an
// install is genuinely still in progress.

func TestWaitForCompletion_ExtendsWhileInstallInProgress(t *testing.T) {
	// Simulates a slow delta install: still "installing" when the
	// liveness window expires, completing (pending-reboot) only after
	// what would previously have been past the timeout.
	src := newFakeOTASource(map[string]string{"mdb": "idle"})
	q := Queued{MDB: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 100*time.Millisecond, 10*time.Second, nil)
	}()

	// Install starts before the first window expires.
	src.push("mdb", "downloading")
	src.push("mdb", "installing")

	// Wait well past the 100ms window. The old fixed-timeout behavior
	// returned DeadlineExceeded here; the new behavior extends the
	// window because the install is genuinely running.
	select {
	case err := <-done:
		t.Fatalf("awaiter gave up while install was in progress: err=%v", err)
	case <-time.After(250 * time.Millisecond):
	}

	// The slow delta apply finishes and update-service reports
	// pending-reboot; the awaiter must complete and let the caller
	// trigger the reboot.
	src.push("mdb", "pending-reboot")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for completion after extension")
	}
}

func TestWaitForCompletion_OverallCapStopsExtension(t *testing.T) {
	// A permanently "installing" status must not hold the wait (and
	// the reboot-ownership claim) forever: the overall cap ends it.
	src := newFakeOTASource(map[string]string{"mdb": "idle"})
	q := Queued{MDB: true}

	src.push("mdb", "installing")

	start := time.Now()
	err := WaitForCompletion(context.Background(), src, q, 50*time.Millisecond, 150*time.Millisecond, nil)
	elapsed := time.Since(start)

	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("returned before the overall cap elapsed: %v", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("cap did not stop the wait: %v", elapsed)
	}
}

func TestWaitForCompletion_DBCPendingRebootExtends(t *testing.T) {
	// A DBC at pending-reboot is mid-flight (local reboot, verify,
	// commit): window expiry there must extend, and the final idle
	// must still complete the wait.
	src := newFakeOTASource(map[string]string{"dbc": "idle"})
	q := Queued{DBC: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 100*time.Millisecond, 10*time.Second, nil)
	}()

	src.push("dbc", "installing")
	src.push("dbc", "pending-reboot")

	select {
	case err := <-done:
		t.Fatalf("awaiter gave up while DBC was verifying its commit: err=%v", err)
	case <-time.After(250 * time.Millisecond):
	}

	src.push("dbc", "idle")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DBC final idle after extension")
	}
}

func TestWaitForCompletion_NoExtensionWithoutActivity(t *testing.T) {
	// If the install never starts (status stays idle), the window
	// expiry must still end the wait promptly — the old fail-safe
	// timeout path for a dead or stuck update-service.
	src := newFakeOTASource(map[string]string{"mdb": "idle"})
	q := Queued{MDB: true}

	start := time.Now()
	err := WaitForCompletion(context.Background(), src, q, 100*time.Millisecond, time.Minute, nil)
	elapsed := time.Since(start)

	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("window expiry without install activity did not end the wait: %v", elapsed)
	}
}

func TestInstallInProgress(t *testing.T) {
	cases := []struct {
		name       string
		initial    map[string]string
		components []string
		want       bool
	}{
		{"mdb installing", map[string]string{"mdb": "installing"}, []string{"mdb"}, true},
		{"mdb downloading", map[string]string{"mdb": "downloading"}, []string{"mdb"}, true},
		{"mdb preparing", map[string]string{"mdb": "preparing"}, []string{"mdb"}, true},
		{"mdb idle", map[string]string{"mdb": "idle"}, []string{"mdb"}, false},
		{"mdb pending-reboot is not activity", map[string]string{"mdb": "pending-reboot"}, []string{"mdb"}, false},
		{"mdb error", map[string]string{"mdb": "error"}, []string{"mdb"}, false},
		{"dbc pending-reboot is activity", map[string]string{"dbc": "pending-reboot"}, []string{"dbc"}, true},
		{"dbc idle", map[string]string{"dbc": "idle"}, []string{"dbc"}, false},
		{"empty status", map[string]string{"mdb": ""}, []string{"mdb"}, false},
		{"missing status", map[string]string{}, []string{"mdb"}, false},
		{"dbc installing among idle mdb", map[string]string{"mdb": "idle", "dbc": "installing"}, []string{"mdb", "dbc"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newFakeOTASource(tc.initial)
			if got := installInProgress(src, tc.components); got != tc.want {
				t.Errorf("installInProgress(%v) = %v, want %v", tc.components, got, tc.want)
			}
		})
	}
}

func TestInstallInProgress_SourceErrorFailsClosed(t *testing.T) {
	// A read error must count as not-in-progress so the awaiter falls
	// back to its retain-the-claim timeout path instead of extending
	// on unknown state.
	src := &errorOTASource{}
	if installInProgress(src, []string{"mdb"}) {
		t.Error("installInProgress returned true on source error, want false")
	}
}

// errorOTASource is an OTAStatusSource whose Current always fails.
type errorOTASource struct{}

func (errorOTASource) Current(string) (string, error) {
	return "", errors.New("redis unavailable")
}
func (errorOTASource) Changes() <-chan StatusUpdate {
	return make(chan StatusUpdate)
}
func (errorOTASource) Stop() {}
