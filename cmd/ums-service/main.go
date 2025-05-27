package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/librescoot/ums-service/internal/service"
	"github.com/librescoot/ums-service/pkg/config"
)

func main() {
	cfg := config.New()

	svc, err := service.New(cfg)
	if err != nil {
		log.Fatalf("Failed to create service: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		cancel()
	}()

	if err := svc.Run(ctx); err != nil {
		log.Fatalf("Service error: %v", err)
	}
}