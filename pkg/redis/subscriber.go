package redis

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/go-redis/redis/v8"
)

type ModeHandler func(mode string) error

type Subscriber struct {
	client      *redis.Client
	channel     string
	modeHandler ModeHandler
}

func NewSubscriber(addr, password, channel string, db int) (*Subscriber, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &Subscriber{
		client:  client,
		channel: channel,
	}, nil
}

func (s *Subscriber) SetModeHandler(handler ModeHandler) {
	s.modeHandler = handler
}

func (s *Subscriber) Subscribe(ctx context.Context) error {
	// Subscribe to the "usb" channel for PUBLISH messages
	pubsub := s.client.Subscribe(ctx, "usb")
	defer pubsub.Close()

	ch := pubsub.Channel()

	log.Printf("Subscribed to Redis channel: usb")

	// Check initial mode
	go s.handleModeChange(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-ch:
			// Only process if the payload is "mode"
			if msg.Payload == "mode" {
				log.Printf("Received mode change notification")
				go s.handleModeChange(ctx)
			}
		}
	}
}

func (s *Subscriber) handleModeChange(ctx context.Context) {
	// Get the mode from the "usb" hash, field "mode"
	mode, err := s.client.HGet(ctx, "usb", "mode").Result()
	if err != nil {
		if err == redis.Nil {
			// Key doesn't exist, default to normal
			mode = "normal"
		} else {
			log.Printf("Error getting mode from Redis: %v", err)
			return
		}
	}

	mode = strings.TrimSpace(mode)
	if mode == "ums" || mode == "normal" {
		log.Printf("Mode changed to: %s", mode)
		if s.modeHandler != nil {
			if err := s.modeHandler(mode); err != nil {
				log.Printf("Error handling mode change: %v", err)
			}
		}
	} else {
		log.Printf("Invalid mode value: %s", mode)
	}
}

func (s *Subscriber) Close() error {
	return s.client.Close()
}