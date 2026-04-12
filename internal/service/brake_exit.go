package service

import (
	"log"

	ipc "github.com/librescoot/redis-ipc"
)

func (s *Service) startBrakeExitListener() error {
	_, err := ipc.Subscribe(s.client, "input-events", func(event string) error {
		s.mu.Lock()
		currentMode := s.usbCtrl.GetCurrentMode()
		s.mu.Unlock()

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
