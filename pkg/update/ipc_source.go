package update

import (
	"fmt"
	"strings"

	ipc "github.com/librescoot/redis-ipc"
)

// ipcOTASource adapts an ipc.HashWatcher on the "ota" hash to the
// OTAStatusSource interface used by WaitForCompletion. Watches the
// "status:mdb" and "status:dbc" fields.
type ipcOTASource struct {
	client  *ipc.Client
	watcher *ipc.HashWatcher
	updates chan StatusUpdate
}

// NewIPCOTASource subscribes to the "ota" hash and returns a source the
// awaiter can read from. Call Stop when done.
//
// IMPORTANT: this subscribes synchronously. Construct it BEFORE
// LPushing the install command so the install→pending-reboot
// transition can't be missed.
func NewIPCOTASource(client *ipc.Client) (*ipcOTASource, error) {
	s := &ipcOTASource{
		client:  client,
		watcher: client.NewHashWatcher("ota"),
		updates: make(chan StatusUpdate, 32),
	}

	for _, component := range []string{"mdb", "dbc"} {
		field := "status:" + component
		comp := component
		s.watcher.OnField(field, func(value string) error {
			select {
			case s.updates <- StatusUpdate{Component: comp, Status: value}:
			default:
				// Channel full — drop. The awaiter polls Current
				// on next iteration if it needs the latest.
			}
			return nil
		})
	}

	if err := s.watcher.Start(); err != nil {
		return nil, fmt.Errorf("subscribe to ota hash: %w", err)
	}
	return s, nil
}

func (s *ipcOTASource) Current(component string) (string, error) {
	val, err := s.client.HGet("ota", "status:"+component)
	if err != nil {
		// redis-ipc reports field-not-found as an error; treat that
		// as "" so the awaiter sees an idle/blank starting state.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "nil") {
			return "", nil
		}
		return "", err
	}
	return val, nil
}

func (s *ipcOTASource) Changes() <-chan StatusUpdate {
	return s.updates
}

func (s *ipcOTASource) Stop() {
	s.watcher.Stop()
}
