package update

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeOTASource is a deterministic OTAStatusSource for testing.
type fakeOTASource struct {
	initial map[string]string
	updates chan StatusUpdate
}

func newFakeOTASource(initial map[string]string) *fakeOTASource {
	return &fakeOTASource{
		initial: initial,
		updates: make(chan StatusUpdate, 32),
	}
}

func (f *fakeOTASource) Current(component string) (string, error) {
	return f.initial[component], nil
}

func (f *fakeOTASource) Changes() <-chan StatusUpdate {
	return f.updates
}

func (f *fakeOTASource) Stop() {}

func (f *fakeOTASource) push(component, status string) {
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
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second)
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

func TestWaitForCompletion_DBCOnly_HappyPath(t *testing.T) {
	src := newFakeOTASource(map[string]string{"dbc": "idle"})
	q := Queued{DBC: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second)
	}()

	src.push("dbc", "installing")
	src.push("dbc", "pending-reboot")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for awaiter to return")
	}
}

func TestWaitForCompletion_Both_BothMustComplete(t *testing.T) {
	src := newFakeOTASource(map[string]string{"mdb": "idle", "dbc": "idle"})
	q := Queued{MDB: true, DBC: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second)
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
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for awaiter to return after both complete")
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
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second)
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
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second)
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
		done <- WaitForCompletion(context.Background(), src, q, 2*time.Second)
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
	err := WaitForCompletion(context.Background(), src, q, 100*time.Millisecond)
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
		done <- WaitForCompletion(ctx, src, q, 5*time.Second)
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

	err := WaitForCompletion(context.Background(), src, q, 1*time.Second)
	if err != nil {
		t.Errorf("expected nil error for empty Queued, got %v", err)
	}
}

func TestWaitForCompletion_SourceClosed(t *testing.T) {
	src := newFakeOTASource(map[string]string{"mdb": "idle"})
	q := Queued{MDB: true}

	done := make(chan error, 1)
	go func() {
		done <- WaitForCompletion(context.Background(), src, q, 5*time.Second)
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
