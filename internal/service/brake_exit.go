package service

import (
	"log"

	ipc "github.com/librescoot/redis-ipc"
)

func (s *Service) startBrakeExitListener() error {
	_, err := ipc.Subscribe(s.client, "input-events", func(event string) error {
		s.mu.Lock()
		cur := s.currentOp
		active := cur != nil && isUMSTarget(cur.target)
		s.mu.Unlock()

		if !active {
			return nil
		}

		log.Println("Left brake hold detected, exiting UMS mode")
		s.exitToNormal()
		return nil
	})
	return err
}
