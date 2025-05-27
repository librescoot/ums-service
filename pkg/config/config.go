package config

import (
	"os"
)

type Config struct {
	RedisAddr        string
	RedisPassword    string
	RedisDB          int
	USBDriveFile     string
	USBDriveSize     int64
}

func New() *Config {
	return &Config{
		RedisAddr:        getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:    getEnv("REDIS_PASSWORD", ""),
		RedisDB:          0,
		USBDriveFile:     "/data/usb.drive",
		USBDriveSize:     1024 * 1024 * 1024, // 1GB
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}