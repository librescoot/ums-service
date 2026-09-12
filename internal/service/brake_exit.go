package service

import (
	"log"

	ipc "github.com/librescoot/redis-ipc"
)

// vehicle-service publishes namespaced gestures on "input-events", e.g.
// brake:right:press or seatbox:tap. Only a 3s left brake hold exits UMS.
const umsExitEvent = "brake:left:hold"

func (s *Service) startBrakeExitListener() error {
	_, err := ipc.Subscribe(s.client, "input-events", func(event string) error {
		if event != umsExitEvent {
			return nil
		}

		s.mu.Lock()
		current := s.currentOp
		active := current != nil && isUMSTarget(current.target)
		s.mu.Unlock()
		if !active {
			return nil
		}

		// The operation target covers preparation as well as an active
		// gadget, so a brake hold cancels an entry before UMS is attached.
		log.Println("Left brake hold detected, exiting UMS mode")
		s.doSwitchToNormal()

		return nil
	})
	return err
}
