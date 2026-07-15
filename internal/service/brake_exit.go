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

		// Flag first: switchToUMS holds mu for the whole preparing phase,
		// so the Lock below parks us until entry has finished. Recording
		// the hold beforehand lets switchToUMS see it and abandon entry.
		s.cancelPending.Store(true)

		s.mu.Lock()
		currentMode := s.usbCtrl.GetCurrentMode()
		s.mu.Unlock()

		// Entry we just cancelled never reached UMS, so there is nothing
		// to switch back; switchToUMS already returned things to idle.
		if currentMode != "ums" {
			return nil
		}

		log.Println("Left brake hold detected, exiting UMS mode")

		s.mu.Lock()
		s.doSwitchToNormal()
		s.mu.Unlock()

		return nil
	})
	return err
}
