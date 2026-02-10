package redis

import (
	"context"
	"fmt"
	"log"

	"github.com/go-redis/redis/v8"
)

type Publisher struct {
	client *redis.Client
}

func NewPublisher(addr, password string, db int) (*Publisher, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &Publisher{client: client}, nil
}

func (p *Publisher) PushDBCUpdate(filePath string) error {
	return p.pushUpdate("scooter:update:dbc", filePath)
}

func (p *Publisher) PushMDBUpdate(filePath string) error {
	return p.pushUpdate("scooter:update:mdb", filePath)
}

func (p *Publisher) pushUpdate(queue, filePath string) error {
	ctx := context.Background()
	result, err := p.client.LPush(ctx, queue, fmt.Sprintf("update-from-file:%s", filePath)).Result()
	if err != nil {
		return fmt.Errorf("failed to push update to %s: %w", queue, err)
	}
	log.Printf("Pushed update to %s (queue length: %d): %s", queue, result, filePath)
	return nil
}
