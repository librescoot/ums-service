package redis

import (
	"context"
	"fmt"
	"log"
	"strings"

	ipc "github.com/librescoot/redis-ipc"
)

type ModeHandler func(mode string) error

type Subscriber struct {
	watcher *ipc.HashWatcher
}

func NewSubscriber(addr, password string, _ string, db int) (*Subscriber, error) {
	opts := []ipc.Option{
		ipc.WithAddress(addr),
		ipc.WithPort(6379),
	}
	if password != "" {
		opts = append(opts, ipc.WithURL(fmt.Sprintf("redis://:%s@%s:%d", password, addr, 6379)))
	}

	client, err := ipc.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Redis client: %w", err)
	}

	sub := &Subscriber{
		watcher: client.NewHashWatcher("usb"),
	}

	sub.watcher.OnField("mode", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "ums" || value == "normal" {
			log.Printf("Mode changed to: %s", value)
			return nil
		}
		log.Printf("Invalid mode value: %s", value)
		return nil
	})

	return sub, nil
}

func (s *Subscriber) SetModeHandler(handler ModeHandler) {
	s.watcher.OnField("mode", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "ums" || value == "normal" {
			log.Printf("Mode changed to: %s", value)
			return handler(value)
		}
		log.Printf("Invalid mode value: %s", value)
		return nil
	})
}

func (s *Subscriber) Subscribe(ctx context.Context) error {
	s.watcher.StartWithSync()
	<-ctx.Done()
	return ctx.Err()
}

func (s *Subscriber) Close() error {
	s.watcher.Stop()
	return nil
}
