package dbc

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Interface struct {
	ip         string
	port       int
	dataDir    string
	httpServer *http.Server
	enabled    bool
}

func New(dataDir string) *Interface {
	return &Interface{
		ip:      "192.168.7.2",
		port:    31337,
		dataDir: dataDir,
		enabled: false,
	}
}

func (i *Interface) Enable(ctx context.Context) error {
	if i.enabled {
		return nil
	}

	log.Println("Enabling DBC interface...")
	cmd := exec.Command("/usr/bin/keycard.sh")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run keycard.sh: %w", err)
	}

	// Wait for DBC to become reachable
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	
	timeout := time.After(60 * time.Second)
	
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for DBC to become reachable")
		case <-ticker.C:
			if i.isReachable() {
				i.enabled = true
				log.Println("DBC is now reachable")
				return i.startHTTPServer()
			}
		}
	}
}

func (i *Interface) Disable() error {
	if !i.enabled {
		return nil
	}

	log.Println("Disabling DBC interface...")
	
	if i.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := i.httpServer.Shutdown(ctx); err != nil {
			log.Printf("Error shutting down HTTP server: %v", err)
		}
		i.httpServer = nil
	}

	cmd := exec.Command("/usr/bin/keycard.sh")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run keycard.sh to disable: %w", err)
	}

	i.enabled = false
	return nil
}

func (i *Interface) isReachable() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:22", i.ip), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (i *Interface) startHTTPServer() error {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(i.dataDir)))

	i.httpServer = &http.Server{
		Addr:    fmt.Sprintf("192.168.7.1:%d", i.port),
		Handler: mux,
	}

	go func() {
		log.Printf("Starting HTTP server on port %d serving %s", i.port, i.dataDir)
		if err := i.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	return nil
}

func (i *Interface) DownloadFile(localPath, remotePath string) error {
	if !i.enabled {
		return fmt.Errorf("DBC interface not enabled")
	}

	filename := filepath.Base(localPath)
	url := fmt.Sprintf("http://192.168.7.1:%d/%s", i.port, filename)

	cmd := exec.Command("ssh", 
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		fmt.Sprintf("root@%s", i.ip),
		fmt.Sprintf("wget -O %s %s", remotePath, url))
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to download file via SSH: %v, output: %s", err, string(output))
	}

	log.Printf("Downloaded %s to DBC at %s", filename, remotePath)
	return nil
}

func (i *Interface) CopyFile(localPath, remotePath string) error {
	if !i.enabled {
		return fmt.Errorf("DBC interface not enabled")
	}

	cmd := exec.Command("scp",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		localPath,
		fmt.Sprintf("root@%s:%s", i.ip, remotePath))
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to copy file: %v, output: %s", err, string(output))
	}

	log.Printf("Copied %s to DBC at %s", localPath, remotePath)
	return nil
}

func (i *Interface) RunCommand(command string) (string, error) {
	if !i.enabled {
		return "", fmt.Errorf("DBC interface not enabled")
	}

	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		fmt.Sprintf("root@%s", i.ip),
		command)
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run command: %v, output: %s", err, string(output))
	}

	return strings.TrimSpace(string(output)), nil
}

func (i *Interface) IsEnabled() bool {
	return i.enabled
}