package diagnostics

import (
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
	dbcIP   = "192.168.7.2"
	dbcAddr = dbcIP + ":22"
)

type Collector struct{}

func New() *Collector {
	return &Collector{}
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
	conn.Close()
	return true
}

func (c *Collector) collectMDB(dir string) {
	writeCommandOutput(dir, "journal.log", "journalctl", "--no-pager", "--since", "24 hours ago")
	writeCommandOutput(dir, "dmesg.log", "dmesg")
	c.writeMDBSystemInfo(dir)
}

func (c *Collector) collectDBC(dir string) {
	c.writeDBCCommand(dir, "journal.log", "journalctl --no-pager --since '24 hours ago'")
	c.writeDBCCommand(dir, "dmesg.log", "dmesg")
	c.writeDBCSystemInfo(dir)
}

func (c *Collector) runDBCCommand(command string) (string, error) {
	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", dbcIP),
		command)
	output, err := cmd.CombinedOutput()
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
	cmd := `printf '=== uptime ===\n'; uptime; printf '\n=== disk usage ===\n'; df -h; printf '\n=== memory ===\n'; free -m; printf '\n=== installed packages ===\n'; rpm -qa --last 2>/dev/null | head -50`
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
		{"installed packages", "rpm", []string{"-qa", "--last"}},
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

	if err := os.WriteFile(filepath.Join(dir, "system-info.txt"), []byte(content), 0644); err != nil {
		log.Printf("Failed to write system-info.txt: %v", err)
	}
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
