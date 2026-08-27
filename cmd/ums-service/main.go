package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/librescoot/ums-service/internal/service"
	"github.com/librescoot/ums-service/pkg/config"
)

// Injected at build time from git describe.
var version = "dev"

func main() {
	// Scanned rather than parsed with flag: an unrecognised argument used to
	// start the service anyway, and flag would instead make it fatal.
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-version" {
			fmt.Println(version)
			return
		}
	}

	if os.Getenv("JOURNAL_STREAM") != "" {
		log.SetFlags(0)
	} else {
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	}

	log.Printf("ums-service %s", version)

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
