package config

import (
	"log"
	"os"
	"time"
)

type Config struct {
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	USBDriveFile  string
	USBDriveSize  int64

	// Per-operation timeouts for DBC transfers. These wrap the entire
	// upload (HTTP PUT + SCP fallback) for one file, so they need to
	// fit the slow path. Override via env.
	MapTransferTimeout    time.Duration
	RPMTransferTimeout    time.Duration
	ScriptTransferTimeout time.Duration
	MenderTransferTimeout time.Duration
}

func New() *Config {
	return &Config{
		RedisAddr:             getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:         getEnv("REDIS_PASSWORD", ""),
		RedisDB:               0,
		USBDriveFile:          "/data/usb.drive",
		USBDriveSize:          1024 * 1024 * 1024, // 1GB
		MapTransferTimeout:    getDuration("UMS_MAP_TIMEOUT", 10*time.Minute),
		RPMTransferTimeout:    getDuration("UMS_RPM_TIMEOUT", 5*time.Minute),
		ScriptTransferTimeout: getDuration("UMS_SCRIPT_TIMEOUT", 2*time.Minute),
		MenderTransferTimeout: getDuration("UMS_MENDER_TIMEOUT", 15*time.Minute),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getDuration(key string, defaultValue time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultValue
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("config: bad %s=%q: %v, using default %s", key, raw, err, defaultValue)
		return defaultValue
	}
	return d
}
