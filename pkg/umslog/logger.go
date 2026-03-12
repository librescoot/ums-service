package umslog

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Logger struct {
	entries []string
}

func New() *Logger {
	return &Logger{}
}

func (l *Logger) timestamp() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func (l *Logger) Log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	l.entries = append(l.entries, fmt.Sprintf("%s %s", l.timestamp(), msg))
}

func (l *Logger) Logf(category, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	l.entries = append(l.entries, fmt.Sprintf("%s [%s] %s", l.timestamp(), category, msg))
}

func (l *Logger) Error(category, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	l.entries = append(l.entries, fmt.Sprintf("%s [%s] ERROR: %s", l.timestamp(), category, msg))
}

func (l *Logger) WriteToFile(path string) error {
	content := strings.Join(l.entries, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0644)
}
