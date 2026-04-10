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
	entries      []string
	client       *ipc.Client
	lastProgress int
	lastDetail   string
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

// SetProgress publishes the current per-file progress (0..100) on the
// `usb` hash. The UI's UsbStore picks this up and drives a progress bar.
// Cheap enough to call from a progress callback after every chunk, but
// coalesces to no-op when the percentage hasn't changed.
func (l *Logger) SetProgress(pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if pct == l.lastProgress {
		return
	}
	l.lastProgress = pct
	if l.client == nil {
		return
	}
	if err := l.client.HSet("usb", "progress", fmt.Sprintf("%d", pct)); err != nil {
		log.Printf("umslog: SetProgress HSet error: %v", err)
	}
}

// SetDetail publishes a short human-readable sub-step description on
// `usb.detail`, e.g. "map.mbtiles (120/380 MB)". Pass empty string to
// clear.
func (l *Logger) SetDetail(msg string) {
	if msg == l.lastDetail {
		return
	}
	l.lastDetail = msg
	if l.client == nil {
		return
	}
	if err := l.client.HSet("usb", "detail", msg); err != nil {
		log.Printf("umslog: SetDetail HSet error: %v", err)
	}
}

// ClearProgress resets both `usb.progress` and `usb.detail` at phase
// boundaries so stale values don't persist on the UI.
func (l *Logger) ClearProgress() {
	l.SetProgress(0)
	l.SetDetail("")
}

// ProgressCallback returns a closure suitable for handing to
// dbc.TransferFile. It updates `usb.progress` as a percentage and
// `usb.detail` as "<label> (sent/total MB)".
func (l *Logger) ProgressCallback(label string) func(sent, total int64) {
	return func(sent, total int64) {
		if total > 0 {
			l.SetProgress(int(sent * 100 / total))
		}
		l.SetDetail(fmt.Sprintf("%s (%d/%d MB)", label, sent/(1024*1024), total/(1024*1024)))
	}
}

func (l *Logger) WriteToFile(path string) error {
	content := strings.Join(l.entries, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0644)
}
