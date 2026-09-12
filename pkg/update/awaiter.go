package update

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Component status values mirror update-service/internal/status.
const (
	statusIdle          = "idle"
	statusPendingReboot = "pending-reboot"
	// statusStagedNoop: update-service found nothing applicable to install.
	statusStagedNoop = "staged-noop"
	statusError      = "error"
)

// ErrorSettleDuration is how long a watched component must remain in
// error status before the wait concludes the install actually failed.
// update-service has a known transient error flip in the reboot window
// (observed on the bench: pending-reboot -> error -> pending-reboot ->
// idle with the install and commit succeeding), so an error alone is
// not a failure verdict. Tests shorten this to keep runs fast.
var ErrorSettleDuration = 30 * time.Second

// StatusUpdate is a change observed on one component's OTA status.
type StatusUpdate struct {
	Component string // "mdb" or "dbc"
	Status    string
}

// OTAStatusSource is the small surface the awaiter needs from a Redis
// hash watcher. Production uses ipcOTASource; tests use a fake.
type OTAStatusSource interface {
	// Current returns the current status for a component ("mdb" or
	// "dbc"). Returns ("", nil) if the field isn't set.
	Current(component string) (string, error)
	// Changes returns a channel of status updates. The channel may be
	// closed if the underlying watcher stops; the awaiter treats a
	// closed channel as a fatal error.
	Changes() <-chan StatusUpdate
	// Stop releases any subscription resources.
	Stop()
}

// awaiterState tracks per-component progress through the install
// lifecycle as observed via the ota hash.
type awaiterState struct {
	// sawNonPendingReboot becomes true once we observe a status
	// other than pending-reboot. Required to ignore a stale
	// pending-reboot left from a prior install: we want to see the
	// status leave pending-reboot (downloading/installing/error)
	// before counting a subsequent pending-reboot as ours.
	sawNonPendingReboot bool
	// sawPendingReboot is used by DBC installs, which remain unfinished
	// until update-service reboots the DBC and reports its final idle or
	// error status, including when an MDB update is queued alongside it.
	sawPendingReboot bool
	// done becomes true at pending-reboot for MDB-driven installs, or
	// at the final idle status for a DBC-only install.
	done bool
	// errorAt is the time an error status was last observed for the
	// component, or zero while the component is not (or no longer) in
	// error. A later transition away from error clears it; the settle
	// timer only fails the wait when an error has persisted for
	// ErrorSettleDuration.
	errorAt time.Time
}

// installInProgress reports whether any of the given components is
// currently showing a status that means an install is genuinely still
// running: downloading, preparing, or installing. A DBC at
// pending-reboot is also mid-flight: update-service reboots it locally
// and the component only reaches its final state after the post-reboot
// verification and commit. An MDB at pending-reboot is not treated as
// active — for MDB that status is the completion signal, so observing
// it at window expiry means the install never started this cycle (a
// stale leftover), not that it is running. A read error counts as
// not-in-progress so the awaiter fails closed into its old
// retain-the-claim timeout path.
func installInProgress(source OTAStatusSource, components []string) bool {
	for _, c := range components {
		st, err := source.Current(c)
		if err != nil || st == "" {
			continue
		}
		switch st {
		case "downloading", "preparing", "installing":
			return true
		case statusPendingReboot:
			if c == "dbc" {
				return true
			}
		}
		// statusStagedNoop deliberately falls through: it is not install activity.
	}
	return false
}

