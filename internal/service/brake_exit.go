package service

import (
	"log"

	ipc "github.com/librescoot/redis-ipc"
)

func (s *Service) startBrakeExitListener() error {
	sub, err := ipc.Subscribe(s.client, "input-events", func(event string) error {
		if event != "brake:left:hold" {
			return nil
		}

		s.mu.Lock()
		currentMode := s.usbCtrl.GetCurrentMode()
		s.mu.Unlock()

		if currentMode != "ums" {
			return nil
		}

		log.Println("Left brake hold detected, exiting UMS mode")

		s.mu.Lock()
		defer s.mu.Unlock()
		s.doSwitchToNormal()

		return nil
	})
	if err != nil {
		return err
	}

	s.brakeExitSub = sub
	return nil
}
