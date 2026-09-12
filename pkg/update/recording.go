package update

import "sync"

// RecordingSource wraps an OTAStatusSource and remembers what it saw:
// the last observed status per component and every status ever
// observed for a component. The awaiter uses it after a failed wait to
// distinguish "the install failed" from "the wait gave up but the
// installs actually recovered" (see InstallRecovered) without
// duplicating WaitForCompletion's lifecycle state machine.
type RecordingSource struct {
	OTAStatusSource

	mu   sync.Mutex
	last map[string]string
	seen map[string]map[string]bool
	done chan struct{}
}

// NewRecordingSource wraps src. The wrapper forwards every status
// update to its consumer while recording it; Stop releases both the
// wrapper's pump and the wrapped source.
func NewRecordingSource(src OTAStatusSource) *RecordingSource {
	return &RecordingSource{
		OTAStatusSource: src,
		last:            make(map[string]string),
		seen:            make(map[string]map[string]bool),
		done:            make(chan struct{}),
	}
}

func (r *RecordingSource) record(u StatusUpdate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last[u.Component] = u.Status
	seen := r.seen[u.Component]
	if seen == nil {
		seen = make(map[string]bool)
		r.seen[u.Component] = seen
	}
	seen[u.Status] = true
}

// Last returns the most recent status observed for component, or ""
// if none was observed yet.
func (r *RecordingSource) Last(component string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[component]
}

// Saw reports whether status was ever observed for component.
func (r *RecordingSource) Saw(component, status string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[component][status]
}

// Changes returns a channel of recorded-then-forwarded status updates.
func (r *RecordingSource) Changes() <-chan StatusUpdate {
	out := make(chan StatusUpdate, 16)
	inner := r.OTAStatusSource.Changes()
	go func() {
		defer close(out)
		for {
			select {
			case <-r.done:
				return
			case u, ok := <-inner:
				if !ok {
					return
				}
				r.record(u)
				select {
				case out <- u:
				case <-r.done:
					return
				}
			}
		}
	}()
	return out
}

// Stop releases the wrapper's pump and the wrapped source.
func (r *RecordingSource) Stop() {
	close(r.done)
	r.OTAStatusSource.Stop()
}

// InstallRecovered reports whether every queued component actually
// reached the completed state its lifecycle requires, based on the
// recording source's history. An MDB is complete when its current
// status is pending-reboot: that status only exists after a finished
// install, so a stale pre-cycle idle cannot be mistaken for
// completion. A DBC is complete when idle was observed after its own
// pending-reboot, mirroring WaitForCompletion's rule that a DBC
// install only finishes after the local reboot and verified commit.
//
// Use this on a wait failure to decide whether the failure was real
// (retain any ownership claims) or the wait merely gave up while the
// installs recovered (proceed as if the wait had succeeded).
func InstallRecovered(rec *RecordingSource, q Queued) bool {
	if q.MDB && rec.Last("mdb") != statusPendingReboot {
		return false
	}
	if q.DBC && !(rec.Last("dbc") == statusIdle && rec.Saw("dbc", statusPendingReboot)) {
		return false
	}
	return true
}
