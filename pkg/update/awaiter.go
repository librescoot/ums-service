package update

import (
	"context"
	"fmt"
	"time"
)

// Component status values mirror update-service/internal/status.
const (
	statusPendingReboot = "pending-reboot"
	statusError         = "error"
)

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
	// done becomes true when pending-reboot is observed after
	// sawNonPendingReboot is true.
	done bool
}

// WaitForCompletion blocks until every component in q with its bool set
// has transitioned to pending-reboot since the function was entered.
//
// Returns nil on success, an error wrapping context.DeadlineExceeded on
// timeout, an error wrapping context.Canceled on ctx cancellation, or an
// error naming the component that went to error status.
func WaitForCompletion(ctx context.Context, source OTAStatusSource, q Queued, timeout time.Duration) error {
	required := requiredComponents(q)
	if len(required) == 0 {
		return nil
	}

	states := make(map[string]*awaiterState, len(required))
	for _, c := range required {
		st := &awaiterState{}
		initial, err := source.Current(c)
		if err == nil && initial != "" && initial != statusPendingReboot {
			st.sawNonPendingReboot = true
		}
		states[c] = st
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	updates := source.Changes()
	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("waiting for install completion: %w", waitCtx.Err())
		case u, ok := <-updates:
			if !ok {
				return fmt.Errorf("ota status source closed before completion")
			}
			st, watched := states[u.Component]
			if !watched {
				continue
			}
			switch u.Status {
			case statusPendingReboot:
				if st.sawNonPendingReboot {
					st.done = true
					if allDone(states) {
						return nil
					}
				}
			case statusError:
				if st.sawNonPendingReboot {
					return fmt.Errorf("install for %s reported error", u.Component)
				}
				// Pre-existing error before we saw any install
				// activity — treat as starting state, like idle.
				st.sawNonPendingReboot = true
			default:
				st.sawNonPendingReboot = true
			}
		}
	}
}

func requiredComponents(q Queued) []string {
	var out []string
	if q.MDB {
		out = append(out, "mdb")
	}
	if q.DBC {
		out = append(out, "dbc")
	}
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