// WaitForCompletion blocks until every component in q with its bool set
// has completed the relevant install lifecycle. MDB installs complete at
// pending-reboot so the caller can trigger the MDB reboot. DBC installs
// complete only when update-service reports idle after its local reboot and
// verified commit; this also applies to combined MDB+DBC installs. A
// post-pending-reboot error fails the wait.
//
// windowTimeout is a liveness window, not a total budget: when it
// expires while a watched component still shows genuine install
// activity (see installInProgress), the window resets and the wait
// continues, so a slow delta install is not abandoned mid-flight. The
// wait gives up for good once overallCap has elapsed, or as soon as a
// window expires with no watched component actively installing (the
// install never started or is stuck). On give-up the caller retains
// whatever ownership claims it holds, preserving the existing
// fail-safe semantics.
//
// onPending receives the sorted, non-empty set of unfinished components.
//
// A watched component entering error status does not fail the wait
// immediately: the error must persist for ErrorSettleDuration before
// the wait concludes failure, so a transient flip followed by recovery
// (pending-reboot, idle) keeps the wait alive.
//
// Returns nil on success, an error wrapping context.DeadlineExceeded on
// final timeout, an error wrapping context.Canceled on ctx cancellation,
// or an error naming the component whose error status persisted past
// the settle duration.
func WaitForCompletion(ctx context.Context, source OTAStatusSource, q Queued, windowTimeout, overallCap time.Duration, onPending func([]string)) error {
	required := RequiredComponents(q)
	if len(required) == 0 {
		return nil
	}

	waitForDBCFinal := q.DBC
	states := make(map[string]*awaiterState, len(required))
	for _, c := range required {
		st := &awaiterState{}
		initial, err := source.Current(c)
		if err != nil {
			return fmt.Errorf("read initial status for %s: %w", c, err)
		}
		if initial != "" && initial != statusPendingReboot {
			st.sawNonPendingReboot = true
		}
		states[c] = st
	}

	notify := func() {
		if onPending == nil {
			return
		}
		if p := pendingComponents(states); len(p) > 0 {
			onPending(p)
		}
	}
	notify()

	window := time.NewTimer(windowTimeout)
	defer window.Stop()
	// overallCap bounds the total wait across all extensions. Zero or
	// negative disables the cap; production always passes a positive
	// cap so a hung install cannot hold the reboot-ownership claim
	// forever.
	var overall time.Time
	if overallCap > 0 {
		overall = time.Now().Add(overallCap)
	}

	updates := source.Changes()
	// errorSettle fires ErrorSettleDuration after the earliest
	// un-cleared error observation, so a transient error flip does not
	// fail the wait while a genuinely stuck error still does.
	var settleTimer *time.Timer
	var settleC <-chan time.Time
	armSettle := func() {
		earliest := time.Time{}
		for _, st := range states {
			if !st.errorAt.IsZero() && (earliest.IsZero() || st.errorAt.Before(earliest)) {
				earliest = st.errorAt
			}
		}
		if earliest.IsZero() {
			if settleTimer != nil {
				settleTimer.Stop()
				settleTimer = nil
				settleC = nil
			}
			return
		}
		delay := earliest.Add(ErrorSettleDuration).Sub(time.Now())
		if delay < 0 {
			delay = 0
		}
		if settleTimer == nil {
			settleTimer = time.NewTimer(delay)
		} else {
			if !settleTimer.Stop() {
				select {
				case <-settleTimer.C:
				default:
				}
			}
			settleTimer.Reset(delay)
		}
		settleC = settleTimer.C
	}
	defer func() {
		if settleTimer != nil {
			settleTimer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for install completion: %w", ctx.Err())
		case <-window.C:
			if !overall.IsZero() && !time.Now().Before(overall) {
				return fmt.Errorf("waiting for install completion: %w", context.DeadlineExceeded)
			}
			if !installInProgress(source, required) {
				return fmt.Errorf("waiting for install completion: %w", context.DeadlineExceeded)
			}
			window.Reset(windowTimeout)
		case <-settleC:
			// The earliest observed error has had the settle duration to
			// recover. Fail only if the component is still in error; a
			// recovery event we somehow missed clears the state instead.
			failed := ""
			now := time.Now()
			for c, st := range states {
				if st.errorAt.IsZero() {
					continue
				}
				cur, err := source.Current(c)
				if err != nil || cur == statusError {
					if now.Sub(st.errorAt) >= ErrorSettleDuration {
						failed = c
						break
					}
				} else {
					st.errorAt = time.Time{}
				}
			}
			if failed != "" {
				return fmt.Errorf("install for %s reported error", failed)
			}
			armSettle()
		case u, ok := <-updates:
			if !ok {
				return fmt.Errorf("ota status source closed before completion")
			}
			st, watched := states[u.Component]
			if !watched {
				continue
			}
			switch u.Status {
			case statusStagedNoop:
				st.errorAt = time.Time{}
				st.sawNonPendingReboot = true
				if !st.done {
					st.done = true
					if allDone(states) {
						return nil
					}
					notify()
				}
			case statusPendingReboot:
				st.errorAt = time.Time{}
				if st.sawNonPendingReboot && !st.done {
					if waitForDBCFinal && u.Component == "dbc" {
						st.sawPendingReboot = true
						continue
					}
					st.done = true
					if allDone(states) {
						return nil
					}
					notify()
				}
			case statusError:
				if st.sawNonPendingReboot {
					// Do not fail yet: hold the error open for the settle
					// duration so a transient flip followed by recovery
					// keeps the wait alive (bench: pending-reboot ->
					// error -> pending-reboot -> idle, install fine).
					if st.errorAt.IsZero() {
						st.errorAt = time.Now()
						armSettle()
					}
					continue
				}
				// Pre-existing error before we saw any install
				// activity — treat as starting state, like idle.
				st.sawNonPendingReboot = true
			case statusIdle:
				st.errorAt = time.Time{}
				st.sawNonPendingReboot = true
				if waitForDBCFinal && u.Component == "dbc" && st.sawPendingReboot {
					st.done = true
					if allDone(states) {
						return nil
					}
					notify()
				}
			default:
				st.errorAt = time.Time{}
				st.sawNonPendingReboot = true
			}
		}
	}
}

// MDBRebootNeeded reports whether a queued MDB install reached pending-reboot.
func MDBRebootNeeded(q Queued, rec *RecordingSource) bool {
	return q.MDB && rec != nil && rec.Last("mdb") == statusPendingReboot
}

// RequiredComponents returns queued components in stable order.
func RequiredComponents(q Queued) []string {
	var out []string
	if q.DBC {
		out = append(out, "dbc")
	}
	if q.MDB {
		out = append(out, "mdb")
	}
	return out
}

func pendingComponents(states map[string]*awaiterState) []string {
	var out []string
	for c, st := range states {
		if !st.done {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func allDone(states map[string]*awaiterState) bool {
	for _, st := range states {
		if !st.done {
			return false
		}
	}
	return true
}
