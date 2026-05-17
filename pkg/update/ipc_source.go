package update

import (
	"errors"
	"fmt"
	"log"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/redis/go-redis/v9"
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
				// Channel full; drop. The status sequence per
				// install is short (a handful of events) and
				// the buffer is sized generously. If this fires
				// something is stuck upstream and the awaiter's
				// timeout will catch it.
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
		if errors.Is(err, redis.Nil) {
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
	if err := s.watcher.Stop(); err != nil {
		log.Printf("ota source: stop watcher: %v", err)
	}
}
