package diagnostics

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	dbcIP             = "192.168.7.2"
	dbcAddr           = dbcIP + ":22"
	journalMaxAge     = "8 hours ago"
	dbcCommandTimeout = 30 * time.Second
)

// HashReader reads a whole Redis hash. Satisfied by *ipc.Client.
type HashReader interface {
	HGetAll(key string) (map[string]string, error)
}

type Collector struct {
	hashes HashReader
}

// New builds a collector. hashes may be nil, in which case the Redis-derived
// sections of the system info dump are skipped.
func New(hashes HashReader) *Collector {
	return &Collector{hashes: hashes}
}

func (c *Collector) CollectToUSB(mountPoint string) {
	mdbDir := filepath.Join(mountPoint, "diagnostics", "mdb")
	if err := os.MkdirAll(mdbDir, 0755); err != nil {
		log.Printf("Failed to create MDB diagnostics directory: %v", err)
		return
	}

	c.collectMDB(mdbDir)

	if c.dbcReachable() {
		dbcDir := filepath.Join(mountPoint, "diagnostics", "dbc")
		if err := os.MkdirAll(dbcDir, 0755); err != nil {
			log.Printf("Failed to create DBC diagnostics directory: %v", err)
			return
		}
		c.collectDBC(dbcDir)
	} else {
		log.Println("DBC not reachable, skipping DBC diagnostics")
	}

	log.Println("Diagnostics collection complete")
}

func (c *Collector) dbcReachable() bool {
	conn, err := net.DialTimeout("tcp", dbcAddr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (c *Collector) collectMDB(dir string) {
	writeCommandOutput(dir, "journal.log", "journalctl", "--no-pager", "--since", journalMaxAge)
	writeCommandOutput(dir, "dmesg.log", "dmesg")
	c.writeMDBSystemInfo(dir)
}

func (c *Collector) collectDBC(dir string) {
	c.writeDBCCommand(dir, "journal.log", fmt.Sprintf("journalctl --no-pager --since '%s'", journalMaxAge))
	c.writeDBCCommand(dir, "dmesg.log", "dmesg")
	c.writeDBCSystemInfo(dir)
}

func (c *Collector) runDBCCommand(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbcCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx,
		"ssh", "-y",
		fmt.Sprintf("root@%s", dbcIP),
		command)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("ssh command timed out after %v", dbcCommandTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("ssh command failed: %v, output: %s", err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}

func (c *Collector) writeDBCCommand(dir, filename, command string) {
	output, err := c.runDBCCommand(command)
	if err != nil {
		log.Printf("Failed to collect DBC %s: %v", filename, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(output), 0644); err != nil {
		log.Printf("Failed to write DBC %s: %v", filename, err)
	}
}

func (c *Collector) writeDBCSystemInfo(dir string) {
	cmd := `printf '=== uptime ===\n'; uptime; printf '\n=== disk usage ===\n'; df -h; printf '\n=== memory ===\n'; free -m`
	output, err := c.runDBCCommand(cmd)
	if err != nil {
		log.Printf("Failed to collect DBC system info: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "system-info.txt"), []byte(output), 0644); err != nil {
		log.Printf("Failed to write DBC system-info.txt: %v", err)
	}
}

func (c *Collector) writeMDBSystemInfo(dir string) {
	sections := []struct {
		header string
		name   string
		args   []string
	}{
		{"uptime", "uptime", nil},
		{"disk usage", "df", []string{"-h"}},
		{"memory", "free", []string{"-m"}},
	}

	var content string
	for _, s := range sections {
		cmd := exec.Command(s.name, s.args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			content += fmt.Sprintf("=== %s ===\nERROR: %v\n\n", s.header, err)
			continue
		}
		out := string(output)
		if s.header == "installed packages" {
			out = truncateLines(out, 50)
		}
		content += fmt.Sprintf("=== %s ===\n%s\n", s.header, out)
	}

	content += c.modemSection()

	if err := os.WriteFile(filepath.Join(dir, "system-info.txt"), []byte(content), 0644); err != nil {
		log.Printf("Failed to write system-info.txt: %v", err)
	}
}

// modemRows lists what the modem section prints, in order, as
// {label, hash, field}. Identity first: IMEI and ICCID are what the
// connectivity onboarding flow asks people for, IMSI identifies the
// subscription. The rest is network and health context for support.
var modemRows = [][3]string{
	{"IMEI", "internet", "sim-imei"},
	{"ICCID", "internet", "sim-iccid"},
	{"IMSI", "internet", "sim-imsi"},
	{"operator", "modem", "operator-name"},
	{"operator code", "modem", "operator-code"},
	{"access tech", "internet", "access-tech"},
	{"signal quality", "internet", "signal-quality"},
	{"registration", "modem", "registration"},
	{"roaming", "modem", "is-roaming"},
	{"connectivity", "internet", "connectivity"},
	{"modem state", "internet", "modem-state"},
	{"internet status", "internet", "status"},
	{"ip address", "internet", "ip-address"},
	{"modem health", "internet", "modem-health"},
	{"power state", "modem", "power-state"},
	{"sim state", "modem", "sim-state"},
	{"sim lock", "modem", "sim-lock"},
	{"pin action", "modem", "pin-action"},
	{"apn action", "modem", "apn-action"},
	{"registration fail", "modem", "registration-fail"},
	{"error state", "modem", "error-state"},
}

func (c *Collector) modemSection() string {
	if c.hashes == nil {
		return "=== modem ===\nERROR: no Redis connection\n\n"
	}

	values := map[string]map[string]string{}
	for _, name := range []string{"internet", "modem"} {
		h, err := c.hashes.HGetAll(name)
		if err != nil {
			return fmt.Sprintf("=== modem ===\nERROR: reading %s hash: %v\n\n", name, err)
		}
		values[name] = h
	}

	width := 0
	for _, r := range modemRows {
		if n := len(r[0]) + 1; n > width { // +1 for the trailing colon
			width = n
		}
	}

	content := "=== modem ===\n"
	for _, r := range modemRows {
		v := values[r[1]][r[2]]
		if v == "" {
			v = "-"
		}
		content += fmt.Sprintf("%-*s  %s\n", width, r[0]+":", v)
	}
	return content + "\n"
}

func writeCommandOutput(dir, filename string, name string, args ...string) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to collect %s: %v", filename, err)
		output = []byte(fmt.Sprintf("ERROR: %v\n%s", err, string(output)))
	}
	if err := os.WriteFile(filepath.Join(dir, filename), output, 0644); err != nil {
		log.Printf("Failed to write %s: %v", filename, err)
	}
}

func truncateLines(s string, max int) string {
	lines := 0
	for i, ch := range s {
		if ch == '\n' {
			lines++
			if lines >= max {
				return s[:i+1]
			}
		}
	}
	return s
}
