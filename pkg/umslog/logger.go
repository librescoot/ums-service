package umslog

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const redisKey = "usb:log"
const maxEntries = 100

// Logger collects timestamped entries during USB processing.
// Entries are pushed to Redis in real-time and written to a file at the end.
type Logger struct {
	entries []string
	client  *ipc.Client
}

func New(client *ipc.Client) *Logger {
	return &Logger{client: client}
}

func (l *Logger) timestamp() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func (l *Logger) push(entry string) {
	l.entries = append(l.entries, entry)

	if l.client == nil {
		return
	}

	if _, err := l.client.LPush(redisKey, entry); err != nil {
		log.Printf("umslog: LPush error: %v", err)
		return
	}

	if len(l.entries)%10 == 0 {
		if _, err := l.client.Do("LTRIM", redisKey, 0, maxEntries-1); err != nil {
			log.Printf("umslog: LTRIM error: %v", err)
		}
	}
}

func (l *Logger) Log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	l.push(fmt.Sprintf("%s %s", l.timestamp(), msg))
}

func (l *Logger) Logf(category, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	l.push(fmt.Sprintf("%s [%s] %s", l.timestamp(), category, msg))
}

func (l *Logger) Error(category, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	l.push(fmt.Sprintf("%s [%s] ERROR: %s", l.timestamp(), category, msg))
}

func (l *Logger) WriteToFile(path string) error {
	content := strings.Join(l.entries, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0644)
}
