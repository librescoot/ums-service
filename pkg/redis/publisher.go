package redis

import (
	"fmt"
	"log"

	ipc "github.com/librescoot/redis-ipc"
)

type Publisher struct {
	client *ipc.Client
}

func NewPublisher(addr, password string, db int) (*Publisher, error) {
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

	return &Publisher{client: client}, nil
}

func (p *Publisher) PushDBCUpdate(filePath string) error {
	return p.pushUpdate("scooter:update:dbc", filePath)
}

func (p *Publisher) PushMDBUpdate(filePath string) error {
	return p.pushUpdate("scooter:update:mdb", filePath)
}

func (p *Publisher) pushUpdate(queue, filePath string) error {
	_, err := p.client.LPush(queue, fmt.Sprintf("update-from-file:%s", filePath))
	if err != nil {
		return fmt.Errorf("failed to push update to %s: %w", queue, err)
	}
	log.Printf("Pushed update to %s: %s", queue, filePath)
	return nil
}
